# M0 基础层（constant / common / logger / cachex / setting）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 M0 的最底层——持久化枚举、基础设施（JSON/数值安全/环境变量/Redis 封装）、结构化日志、混合缓存计数器，以及反射驱动的配置注册表与第一个配置域。

**Architecture:** 自下而上单向依赖：`setting/*_setting → setting/config → common/logger`，没有任何包反向依赖。配置系统用「结构体 + json tag + Register」把类型、默认值、持久化 key、文档合成一份，并用 `atomic.Pointer` 快照让热路径读无锁且不会读到中间态。金额一律整数记账，浮点转换走三档语义函数族。

**Tech Stack:** Go 1.23（本机 go1.23.2 windows/amd64）、标准库 `log/slog` + `testing`、`github.com/go-redis/redis/v8 v8.11.5`、`github.com/shopspring/decimal v1.5.0`。

**依据文档：** `docs/superpowers/specs/2026-10-12-m0-skeleton-design.md`（下称"设计文档"），
架构背景见 `docs/architecture/08-data-model-and-settings.md`、`06-billing-and-quota.md`。

**本计划的边界：** 只做基础层。结束时 `go build ./...` 与 `go test -race ./...` 全绿，
但**进程还起不来**（没有 `main.go`）——运行时与交付物在 `2026-10-12-m0-runtime.md`。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `go.mod` | 模块定义，`go 1.23.0`（toolchain go1.23.2） |
| `.gitignore` / `.env.example` | 本地数据与构建产物忽略；环境变量样例（含默认值说明） |
| `internal/constant/constants.go` | 持久化枚举。ABI：只增不改 |
| `internal/common/constants.go` | 版本号、默认值常量、缓存键前缀 |
| `internal/common/env.go` | `InitEnv()`：环境变量 → 启动期只读常量 |
| `internal/common/json.go` | JSON 编解码唯一入口（将来换实现只改这里） |
| `internal/common/quota_math.go` | 整数记账常量 + 数值安全函数族 + `QuotaClamp` |
| `internal/common/redis.go` | go-redis 封装；未配置/连不上时降级 |
| `internal/logger/logger.go` | `log/slog` 封装；级别可运行时更新 |
| `internal/cachex/counter.go` | `Counter`：Redis 与进程内自动切换的原子计数器 |
| `internal/setting/config/config.go` | 反射驱动的配置注册表 |
| `internal/setting/system_setting/system_setting.go` | 第一个配置域 + 原子快照 |

**任务顺序即依赖顺序**，每个任务结束时测试全绿、可独立提交。

---

## Task 1: 模块与仓库脚手架

**Files:**
- Create: `go.mod`（由 `go mod init` 生成）
- Create: `.gitignore`
- Create: `.env.example`

本任务不写 Go 代码，因此没有测试；验证方式是可执行的命令。**不引入任何依赖**——
依赖在首次被 import 的那个任务里用 `go get <pkg>@<version>` 引入并锁定版本，
避免"提前 `go mod tidy` 把还没用到的依赖清掉"。

- [ ] **Step 1: 确认当前分支状态**

Run: `git status -sb && git log --oneline -1`
Expected: 在 `feat/m0-skeleton` 分支上，HEAD 是设计文档的提交。

- [ ] **Step 2: 初始化 Go module**

Run: `go mod init github.com/yyyCode/whats-api`
Expected: 生成 `go.mod`，内容为 `module github.com/yyyCode/whats-api` 与 `go 1.23.0`。
如果它写的 go directive 是 `go 1.23.2`（本机 go 版本），手工改成 `go 1.23.0` 并补一行
`toolchain go1.23.2`，再继续——把语言版本与工具链版本分开写，别让工具链顶高 `go` 行。

- [ ] **Step 3: 写 `.gitignore`**

```
# 环境与本地数据
.env
*.db
*.db-journal
*.db-shm
*.db-wal

# 构建产物
bin/
dist/
whats-api
whats-api.exe
optctl
optctl.exe
whats-api-linux

# 编辑器 / 系统
.DS_Store
.idea/
.vscode/
*.swp

# 测试与覆盖率
coverage.out
```

- [ ] **Step 4: 写 `.env.example`**

每一个变量都必须有默认值——"不配任何环境变量也能启动"是 M0 第一条验收标准。

```
# whats-api 环境变量样例。全部变量都有默认值：不配任何变量也能启动（SQLite + 进程内缓存）。
# 复制为 .env 后按需修改。

# ---- HTTP ----
# HTTP 监听端口
PORT=3000
# gin 运行模式：debug / release
GIN_MODE=release
# 允许的跨域来源，逗号分隔；* 表示全部允许
CORS_ALLOW_ORIGINS=*

# ---- 数据库 ----
# 主库 DSN。留空 = SQLite 文件 whats-api.db；按前缀选驱动：
#   postgres:// postgresql://   → PostgreSQL
#   local / local:<path>        → SQLite（纯 Go 驱动，免 CGO）
#   其他                        → MySQL（自动补 parseTime=true）
SQL_DSN=
# 日志库 DSN。留空 = 复用主库。
# 仅日志库允许 clickhouse:// tcp:// http(s)://（尚未实现，会明确报错）
LOG_SQL_DSN=

# ---- 缓存 ----
# Redis 连接串，例如 redis://127.0.0.1:6379 或简写 127.0.0.1:6379。
# 留空 = 使用进程内缓存；配置了但连不上 = 告警并降级为进程内缓存
REDIS_CONN_STRING=

# ---- 节点与同步 ----
# master 执行数据库迁移；slave 跳过（多实例部署时只让一个节点迁移）
NODE_TYPE=master
# 实例标识，用于日志/审计区分来源
NODE_NAME=
# 配置定时同步间隔（秒），最小 5
SYNC_FREQUENCY=60

# ---- 生命周期与日志 ----
# 优雅停机超时（秒）。流式响应可能跑几分钟，不要调小
SHUTDOWN_TIMEOUT_SECONDS=120
# 启动期日志级别：debug / info / warn / error（可被库配置 system_setting.log_level 覆盖）
LOG_LEVEL=info

# ---- 尚未使用（后续里程碑）----
# 鉴权在 M1/M3 引入
# SESSION_SECRET=
```

- [ ] **Step 5: 验证**

Run: `go build ./...`
Expected: 退出码 0，stderr 只有一行 `go: warning: "./..." matched no packages`（此时模块里还没有包，这是正常的）。

**不要在这里跑 `go vet ./...`，也不要期望它退出码 0。** 本任务不写 Go 代码，
模块里没有包，`go vet` 会打印 `no packages to vet` 并**以退出码 1 结束**——
那不是"代码有问题"，而是"没有可检查的对象"。从 Task 2 起模块里有了包，
`go build` 与 `go vet` 才都成为有意义的验证（Task 10 做全量）。

Run: `git status --short`
Expected: 显示 `?? .env.example`、`?? .gitignore`、`?? go.mod`。

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore .env.example
git commit -m "chore: 初始化 Go module 与仓库脚手架

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 2: 持久化枚举（`internal/constant`）

**Files:**
- Create: `internal/constant/constants.go`
- Test: `internal/constant/constants_test.go`

这些数值会写进数据库，属于 ABI。测试的职责不是"验证实现"，而是**钉死数值**：
任何重编号都必须让测试失败。

- [ ] **Step 1: 写失败的测试**

创建 `internal/constant/constants_test.go`：

```go
package constant

import "testing"

// 这些数值是持久化到数据库的 ABI。改它们必须配一次数据迁移，
// 所以这里逐个钉住：重编号 = 测试失败 = 提醒你去写迁移。
func TestPersistenceEnumValues(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"RoleUser", RoleUser, 1},
		{"RoleAdmin", RoleAdmin, 10},
		{"RoleRoot", RoleRoot, 100},
		{"UserStatusEnabled", UserStatusEnabled, 1},
		{"UserStatusDisabled", UserStatusDisabled, 2},
		{"ChannelStatusEnabled", ChannelStatusEnabled, 1},
		{"ChannelStatusManuallyDisabled", ChannelStatusManuallyDisabled, 2},
		{"ChannelStatusAutoDisabled", ChannelStatusAutoDisabled, 3},
		{"TokenStatusEnabled", TokenStatusEnabled, 1},
		{"TokenStatusDisabled", TokenStatusDisabled, 2},
		{"TokenStatusExpired", TokenStatusExpired, 3},
		{"TokenStatusExhausted", TokenStatusExhausted, 4},
		{"LogTypeUnknown", LogTypeUnknown, 0},
		{"LogTypeConsume", LogTypeConsume, 1},
		{"LogTypeTopup", LogTypeTopup, 2},
		{"LogTypeManage", LogTypeManage, 3},
		{"LogTypeSystem", LogTypeSystem, 4},
		{"LogTypeError", LogTypeError, 5},
		{"LogTypeRefund", LogTypeRefund, 6},
		{"LogTypeLogin", LogTypeLogin, 7},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d，但已发布的 ABI 值是 %d；"+
				"改变持久化枚举必须配一次数据迁移，不能只改常量", c.name, c.got, c.want)
		}
	}
}

// 同一枚举内撞值是最容易犯的重排错误：两个不同的语义落库后无法区分。
func TestEnumsHaveNoDuplicateValues(t *testing.T) {
	groups := map[string]map[string]int{
		"UserStatus": {
			"Enabled":  UserStatusEnabled,
			"Disabled": UserStatusDisabled,
		},
		"ChannelStatus": {
			"Enabled":          ChannelStatusEnabled,
			"ManuallyDisabled": ChannelStatusManuallyDisabled,
			"AutoDisabled":     ChannelStatusAutoDisabled,
		},
		"TokenStatus": {
			"Enabled":   TokenStatusEnabled,
			"Disabled":  TokenStatusDisabled,
			"Expired":   TokenStatusExpired,
			"Exhausted": TokenStatusExhausted,
		},
		"LogType": {
			"Unknown": LogTypeUnknown,
			"Consume": LogTypeConsume,
			"Topup":   LogTypeTopup,
			"Manage":  LogTypeManage,
			"System":  LogTypeSystem,
			"Error":   LogTypeError,
			"Refund":  LogTypeRefund,
			"Login":   LogTypeLogin,
		},
	}
	for group, members := range groups {
		seen := map[int]string{}
		for name, v := range members {
			if dup, ok := seen[v]; ok {
				t.Errorf("%s.%s 与 %s 撞值 %d", group, name, dup, v)
			}
			seen[v] = name
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/constant/`
Expected: FAIL — `undefined: RoleUser` 等一长串编译错误。

- [ ] **Step 3: 写实现**

创建 `internal/constant/constants.go`：

```go
// Package constant 存放**持久化枚举**。
//
// 这些数值一旦写进数据库就是 ABI：只能追加，不能改变已有值，也不能重排。
// 改动它们必须同时改 constants_test.go —— 那一步失败正是在提醒你：
// 线上库里已经存着旧值，需要一次数据迁移。
package constant

// 用户角色。数值越大权限越高，比较用 >= 而不是 ==。
const (
	RoleUser  = 1
	RoleAdmin = 10
	RoleRoot  = 100
)

// 用户状态。
const (
	UserStatusEnabled  = 1
	UserStatusDisabled = 2
)

// 渠道状态。
//
// ManuallyDisabled 与 AutoDisabled 从第一天就分开：后续的自动恢复逻辑
// 只允许恢复 AutoDisabled，绝不能把运维手动禁用的渠道恢复回来。
// 如果这时把两者合并成一个值，将来想做这条区分就晚了（库里存量数据无法回溯）。
const (
	ChannelStatusEnabled          = 1
	ChannelStatusManuallyDisabled = 2
	ChannelStatusAutoDisabled     = 3
)

// 令牌状态。
const (
	TokenStatusEnabled   = 1
	TokenStatusDisabled  = 2
	TokenStatusExpired   = 3
	TokenStatusExhausted = 4
)

// 日志类型。logs.type 是账本的一列，用来区分"这笔记录是什么事"。
const (
	LogTypeUnknown = 0
	LogTypeConsume = 1
	LogTypeTopup   = 2
	LogTypeManage  = 3
	LogTypeSystem  = 4
	LogTypeError   = 5
	LogTypeRefund  = 6
	LogTypeLogin   = 7
)
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/constant/ -v`
Expected: PASS，两个测试都通过。

- [ ] **Step 5: Commit**

```bash
git add internal/constant/
git commit -m "feat(constant): 定义持久化枚举并用测试钉住数值

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 3: 启动期常量与 `InitEnv`（`internal/common/env.go`）

**Files:**
- Create: `internal/common/constants.go`
- Create: `internal/common/env.go`
- Test: `internal/common/env_test.go`

**这里有一条贯穿全项目的纪律要说清楚**：`common` 里的这些变量是**启动期常量**——
`InitEnv()` 写一次，之后只读，因此并发安全。需要热更新的东西绝不能放这里，
它们属于 `setting/` 的 Option 系统。把两者混起来是配置系统失控的起点。

- [ ] **Step 1: 写失败的测试**

创建 `internal/common/env_test.go`：

```go
package common

import (
	"strconv"
	"strings"
	"testing"
)

// 清空所有本包读取的环境变量——"不配任何环境变量"是第一条验收标准的前提。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PORT", "GIN_MODE", "CORS_ALLOW_ORIGINS", "SQL_DSN", "LOG_SQL_DSN",
		"REDIS_CONN_STRING", "NODE_TYPE", "NODE_NAME", "SYNC_FREQUENCY",
		"SHUTDOWN_TIMEOUT_SECONDS", "LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func TestInitEnvDefaultsWithNoEnv(t *testing.T) {
	clearEnv(t)
	InitEnv()

	if Port != DefaultPort {
		t.Errorf("Port = %d，期望默认值 %d", Port, DefaultPort)
	}
	if GinMode != "release" {
		t.Errorf("GinMode = %q，期望 release", GinMode)
	}
	if SQLDSN != "" {
		t.Errorf("SQLDSN = %q，期望空（空 = SQLite 默认文件）", SQLDSN)
	}
	if !IsMasterNode || NodeType != DefaultNodeType {
		t.Errorf("默认应为 master 节点，得到 NodeType=%q IsMasterNode=%v", NodeType, IsMasterNode)
	}
	if SyncFrequency != DefaultSyncFrequencySeconds {
		t.Errorf("SyncFrequency = %d，期望 %d", SyncFrequency, DefaultSyncFrequencySeconds)
	}
	if ShutdownTimeout != DefaultShutdownTimeoutSeconds {
		t.Errorf("ShutdownTimeout = %d，期望 %d", ShutdownTimeout, DefaultShutdownTimeoutSeconds)
	}
	if LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q，期望 %q", LogLevel, DefaultLogLevel)
	}
	if len(CORSAllowOrigins) != 1 || CORSAllowOrigins[0] != "*" {
		t.Errorf("CORSAllowOrigins = %v，期望 [*]", CORSAllowOrigins)
	}
	if len(EnvWarnings) != 0 {
		t.Errorf("默认配置不应产生告警，得到 %v", EnvWarnings)
	}
}

func TestInitEnvReadsOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "8080")
	t.Setenv("SQL_DSN", "local:/tmp/whats.db")
	t.Setenv("LOG_SQL_DSN", "postgres://localhost/logs")
	t.Setenv("REDIS_CONN_STRING", "127.0.0.1:6379")
	t.Setenv("NODE_TYPE", "slave")
	t.Setenv("NODE_NAME", "node-b")
	t.Setenv("SYNC_FREQUENCY", "15")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "30")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("CORS_ALLOW_ORIGINS", "https://a.example.com, https://b.example.com ")
	InitEnv()

	if Port != 8080 {
		t.Errorf("Port = %d，期望 8080", Port)
	}
	if SQLDSN != "local:/tmp/whats.db" {
		t.Errorf("SQLDSN = %q", SQLDSN)
	}
	if LogSQLDSN != "postgres://localhost/logs" {
		t.Errorf("LogSQLDSN = %q", LogSQLDSN)
	}
	if RedisConnString != "127.0.0.1:6379" {
		t.Errorf("RedisConnString = %q", RedisConnString)
	}
	if IsMasterNode {
		t.Error("NODE_TYPE=slave 时 IsMasterNode 必须为 false")
	}
	if NodeName != "node-b" {
		t.Errorf("NodeName = %q", NodeName)
	}
	if SyncFrequency != 15 || ShutdownTimeout != 30 || LogLevel != "debug" {
		t.Errorf("秒级配置读取错误：sync=%d shutdown=%d level=%q", SyncFrequency, ShutdownTimeout, LogLevel)
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(CORSAllowOrigins) != len(want) {
		t.Fatalf("CORSAllowOrigins = %v，期望 %v", CORSAllowOrigins, want)
	}
	for i := range want {
		if CORSAllowOrigins[i] != want[i] {
			t.Errorf("CORSAllowOrigins[%d] = %q，期望 %q（应去掉空格）", i, CORSAllowOrigins[i], want[i])
		}
	}
}

// 同步频率太低会把业务库当轮询靶子，所以低于下限要抬回来并告警。
func TestInitEnvClampsSyncFrequencyAndWarns(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYNC_FREQUENCY", "1")
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds {
		t.Errorf("SyncFrequency = %d，期望被抬到下限 %d", SyncFrequency, MinSyncFrequencySeconds)
	}
	if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "SYNC_FREQUENCY") {
		t.Errorf("应产生一条 SYNC_FREQUENCY 告警，得到 %v", EnvWarnings)
	}
}

func TestInitEnvDoesNotWarnAtExactMinimum(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYNC_FREQUENCY", "5")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "5")
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds || len(EnvWarnings) != 0 {
		t.Errorf("恰好等于下限不应告警：sync=%d warnings=%v", SyncFrequency, EnvWarnings)
	}
}

// 优雅退出的超时会被当成 deadline 用，0 或负数等于"立刻取消"，
// 等于把优雅退出变成强杀；太大则让容器停不下来。下限必须被抬回来。
func TestInitEnvClampsShutdownTimeout(t *testing.T) {
	for _, in := range []string{"0", "-1"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", in)
			InitEnv()

			if ShutdownTimeout != MinShutdownTimeoutSeconds {
				t.Errorf("SHUTDOWN_TIMEOUT_SECONDS=%s 应被抬到下限 %d，得到 %d",
					in, MinShutdownTimeoutSeconds, ShutdownTimeout)
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "SHUTDOWN_TIMEOUT_SECONDS") {
				t.Errorf("应产生一条 SHUTDOWN_TIMEOUT_SECONDS 告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 端口超出 1-65535 是配置错误：回退默认值并告警，而不是把 0 端口交给监听。
func TestInitEnvRejectsOutOfRangePort(t *testing.T) {
	for _, in := range []string{"0", "-1", "65536", "99999"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PORT", in)
			InitEnv()

			if Port != DefaultPort {
				t.Errorf("PORT=%s 应回退 %d，得到 %d", in, DefaultPort, Port)
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "PORT") {
				t.Errorf("应产生一条 PORT 告警，得到 %v", EnvWarnings)
			}
		})
	}

	// 边界值合法，不该被误伤。
	for _, in := range []string{"1", "65535"} {
		t.Run("边界 "+in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PORT", in)
			InitEnv()

			if port, _ := strconv.Atoi(in); Port != port {
				t.Errorf("PORT=%s 是合法端口，不该被改，得到 %d", in, Port)
			}
			if len(EnvWarnings) != 0 {
				t.Errorf("合法端口不该告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 大小写不该决定主从：NODE_TYPE=Slave 若按原样比较，从节点会被判成主节点，
// 从而去跑迁移——这是"多节点同时建表"最直接的来源。
func TestInitEnvNodeTypeIsCaseInsensitive(t *testing.T) {
	cases := []struct {
		in         string
		wantMaster bool
	}{
		{"master", true},
		{"MASTER", true},
		{"Master", true},
		{"slave", false},
		{"SLAVE", false},
		{"Slave", false},
		{"  slave  ", false}, // envStr 会去空白
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NODE_TYPE", c.in)
			InitEnv()

			if IsMasterNode != c.wantMaster {
				t.Errorf("NODE_TYPE=%q 时 IsMasterNode = %v，期望 %v", c.in, IsMasterNode, c.wantMaster)
			}
			if len(EnvWarnings) != 0 {
				t.Errorf("大小写差异不该告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 认不出来的取值按默认处理，但必须告警——否则运维写错一个词，
// 主从角色就静默变了，而日志里什么都没有。
func TestInitEnvWarnsOnUnknownNodeType(t *testing.T) {
	clearEnv(t)
	t.Setenv("NODE_TYPE", "primary")
	InitEnv()

	if NodeType != DefaultNodeType || !IsMasterNode {
		t.Errorf("NODE_TYPE=primary 应按默认 %q 处理，得到 %q", DefaultNodeType, NodeType)
	}
	if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "NODE_TYPE") {
		t.Errorf("应产生一条 NODE_TYPE 告警，得到 %v", EnvWarnings)
	}
}

// CORS_ALLOW_ORIGINS 配了但一个有效条目都没有（如 ","）：必须按"谁都不允许"
// 处理并告警，绝不能回落到默认值 "*"——那是允许所有来源，
// 会把一次本想收紧的配置变成完全放开。
func TestInitEnvTreatsEmptyCORSListAsDenyAll(t *testing.T) {
	for _, in := range []string{",", " , ", ",,"} {
		t.Run(in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("CORS_ALLOW_ORIGINS", in)
			InitEnv()

			if len(CORSAllowOrigins) != 0 {
				t.Errorf("CORS_ALLOW_ORIGINS=%q 应得到空列表，得到 %v", in, CORSAllowOrigins)
			}
			for _, o := range CORSAllowOrigins {
				if o == "*" {
					t.Fatalf("CORS_ALLOW_ORIGINS=%q 回落到通配符 %q，等于完全放开", in, o)
				}
			}
			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], "CORS_ALLOW_ORIGINS") {
				t.Errorf("应产生一条 CORS_ALLOW_ORIGINS 告警，得到 %v", EnvWarnings)
			}
		})
	}
}

// 非法数值不致命：回退默认值继续启动，而不是启动不了。
// 但回退必须留痕——否则 PORT=abc 会安安静静跑在默认端口上，
// 而运维以为服务听在他们配的端口上。
func TestInitEnvFallsBackOnInvalidNumbers(t *testing.T) {
	cases := []struct {
		key string
		val string
	}{
		{"PORT", "not-a-port"},
		{"PORT", "3000.5"},
		{"SHUTDOWN_TIMEOUT_SECONDS", "abc"},
		{"SYNC_FREQUENCY", "60秒"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(c.key, c.val)
			InitEnv()

			if len(EnvWarnings) != 1 || !strings.Contains(EnvWarnings[0], c.key) {
				t.Errorf("%s=%q 回退默认值时应留下一条告警，得到 %v", c.key, c.val, EnvWarnings)
			}
		})
	}

	clearEnv(t)
	t.Setenv("PORT", "not-a-port")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "abc")
	InitEnv()
	if Port != DefaultPort {
		t.Errorf("非法 PORT 应回退 %d，得到 %d", DefaultPort, Port)
	}
	if ShutdownTimeout != DefaultShutdownTimeoutSeconds {
		t.Errorf("非法超时应回退 %d，得到 %d", DefaultShutdownTimeoutSeconds, ShutdownTimeout)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/common/`
Expected: FAIL — `undefined: InitEnv` 等编译错误。

- [ ] **Step 3: 写 `constants.go`**

创建 `internal/common/constants.go`：

```go
// Package common 是基础设施层：启动期常量、JSON 编解码、数值安全、Redis 封装。
//
// 它只依赖 internal/logger（一个只依赖标准库的横切叶子包）。
// 持久化枚举在 internal/constant，那是给 model 层用的，common 不引用它——
// 这两处常量服务的目的不同：这里的默认值是"没配环境变量时用什么"，
// constant 里的是"已经写进库里的值不能变"。
package common

// Version 出现在启动日志、/api/status 与响应头 X-Whats-Api-Version。
const Version = "v0.1.0-m0"

// CacheNamespacePrefix 是缓存键前缀，避免与同一个 Redis 实例上的其他系统撞键。
const CacheNamespacePrefix = "whats-api"

// 默认值集中在这里：环境变量只做覆盖，代码里不再散落字面量。
//
// 命名注意：秒级默认值统一带 Seconds 后缀（测试与其他任务都按这个名字引用）。
const (
	DefaultPort                   = 3000
	DefaultSyncFrequencySeconds   = 60
	DefaultShutdownTimeoutSeconds = 120
	DefaultSQLiteFile             = "whats-api.db"
	DefaultLogLevel               = "info"
	DefaultCORSAllowOrigins       = "*"
	DefaultNodeType               = "master"
)

// MinSyncFrequencySeconds 是同步间隔的下限：再低就是在把业务库当轮询靶子。
const MinSyncFrequencySeconds = 5

// MinShutdownTimeoutSeconds 是优雅退出超时的下限。
//
// 它必须大于 0：超时会变成 context.WithTimeout 的 deadline，0 或负数
// 意味着"立刻取消"——正在处理的请求会被直接掐断，优雅退出就成了强杀。
const MinShutdownTimeoutSeconds = 5
```

- [ ] **Step 4: 写 `env.go`**

创建 `internal/common/env.go`：

```go
package common

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 启动期常量：InitEnv 写一次、之后只读，因此并发安全。
//
// 与 Options（可热更新的运行期配置）严格区分：需要热更新的东西绝不能放这里。
var (
	Port             int
	GinMode          string
	CORSAllowOrigins []string

	SQLDSN          string
	LogSQLDSN       string
	RedisConnString string

	NodeType     string
	NodeName     string
	IsMasterNode bool

	SyncFrequency   int // 秒
	ShutdownTimeout int // 秒
	LogLevel        string

	// EnvWarnings 收集初始化期的可告警项。InitEnv 跑在 logger 之前，
	// 所以这里先攒着，等 logger 起来后由启动流程统一输出。
	EnvWarnings []string
)

// InitEnv 从环境变量装载启动期常量，必须在任何模块读配置之前调用。
//
// 所有变量都有默认值：不配任何环境变量也能启动。
func InitEnv() {
	EnvWarnings = nil

	var warn string

	Port, warn = envInt("PORT", DefaultPort)
	appendEnvWarning(warn)
	if Port < 1 || Port > 65535 {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"PORT=%d 不是合法端口（1-65535），已回退默认值 %d", Port, DefaultPort))
		Port = DefaultPort
	}

	GinMode = envStr("GIN_MODE", "release")
	CORSAllowOrigins, warn = envList("CORS_ALLOW_ORIGINS", DefaultCORSAllowOrigins)
	appendEnvWarning(warn)

	SQLDSN = envRaw("SQL_DSN")
	LogSQLDSN = envRaw("LOG_SQL_DSN")
	RedisConnString = envRaw("REDIS_CONN_STRING")

	// 先小写再比较：运维写成 NODE_TYPE=Slave 时，按原样比较会把从节点判成主节点，
	// 从节点于是去跑迁移——这是"多节点同时建表"最直接的来源。
	NodeType = strings.ToLower(envStr("NODE_TYPE", DefaultNodeType))
	switch NodeType {
	case "master", "slave":
	default:
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"NODE_TYPE=%q 无法识别（只认 master/slave），按 %s 处理", NodeType, DefaultNodeType))
		NodeType = DefaultNodeType
	}
	NodeName = envRaw("NODE_NAME")
	IsMasterNode = NodeType != "slave"

	SyncFrequency, warn = envInt("SYNC_FREQUENCY", DefaultSyncFrequencySeconds)
	appendEnvWarning(warn)
	if SyncFrequency < MinSyncFrequencySeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SYNC_FREQUENCY=%d 低于下限 %d 秒，已抬到下限", SyncFrequency, MinSyncFrequencySeconds))
		SyncFrequency = MinSyncFrequencySeconds
	}

	ShutdownTimeout, warn = envInt("SHUTDOWN_TIMEOUT_SECONDS", DefaultShutdownTimeoutSeconds)
	appendEnvWarning(warn)
	if ShutdownTimeout < MinShutdownTimeoutSeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SHUTDOWN_TIMEOUT_SECONDS=%d 低于下限 %d 秒，已抬到下限",
			ShutdownTimeout, MinShutdownTimeoutSeconds))
		ShutdownTimeout = MinShutdownTimeoutSeconds
	}
	LogLevel = envStr("LOG_LEVEL", DefaultLogLevel)
}

// envRaw 返回原样（仅去空白）的变量值，空字符串按"未配置"处理。
func envRaw(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func envStr(key, def string) string {
	if v := envRaw(key); v != "" {
		return v
	}
	return def
}

// envInt 读取整数。非法值不致命：回退默认值继续启动——
// 一个写错的端口不该让服务完全起不来，但也**不能静默**：
// 回退必须留痕，否则 PORT=abc 会安安静静地跑在 3000 上，
// 而运维以为服务听在他们配的端口上。第二个返回值是告警文本。
func envInt(key string, def int) (value int, warn string) {
	v := envRaw(key)
	if v == "" {
		return def, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Sprintf("%s=%q 不是整数，已回退默认值 %d", key, v, def)
	}
	return n, ""
}

// appendEnvWarning 收集一条告警，空串表示无告警。
func appendEnvWarning(w string) {
	if w != "" {
		EnvWarnings = append(EnvWarnings, w)
	}
}

// envList 读取逗号分隔的列表，逐项去空白，空项丢弃。
//
// 它把"没配"和"配了但一个有效条目都没有"分开处理，这是安全语义上的区别：
//
//	没配（键不存在或为空） → 用默认值
//	配了但全是空项（如 ","） → 空列表，并告警
//
// 后一种情况若回落到默认值，默认值里的 "*" 是"允许所有来源"，
// 于是一次本想收紧的配置反而把 CORS 完全打开了。空列表是"谁都不允许"，
// 属于失败时收紧（fail-closed）。
//
// 第二个返回值是给 InitEnv 收集的告警文本，空串表示无告警。
func envList(key, def string) (values []string, warn string) {
	raw := envRaw(key)
	if raw == "" {
		return strings.Split(def, ","), ""
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{}, fmt.Sprintf(
			"%s 已配置但没有有效条目，按空列表处理（不信任任何跨域来源）；"+
				"要允许所有来源请显式写 *", key)
	}
	return out, ""
}
```

import 块里已含 `fmt`（`EnvWarnings` 的那几条 `Sprintf` 都用它）：

```go
import (
	"fmt"
	"os"
	"strconv"
	"strings"
)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/common/ -v`
Expected: PASS，全部测试通过（默认值、覆盖、钳制告警、非法回退等各一组）。

- [ ] **Step 6: Commit**

```bash
git add internal/common/
git commit -m "feat(common): 启动期常量与 InitEnv（全默认值可启动）

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 4: JSON 编解码唯一入口（`internal/common/json.go`）

**Files:**
- Create: `internal/common/json.go`
- Test: `internal/common/json_test.go`

全项目只经过这几个函数进出 JSON。将来要换成 `sonic` 之类的实现时只改这一个文件——
这是这条纪律存在的全部理由。

- [ ] **Step 1: 写失败的测试**

创建 `internal/common/json_test.go`：

```go
package common

import "testing"

type jsonSample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	in := jsonSample{Name: "渠道", Count: 3}
	raw, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}

	var out jsonSample
	if err := Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if out != in {
		t.Errorf("往返后不一致：%+v vs %+v", out, in)
	}
}

// 库里有大量 JSON 存在 TEXT 字段里，读的时候是字符串。
func TestUnmarshalString(t *testing.T) {
	var out jsonSample
	if err := UnmarshalString(`{"name":"a","count":1}`, &out); err != nil {
		t.Fatalf("UnmarshalString 失败: %v", err)
	}
	if out.Name != "a" || out.Count != 1 {
		t.Errorf("解析结果错误：%+v", out)
	}
}

func TestUnmarshalInvalidReturnsError(t *testing.T) {
	var out jsonSample
	if err := UnmarshalString(`{"name":`, &out); err == nil {
		t.Fatal("非法 JSON 应该返回错误")
	}
	if err := Unmarshal([]byte("not json"), &out); err == nil {
		t.Fatal("非法 JSON 应该返回错误")
	}
}

func TestMarshalIndent(t *testing.T) {
	raw, err := MarshalIndent(jsonSample{Name: "a", Count: 1})
	if err != nil {
		t.Fatalf("MarshalIndent 失败: %v", err)
	}
	want := "{\n  \"name\": \"a\",\n  \"count\": 1\n}"
	if string(raw) != want {
		t.Errorf("MarshalIndent = %q，期望 %q", raw, want)
	}
}

// 序列化失败不能让进程崩：日志与配置序列化不该把服务带走。
func TestMustMarshalStringReturnsEmptyOnFailure(t *testing.T) {
	if got := MustMarshalString(make(chan int)); got != "" {
		t.Errorf("不可序列化的值应返回空字符串，得到 %q", got)
	}
	if got := MustMarshalString(jsonSample{Name: "a", Count: 1}); got != `{"name":"a","count":1}` {
		t.Errorf("正常序列化结果错误: %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/common/ -run 'Marshal|Unmarshal'`
Expected: FAIL — `undefined: Marshal`。

- [ ] **Step 3: 写实现**

创建 `internal/common/json.go`：

```go
package common

import "encoding/json"

// JSON 编解码的统一入口。
//
// 不要直接用 encoding/json：全项目只经过这几个函数进出，
// 将来换成 sonic 之类的实现时才只需要改这一个文件。

func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func MarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// UnmarshalString 是 Unmarshal 的字符串版本：库里大量 JSON 存在 TEXT 字段里。
func UnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// MustMarshalString 用于"几乎不会失败"的场景（基础类型构成的 map/结构体）。
// 失败时返回空字符串而不是 panic：日志与配置序列化不该把进程带走。
func MustMarshalString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/common/ -v`
Expected: PASS（Task 3 的测试也应仍然通过）。

- [ ] **Step 5: Commit**

```bash
git add internal/common/json.go internal/common/json_test.go
git commit -m "feat(common): JSON 编解码唯一入口

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 5: 数值安全（`internal/common/quota_math.go`）

**Files:**
- Create: `internal/common/quota_math.go`
- Test: `internal/common/quota_math_test.go`

**这是本计划里最需要认真读的一节。** 计费闭环虽然后面才做，但整数记账的语义（尤其是
"溢出/NaN 时到底取什么值"）一旦写进账本就极难更改，所以现在就把三档语义定死并测试钉住。

先引入依赖：

Run: `go get github.com/shopspring/decimal@v1.5.0`
Expected: `go.mod` 增加 `github.com/shopspring/decimal v1.5.0`。

- [ ] **Step 1: 写失败的测试**

创建 `internal/common/quota_math_test.go`：

```go
package common

import (
	"errors"
	"math"
	"testing"
)

// Checked 档：饱和并留痕（结算、日志用）。
func TestQuotaFromFloatCheckedSaturates(t *testing.T) {
	cases := []struct {
		name  string
		in    float64
		want  int
		clamp QuotaClamp
	}{
		{"正常值", 123, 123, ClampNone},
		{"零", 0, 0, ClampNone},
		{"恰好上限", float64(MaxQuota), MaxQuota, ClampNone},
		{"超过上限", float64(MaxQuota) + 1, MaxQuota, ClampOverflow},
		{"正无穷", math.Inf(1), MaxQuota, ClampOverflow},
		{"NaN", math.NaN(), 0, ClampNaN},
		{"负数", -1, 0, ClampUnderflow},
		{"负无穷", math.Inf(-1), 0, ClampUnderflow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clamp := QuotaFromFloatChecked(c.in)
			if got != c.want || clamp != c.clamp {
				t.Errorf("QuotaFromFloatChecked(%v) = (%d, %v)，期望 (%d, %v)",
					c.in, got, clamp, c.want, c.clamp)
			}
		})
	}
}

// NaN 必须饱和到 0 而不是上限：结算时饱和到上限会给出荒谬的巨额扣费。
// 但 0 是危险的，所以必须带 ClampNaN 让调用方显式处理（退款/告警）。
func TestNaNDoesNotSaturateToMaxQuota(t *testing.T) {
	got, clamp := QuotaFromFloatChecked(math.NaN())
	if got == MaxQuota {
		t.Fatal("NaN 饱和到上限会在结算时造成巨额误扣")
	}
	if !clamp.Saturated() {
		t.Fatal("NaN 必须留下饱和标记，否则调用方会静默按 0 计费")
	}
}

// 基础档静默饱和：调用方无从得知，所以只能用于展示类计算。
func TestQuotaFromFloatIsSilent(t *testing.T) {
	if got := QuotaFromFloat(math.NaN()); got != 0 {
		t.Errorf("QuotaFromFloat(NaN) = %d，期望 0", got)
	}
	if got := QuotaFromFloat(math.Inf(1)); got != MaxQuota {
		t.Errorf("QuotaFromFloat(+Inf) = %d，期望 %d", got, MaxQuota)
	}
	if got := QuotaFromFloat(1.9); got != 1 {
		t.Errorf("QuotaFromFloat 不取整，1.9 应截断为 1，得到 %d", got)
	}
}

// Strict 档：溢出即错（预扣用）。宁可拒绝请求，也不能按错误金额冻结。
func TestQuotaFromFloatStrictRejectsSaturation(t *testing.T) {
	cases := []struct {
		name    string
		in      float64
		wantErr error
	}{
		{"超过上限", math.Inf(1), ErrQuotaOverflow},
		{"NaN", math.NaN(), ErrQuotaNaN},
		{"负数", -0.5, ErrQuotaNegative},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := QuotaFromFloatStrict(c.in)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("期望错误 %v，得到 %v", c.wantErr, err)
			}
			if v != 0 {
				t.Errorf("出错时必须返回 0，得到 %d", v)
			}
		})
	}

	v, err := QuotaFromFloatStrict(42)
	if err != nil || v != 42 {
		t.Fatalf("正常值不该报错：v=%d err=%v", v, err)
	}
}

// 分层计费必须用 QuotaRound：否则预扣与结算会差 ±1 quota，日志里天天有噪声差异。
func TestQuotaRoundIsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0.4, 0},
		{0.5, 1},
		{1.5, 2},
		{2.5, 3},
		{100.49, 100},
		{100.5, 101},
	}
	for _, c := range cases {
		if got := QuotaRound(c.in); got != c.want {
			t.Errorf("QuotaRound(%v) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 负数取整后落到下溢分支，返回 0 并带标记——而不是得到负数金额。
func TestQuotaRoundCheckedNegativeUnderflows(t *testing.T) {
	got, clamp := QuotaRoundChecked(-0.5)
	if got != 0 || clamp != ClampUnderflow {
		t.Errorf("QuotaRoundChecked(-0.5) = (%d, %v)，期望 (0, %v)", got, clamp, ClampUnderflow)
	}

	if v, err := QuotaRoundStrict(-0.5); !errors.Is(err, ErrQuotaNegative) || v != 0 {
		t.Errorf("QuotaRoundStrict(-0.5) = (%d, %v)，期望 (0, ErrQuotaNegative)", v, err)
	}
}

// -0.4 取整后是 -0，而 -0 < 0 为假：负数必须在取整前认出，
// 否则 Round 档与 Float 档对同一个负数会给出相反结论。
func TestQuotaRoundAgreesWithFloatFamilyOnNegativeFractions(t *testing.T) {
	for _, f := range []float64{-0.4, -0.4999, -0.0001} {
		wantV, wantClamp := QuotaFromFloatChecked(f)
		gotV, gotClamp := QuotaRoundChecked(f)
		if gotV != wantV || gotClamp != wantClamp {
			t.Errorf("QuotaRoundChecked(%v) = (%d, %v)，而 QuotaFromFloatChecked(%v) = (%d, %v)",
				f, gotV, gotClamp, f, wantV, wantClamp)
		}
		if _, err := QuotaRoundStrict(f); !errors.Is(err, ErrQuotaNegative) {
			t.Errorf("QuotaRoundStrict(%v) 应报 ErrQuotaNegative，得到 %v", f, err)
		}
	}

	// 精确的 0 与 -0 都不是负数，不该被当成下溢。
	for _, f := range []float64{0, math.Copysign(0, -1)} {
		if v, clamp := QuotaRoundChecked(f); v != 0 || clamp != ClampNone {
			t.Errorf("QuotaRoundChecked(%v) = (%d, %v)，期望 (0, ClampNone)", f, v, clamp)
		}
	}
}

// decimal 档与浮点档对负数的判定也必须一致。
func TestDecimalAgreesWithFloatFamilyOnNegativeFractions(t *testing.T) {
	for _, s := range []string{"-0.4", "-0.0001"} {
		if got, clamp := QuotaFromDecimalChecked(s); got != 0 || clamp != ClampUnderflow {
			t.Errorf("QuotaFromDecimalChecked(%q) = (%d, %v)，期望 (0, ClampUnderflow)", s, got, clamp)
		}
	}
	if got, clamp := QuotaFromDecimalChecked("-0"); got != 0 || clamp != ClampNone {
		t.Errorf("QuotaFromDecimalChecked(\"-0\") = (%d, %v)，期望 (0, ClampNone)", got, clamp)
	}
}

func TestQuotaFromDecimal(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  int
		clamp QuotaClamp
	}{
		{"整数", "1000", 1000, ClampNone},
		{"四舍五入进位", "10.5", 11, ClampNone},
		{"四舍五入舍去", "10.4", 10, ClampNone},
		{"零", "0", 0, ClampNone},
		{"负数", "-1", 0, ClampUnderflow},
		{"超过上限", "99999999999999999999", MaxQuota, ClampOverflow},
		{"负数（取整后为 0）", "-0.4", 0, ClampUnderflow},
		{"负零", "-0", 0, ClampNone},
		{"非数字", "abc", 0, ClampNaN},
		{"空串", "", 0, ClampNaN},
		{"前后空白", "  12.6  ", 13, ClampNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, clamp := QuotaFromDecimalChecked(c.in)
			if got != c.want || clamp != c.clamp {
				t.Errorf("QuotaFromDecimalChecked(%q) = (%d, %v)，期望 (%d, %v)",
					c.in, got, clamp, c.want, c.clamp)
			}
		})
	}
}

// 超大数字必须走 decimal 判定：转成 float64 会丢精度，越界判断就不可靠了。
func TestQuotaFromDecimalStrictRejectsBadInput(t *testing.T) {
	v, err := QuotaFromDecimalStrict("99999999999999999999")
	if !errors.Is(err, ErrQuotaOverflow) || v != 0 {
		t.Errorf("超上限应报 ErrQuotaOverflow，得到 (%d, %v)", v, err)
	}
	if _, err := QuotaFromDecimalStrict("abc"); err == nil {
		t.Error("非数字应报错")
	}
	if _, err := QuotaFromDecimalStrict("-1"); !errors.Is(err, ErrQuotaNegative) {
		t.Errorf("负数应报 ErrQuotaNegative，得到 %v", err)
	}
	if v, err := QuotaFromDecimalStrict("12.5"); err != nil || v != 13 {
		t.Errorf("QuotaFromDecimalStrict(12.5) = (%d, %v)，期望 (13, nil)", v, err)
	}
}

func TestQuotaClampAdminMap(t *testing.T) {
	m := ClampOverflow.AdminMap()
	if m["quota_saturation"] != "overflow" || m["saturated"] != true {
		t.Errorf("ClampOverflow.AdminMap() = %v", m)
	}
	if ClampNone.AdminMap()["saturated"] != false {
		t.Error("ClampNone 不应标记为饱和")
	}
	if ClampNone.String() != "none" || ClampOverflow.String() != "overflow" ||
		ClampUnderflow.String() != "underflow" || ClampNaN.String() != "nan" {
		t.Error("QuotaClamp.String() 的字面量是日志字段的一部分，不能随意改")
	}
	if ClampNone.Saturated() {
		t.Error("ClampNone.Saturated() 应为 false")
	}
}

// 钱包上限取 JS 安全整数上限，否则前端展示会丢精度。
func TestWalletQuotaFitsJSSafeInteger(t *testing.T) {
	if MaxWalletQuota != 9007199254740991 {
		t.Errorf("MaxWalletQuota = %d，期望 9007199254740991 (1<<53-1)", MaxWalletQuota)
	}
	if QuotaPerUnit != 500000 {
		t.Errorf("QuotaPerUnit = %d，期望 500000（50 万 quota = 1 美元）", QuotaPerUnit)
	}
	if MaxQuota != math.MaxInt32 {
		t.Errorf("MaxQuota = %d，期望 math.MaxInt32", MaxQuota)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/common/ -run 'Quota|Clamp|Wallet'`
Expected: FAIL — `undefined: QuotaFromFloatChecked` 等。

- [ ] **Step 3: 写实现**

创建 `internal/common/quota_math.go`：

```go
package common

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

// 记账单位：内部货币是整数 quota，永远不用 float 记账（避免累加误差）。
const (
	// QuotaPerUnit 是 1 美元对应的 quota：50 万 quota = 1 美元。
	QuotaPerUnit = 500 * 1000
	// MaxQuota 是单次请求的金额上限，用于防溢出。
	MaxQuota = math.MaxInt32
	// MaxWalletQuota 是钱包/余额上限，取 JS 安全整数上限，保证前端不丢精度。
	MaxWalletQuota = 1<<53 - 1
	// USD2RMB 仅用于展示换算，不参与记账。
	USD2RMB = 7.3
)

// QuotaClamp 记录一次浮点→整数转换中发生的饱和。
//
// 数值异常必须留痕：调用方拿到非 ClampNone 的返回值时必须写进日志
// （AdminMap 提供了现成字段），不允许静默丢弃。
type QuotaClamp uint8

const (
	ClampNone QuotaClamp = iota
	ClampOverflow
	ClampUnderflow
	ClampNaN
)

func (c QuotaClamp) Saturated() bool { return c != ClampNone }

// String 的字面量是日志/审计字段的一部分，改动等于改日志格式。
func (c QuotaClamp) String() string {
	switch c {
	case ClampOverflow:
		return "overflow"
	case ClampUnderflow:
		return "underflow"
	case ClampNaN:
		return "nan"
	default:
		return "none"
	}
}

// AdminMap 供写入日志的 admin_info.quota_saturation 字段，便于管理员事后排查。
func (c QuotaClamp) AdminMap() map[string]any {
	return map[string]any{
		"quota_saturation": c.String(),
		"saturated":        c.Saturated(),
	}
}

// Strict 档的错误。用哨兵错误而不是字符串，因为重试/退款决策要靠 errors.Is。
var (
	ErrQuotaOverflow = errors.New("quota 超过上限")
	ErrQuotaNaN      = errors.New("quota 不是有效数值")
	ErrQuotaNegative = errors.New("quota 为负数")
)

// clampQuota 把已经取整的浮点落到 [0, MaxQuota]，并报告饱和类型。
//
// 语义（本项目的决定，写进账本前请复核）：
//
//	NaN              → 0        + ClampNaN
//	+Inf / > MaxQuota → MaxQuota + ClampOverflow
//	-Inf / < 0        → 0        + ClampUnderflow
//
// 负号要在取整**之前**判断：math.Round(-0.4) 是 -0，而 Go 里 -0 < 0 为假，
// 先取整会把"负数金额"当成正常值。三档（Float/Round/Decimal）对同一输入
// 的判定必须一致，否则调用方换个入口就得到相反的结论。
//
// 另注：指数超出 decimal 解码上限的字符串（如 "1e1000000000"）在 decimal 档
// 报 ClampNaN 而不是 ClampOverflow——值从未被成功解析出来，无从与上限比较。
//
// 为什么 NaN 不饱和到 MaxQuota：结算路径上那会给出荒谬的巨额扣费。
// 饱和到 0 是"宁可少扣"，但正因为危险，所以必须返回标记让调用方显式处理
// ——零 usage 绝不能当成免费。
func clampQuota(f float64) (int, QuotaClamp) {
	switch {
	case math.IsNaN(f):
		return 0, ClampNaN
	case math.IsInf(f, 1) || f > MaxQuota:
		return MaxQuota, ClampOverflow
	case math.IsInf(f, -1) || f < 0:
		return 0, ClampUnderflow
	}
	return int(f), ClampNone
}

func clampError(c QuotaClamp) error {
	switch c {
	case ClampOverflow:
		return ErrQuotaOverflow
	case ClampUnderflow:
		return ErrQuotaNegative
	case ClampNaN:
		return ErrQuotaNaN
	default:
		return nil
	}
}

// QuotaFromFloat 静默饱和：只用于展示类计算，**不要用于记账**。
// 记账请用 Checked（结算/日志）或 Strict（预扣）。
func QuotaFromFloat(f float64) int {
	v, _ := clampQuota(f)
	return v
}

// QuotaFromFloatChecked 饱和并留痕：用于结算与日志。
func QuotaFromFloatChecked(f float64) (int, QuotaClamp) { return clampQuota(f) }

// QuotaFromFloatStrict 溢出即错：用于预扣——宁可拒绝请求，也不能按错误金额冻结。
func QuotaFromFloatStrict(f float64) (int, error) {
	v, clamp := clampQuota(f)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
}

// QuotaRound 先取整（四舍五入、远离零）再饱和。
//
// 分层计费必须用它：如果先算小数再截断，预扣与结算会差 ±1 quota，
// 日志里天天出现"金额对不上"的噪声。
func QuotaRound(f float64) int {
	v, _ := QuotaRoundChecked(f)
	return v
}

// QuotaRoundChecked 与 QuotaFromFloatChecked 对同一输入的判定必须一致，
// 所以先判符号再取整（-0.4 取整成 -0 会丢掉符号，详见 clampQuota 的说明）。
func QuotaRoundChecked(f float64) (int, QuotaClamp) {
	if math.IsNaN(f) {
		return 0, ClampNaN
	}
	if f < 0 {
		return 0, ClampUnderflow
	}
	return clampQuota(math.Round(f))
}

func QuotaRoundStrict(f float64) (int, error) {
	v, clamp := QuotaRoundChecked(f)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
}

// QuotaFromDecimal 用 decimal 处理字符串价格，避免 "0.1" 这类值走 float 丢精度。
// 只用于展示或已确认输入合法的场景；记账请用 Checked / Strict。
func QuotaFromDecimal(s string) int {
	v, _ := QuotaFromDecimalChecked(s)
	return v
}

// QuotaFromDecimalChecked 解析失败或越界时饱和并留痕。
func QuotaFromDecimalChecked(s string) (int, QuotaClamp) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return 0, ClampNaN
	}
	return decimalToQuota(d)
}

// QuotaFromDecimalStrict 解析失败或越界即错（预扣路径）。
func QuotaFromDecimalStrict(s string) (int, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		// 包上 ErrQuotaNaN：Checked 档对同一输入给 ClampNaN、浮点 Strict 档给
		// ErrQuotaNaN，唯独 strict 这一档拿不到统一的哨兵，调用方就没法用
		// errors.Is 识别"这不是一个有效金额"。
		// shopspring 没有导出解析错误变量，只能把原始错误以文本形式附上。
		return 0, fmt.Errorf("%w：无法解析金额 %q（%v）", ErrQuotaNaN, s, err)
	}
	v, clamp := decimalToQuota(d)
	if clamp.Saturated() {
		return 0, clampError(clamp)
	}
	return v, nil
}

// decimalToQuota 把 decimal 四舍五入到整数（远离零）后落到 [0, MaxQuota]。
//
// 注意这里全程不经过 float64：几十位的价格字符串转 float 就丢精度了，
// 越界判断会变得不可靠。
func decimalToQuota(d decimal.Decimal) (int, QuotaClamp) {
	// 负号在取整前判：d.Round(0) 会把 -0.4 变成 0，之后 IsNegative() 就为假了，
	// 于是同一个负数在 decimal 档变成 ClampNone、在浮点档却是 ClampUnderflow。
	if d.IsNegative() {
		return 0, ClampUnderflow
	}
	rounded := d.Round(0) // shopspring 的 Round 是 half away from zero
	if rounded.GreaterThan(decimal.NewFromInt(MaxQuota)) {
		return MaxQuota, ClampOverflow
	}
	return int(rounded.IntPart()), ClampNone
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/common/ -v`
Expected: PASS，全部通过（含前面任务的测试）。

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/common/quota_math.go internal/common/quota_math_test.go
git commit -m "feat(common): 整数记账的数值安全函数族（三档饱和语义）

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 6: 结构化日志（`internal/logger`）

**Files:**
- Create: `internal/logger/logger.go`
- Test: `internal/logger/logger_test.go`

级别要能在运行时改（它是配置热更新的一条副作用），所以级别存在 `atomic` 里，
由自定义 `slog.Leveler` 每次判断时读取。

- [ ] **Step 1: 写失败的测试**

创建 `internal/logger/logger_test.go`：

```go
package logger

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" info ", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"bogus", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("ParseLevel(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// 配置项 system_setting.log_level 改完必须立即生效，不需要重启。
func TestSetLevelTakesEffectImmediately(t *testing.T) {
	SetupLogger("info", false)

	if Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("info 级别下 debug 不该启用")
	}
	if !Logger().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info 级别下 info 应启用")
	}

	SetLevel(ParseLevel("debug"))
	if !Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("改成 debug 后 debug 应启用")
	}

	SetLevel(ParseLevel("error"))
	if Logger().Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("改成 error 后 warn 不该启用")
	}
}

// SetupLogger 之前就有人打日志时不能 panic。
func TestLoggerBeforeSetupDoesNotPanic(t *testing.T) {
	Info("这条日志发生在 SetupLogger 之前")
	if Logger() == nil {
		t.Fatal("Logger() 不应返回 nil")
	}
}

// 钉住调用顺序契约：SetupLogger 会用 LOG_LEVEL 覆盖此前 SetLevel 设的值。
//
// 启动流程若把"读配置"放在 SetupLogger 之前，配置里的 log_level 就会被
// 静默吃掉，现象是"改了日志级别没反应"而日志里毫无线索。这条测试让
// 覆盖语义一旦变化就地报警，也说明为什么顺序不能反。
func TestSetupLoggerOverridesEarlierSetLevel(t *testing.T) {
	SetLevel(slog.LevelDebug)  // 假装配置域先跑了一步
	SetupLogger("info", false) // 启动流程随后才建 logger

	if Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("SetupLogger 应覆盖此前 SetLevel 设的级别（这正是顺序不能反的原因）")
	}
	if !Logger().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("SetupLogger 之后应按 LOG_LEVEL 生效")
	}

	// SetupLogger 之后再 SetLevel，才是唯一有效的用法。
	SetLevel(slog.LevelDebug)
	if !Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("SetupLogger 之后的 SetLevel 应生效")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/logger/`
Expected: FAIL — `undefined: ParseLevel`。

- [ ] **Step 3: 写实现**

创建 `internal/logger/logger.go`：

```go
// Package logger 是全项目的结构化日志入口（标准库 log/slog）。
//
// 日志级别可以在运行时更新（配置热更新的一条副作用），所以级别存在
// atomic 里，由自定义的 slog.Leveler 在每次判断时读取——而不是在
// SetupLogger 时把级别固化进 HandlerOptions。
//
// ★ 调用顺序：SetupLogger 必须在启动流程里**先于**任何 SetLevel 与任何配置读取。
//
// 原因是 SetupLogger 会用 LOG_LEVEL 重写级别。若配置域在 SetupLogger 之前
// 就读到了 system_setting.log_level 并调了 SetLevel，那条设置会被随后的
// SetupLogger 静默覆盖——现象是"改了日志级别没反应"，而日志里没有任何线索
// 说明它被谁改回去了。启动流程的正确顺序是：
//
//	InitEnv → SetupLogger(LOG_LEVEL) → 读配置 → 之后才开始 SetLevel
//
// 在此之前调用 SetLevel 也不会报错（它只写一个 atomic），但那次设置
// 对输出没有任何影响：Logger() 这时返回的是 slog 默认 logger，
// 它按自己的级别过滤。
package logger

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

var (
	current atomic.Pointer[slog.Logger]
	level   atomic.Int32 // 存 slog.Level
)

// dynamicLevel 让 Handler 每次判断是否输出时都读当前级别。
type dynamicLevel struct{}

func (dynamicLevel) Level() slog.Level { return slog.Level(level.Load()) }

// SetupLogger 建初始 logger。level 来自环境变量 LOG_LEVEL，
// 之后可由配置项 system_setting.log_level 覆盖。
// jsonFormat 为 false 时输出便于人读的文本格式（本地开发）。
//
// **它会用 levelName 重写当前级别**，所以必须在读配置之前调用一次，
// 且整个进程只调用一次。详见包注释里的调用顺序。
func SetupLogger(levelName string, jsonFormat bool) {
	level.Store(int32(ParseLevel(levelName)))

	opts := &slog.HandlerOptions{Level: dynamicLevel{}}
	var handler slog.Handler
	if jsonFormat {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	current.Store(slog.New(handler))
}

// SetLevel 运行时更新日志级别（配置热更新的副作用走这里）。
//
// 必须在 SetupLogger 之后调用：SetupLogger 会覆盖这里设的值。
func SetLevel(lv slog.Level) { level.Store(int32(lv)) }

// ParseLevel 解析 debug/info/warn/error，非法值回退 info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Logger 返回当前 logger。SetupLogger 之前调用会退回 slog 默认 logger，
// 而不是返回 nil——初始化早期打日志是很正常的事。
func Logger() *slog.Logger {
	if l := current.Load(); l != nil {
		return l
	}
	return slog.Default()
}

func Debug(msg string, args ...any) { Logger().Debug(msg, args...) }
func Info(msg string, args ...any)  { Logger().Info(msg, args...) }
func Warn(msg string, args ...any)  { Logger().Warn(msg, args...) }
func Error(msg string, args ...any) { Logger().Error(msg, args...) }
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/logger/ -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/logger/
git commit -m "feat(logger): slog 封装与可运行时更新的日志级别

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 7: Redis 封装与混合计数器（`internal/common/redis.go` + `internal/cachex`）

**Files:**
- Create: `internal/common/redis.go`
- Create: `internal/common/redis_test.go`
- Create: `internal/cachex/counter.go`
- Test: `internal/cachex/counter_test.go`

**设计上的一个取舍，先说清楚：** 设计文档 §6 原本还规划了泛型的 `HybridCache[V]` 与
`ValueCodec`。M0 没有任何缓存对象，写了就是没人用的死代码——所以本任务只做
**真正被用到的** `Counter`（`/api/status` 的请求总数），`HybridCache[V]` 推迟到第一个
真的需要缓存的里程碑（渠道缓存/定价缓存）。这与"不引入死代码"是同一把尺子。

先引入依赖：

Run: `go get github.com/go-redis/redis/v8@v8.11.5`
Expected: `go.mod` 增加 `github.com/go-redis/redis/v8 v8.11.5`。

- [ ] **Step 1: 写失败的测试**

创建 `internal/common/redis_test.go`：

```go
package common

import (
	"context"
	"errors"
	"testing"

	"github.com/go-redis/redis/v8"
)

// resetRedis 把 Redis 状态清成"未启用"。测试与实现同包，直接用未导出的变量，
// 不为测试在生产代码里开后门。
func resetRedis(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = CloseRedis()
		RedisConnString = ""
	})
	_ = CloseRedis()
}

// Redis 未启用时，底层函数必须明确报错，而不是空指针崩掉或静默返回零值——
// 静默返回零值会让上层"降级"变成"数据丢失"。
func TestRedisFunctionsFailClearlyWhenDisabled(t *testing.T) {
	resetRedis(t)

	if RedisEnabled() || LoadRedis() != nil {
		t.Fatal("前置条件不成立：此刻句柄应为空")
	}

	if _, err := RedisGet("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisGet 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if err := RedisSet("k", "v", 0); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisSet 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if err := RedisDel("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisDel 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if _, err := RedisIncr("k", 0); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisIncr 应返回 ErrRedisDisabled，得到 %v", err)
	}
}

// 未配置 REDIS_CONN_STRING 时 InitRedisClient 必须安静降级，不能 panic、不能卡住。
// 同时验证它会把上一轮的句柄**关掉**：否则反复 Init 会漏掉连接池。
func TestInitRedisClientWithoutConfigDegrades(t *testing.T) {
	t.Cleanup(func() { _ = CloseRedis(); RedisConnString = "" })

	// 制造"上一轮已启用"的脏状态。这里的 client 从不真正连出去，
	// 本用例只关心句柄有没有被摘掉、有没有被关闭。
	stale := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	rdb.Store(stale)
	redisEnabled.Store(true)

	RedisConnString = ""
	InitRedisClient()

	if RedisEnabled() {
		t.Error("未配置 REDIS_CONN_STRING 时 RedisEnabled 必须为 false")
	}
	if LoadRedis() != nil {
		t.Error("未配置时不应留下 client")
	}
	if err := stale.Ping(context.Background()).Err(); err == nil {
		t.Error("旧 client 必须被关闭，否则每次 Init 都漏一个连接池")
	}
}

// 配置了一个连不上的地址：必须降级而不是启动失败——这是第二条验收标准。
func TestInitRedisClientUnreachableDegrades(t *testing.T) {
	t.Cleanup(func() { _ = CloseRedis(); RedisConnString = "" })

	RedisConnString = "127.0.0.1:1" // 保留端口，必然连不上
	InitRedisClient()

	if RedisEnabled() {
		t.Error("连接失败时 RedisEnabled 必须为 false")
	}
	if LoadRedis() != nil {
		t.Error("连接失败时不应留下 client")
	}
}

// 关闭后状态必须一并清空：否则调用方拿到的是驱动层的 "client is closed"，
// 一个没法用 errors.Is 识别的错误。Close 必须幂等。
func TestCloseRedisClearsState(t *testing.T) {
	t.Cleanup(func() { RedisConnString = "" })

	rdb.Store(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}))
	redisEnabled.Store(true)

	if err := CloseRedis(); err != nil {
		t.Errorf("CloseRedis 应成功，得到 %v", err)
	}
	if RedisEnabled() || LoadRedis() != nil {
		t.Error("CloseRedis 之后必须报告未启用")
	}
	if _, err := RedisGet("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("关闭后应得到 ErrRedisDisabled，得到 %v", err)
	}
	if err := CloseRedis(); err != nil {
		t.Errorf("CloseRedis 必须幂等，第二次调用报错 %v", err)
	}
}

func TestNormalizeRedisURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"redis://127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"rediss://host:6380", "rediss://host:6380"},
		{"  127.0.0.1:6379  ", "redis://127.0.0.1:6379"},
	}
	for _, c := range cases {
		if got := normalizeRedisURL(c.in); got != c.want {
			t.Errorf("normalizeRedisURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
```

创建 `internal/cachex/counter_test.go`：

```go
package cachex

import (
	"sync"
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

// useMemoryBackend 让 Counter 走内存路径。
//
// Counter 的判断依据是 common.RedisEnabled()，而 Redis 的开关状态是 common
// 的内部状态，cachex 够不着——只能通过公共 API 把它关掉。CloseRedis 正是
// 语义上最贴切的那个：关闭之后系统本来就该退化到内存。
func useMemoryBackend(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = common.CloseRedis() })
	_ = common.CloseRedis()
}

func TestCounterUsesMemoryBackendWhenRedisDisabled(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_requests", time.Minute)
	if c.Backend() != "memory" {
		t.Fatalf("Backend() = %q，期望 memory（这是第二条验收标准的观察点）", c.Backend())
	}

	if _, ok := c.Get("total"); ok {
		t.Fatal("未自增过就不该有值")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Incr("total"); err != nil {
			t.Fatalf("Incr 失败: %v", err)
		}
	}
	got, ok := c.Get("total")
	if !ok || got != 3 {
		t.Errorf("Get(total) = (%d, %v)，期望 (3, true)", got, ok)
	}
}

// 计数器必须是原子的：并发自增不能丢计数。
func TestCounterIncrIsAtomic(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_concurrent", time.Minute)
	const n = 100

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Incr("total"); err != nil {
				t.Errorf("Incr 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	got, _ := c.Get("total")
	if got != n {
		t.Errorf("并发自增 %d 次后 Get = %d，说明丢了计数", n, got)
	}
}

// 不同 key 互不干扰。
func TestCounterKeysAreIndependent(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_keys", time.Minute)
	if _, err := c.Incr("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Incr("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Incr("b"); err != nil {
		t.Fatal(err)
	}

	if v, _ := c.Get("a"); v != 2 {
		t.Errorf("a = %d，期望 2", v)
	}
	if v, _ := c.Get("b"); v != 1 {
		t.Errorf("b = %d，期望 1", v)
	}
}

// TTL 是滑动窗口：每次自增都把过期时间往后推，与 Redis 的 INCR+EXPIRE 一致。
func TestCounterTTLIsSlidingWindow(t *testing.T) {
	useMemoryBackend(t)

	const ttl = 400 * time.Millisecond
	c := NewCounter("test_ttl", ttl)
	if _, err := c.Incr("k"); err != nil {
		t.Fatal(err)
	}

	// 在第一次的 TTL 到期前再自增一次，把过期时间往后推。
	time.Sleep(350 * time.Millisecond)
	if _, err := c.Incr("k"); err != nil {
		t.Fatal(err)
	}

	// t≈500ms：已越过"不滑动时"的 400ms 截止点，但第二次自增把窗口推到了 750ms。
	// 留了 ±100ms 余量，避免机器负载高时误报。
	time.Sleep(150 * time.Millisecond)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Errorf("滑动窗口未生效：Get = (%d, %v)，期望 (2, true)", v, ok)
	}

	time.Sleep(400 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Error("TTL 到期后应过期")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/common/ ./internal/cachex/`
Expected: FAIL — `undefined: ErrRedisDisabled`、`undefined: NewCounter`。

- [ ] **Step 3: 写 `internal/common/redis.go`**

```go
package common

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/yyyCode/whats-api/internal/logger"
)

// ErrRedisDisabled 表示 Redis 未启用（没配置，或启动期就没连上）。
// 调用方据此走进程内缓存——这正是"拿掉 Redis 也能启动"的实现方式。
//
// 注意它**不**覆盖「启动时正常、运行中掉线」：那种情况下 RedisGet/RedisIncr
// 返回的是 redis 驱动的原始错误（连接失败等），不是这个哨兵。需要在运行期
// 降级的调用方（如 cachex.Counter）应当对任何 error 都回退，而不是只认哨兵。
var ErrRedisDisabled = errors.New("redis 未启用")

// 句柄写成 atomic：可用性在每个请求上都被读到，而 Init/Close 是写方。
// 万一将来加入运行期重连，普通变量的并发读写就是数据竞争（-race 会报），
// 不是"读到旧值"这么轻。LoadRedis 一次 Load 同时判定可用性与取句柄，
// 分两次读可能跨越一次重连，拿到"已判定可用但句柄已换"的错配。
var (
	redisEnabled atomic.Bool
	rdb          atomic.Pointer[redis.Client]
)

// LoadRedis 返回当前客户端；未启用时返回 nil。
func LoadRedis() *redis.Client {
	if !redisEnabled.Load() {
		return nil
	}
	return rdb.Load()
}

// RedisEnabled 报告是否有可用的客户端。
func RedisEnabled() bool { return LoadRedis() != nil }

// InitRedisClient 初始化 Redis。**非致命**：
//
//	未配置 REDIS_CONN_STRING → 直接用进程内缓存
//	配置了但解析失败/连不上 → 告警并降级为进程内缓存
//
// 两种失败都不阻断启动：缓存是加速手段，不该成为可用性的单点。
func InitRedisClient() {
	// 先摘掉旧句柄再探测：探测期间 LoadRedis 返回 nil，调用方走进程内缓存，
	// 不会拿到一个马上要被关掉的客户端。
	redisEnabled.Store(false)
	if old := rdb.Swap(nil); old != nil {
		_ = old.Close()
	}

	if RedisConnString == "" {
		logger.Info("未配置 REDIS_CONN_STRING，使用进程内缓存")
		return
	}

	opt, err := redis.ParseURL(normalizeRedisURL(RedisConnString))
	if err != nil {
		logger.Warn("REDIS_CONN_STRING 解析失败，降级为进程内缓存", "error", err)
		return
	}

	client := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("Redis 连接失败，降级为进程内缓存", "error", err)
		_ = client.Close()
		return
	}

	rdb.Store(client)
	redisEnabled.Store(true)
	logger.Info("Redis 已启用")
}

// normalizeRedisURL 允许写 "127.0.0.1:6379" 这种简写：
// redis.ParseURL 只认 URL 形式，而运维习惯写 host:port。
func normalizeRedisURL(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return s
	}
	return "redis://" + s
}

// RedisKeyCacheSeconds 是缓存键的默认 TTL：与配置同步频率一致，
// 这样"多节点最终一致"的窗口只有一个概念，而不是两个不同的时间常数。
func RedisKeyCacheSeconds() int { return SyncFrequency }

// CloseRedis 关闭客户端并清空句柄：关闭后 RedisEnabled() 返回 false，
// 后续调用拿到的是 ErrRedisDisabled，而不是"client is closed"这种
// 没法用 errors.Is 识别的驱动错误。
func CloseRedis() error {
	redisEnabled.Store(false)
	old := rdb.Swap(nil)
	if old == nil {
		return nil
	}
	return old.Close()
}

func RedisGet(key string) (string, error) {
	client := LoadRedis()
	if client == nil {
		return "", ErrRedisDisabled
	}
	return client.Get(context.Background(), key).Result()
}

// RedisSet 写入键值并设 TTL。ttl <= 0 表示不过期。
func RedisSet(key, value string, ttl time.Duration) error {
	client := LoadRedis()
	if client == nil {
		return ErrRedisDisabled
	}
	return client.Set(context.Background(), key, value, ttl).Err()
}

func RedisDel(key string) error {
	client := LoadRedis()
	if client == nil {
		return ErrRedisDisabled
	}
	return client.Del(context.Background(), key).Err()
}

// RedisIncr 原子自增，并刷新 TTL（滑动窗口）。
// 走 pipeline 是为了让 INCR 与 EXPIRE 一次往返完成，避免"自增了但没设过期"的中间态。
func RedisIncr(key string, ttl time.Duration) (int64, error) {
	client := LoadRedis()
	if client == nil {
		return 0, ErrRedisDisabled
	}
	ctx := context.Background()
	pipe := client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
```

- [ ] **Step 4: 写 `internal/cachex/counter.go`**

```go
// Package cachex 提供"Redis 可用则用 Redis、不可用则退化为进程内实现"的缓存原语。
//
// 单机部署与多实例部署共用同一份代码，代价是必须把两种后端的语义差异
// （TTL 精度、原子性）封在实现内部，不让调用方感知。
package cachex

import (
	"sync"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

// Counter 是带 TTL 的原子计数器。
//
// 它与"键值缓存"分开建模，因为语义不同：计数器关心的是原子自增与滑动过期，
// 而键值缓存关心的是编解码与 TTL 精度。混成一个类型会让两边都别扭。
type Counter struct {
	namespace string
	ttl       time.Duration
	memory    *memoryCounter
}

func NewCounter(namespace string, ttl time.Duration) *Counter {
	return &Counter{
		namespace: common.CacheNamespacePrefix + ":" + namespace,
		ttl:       ttl,
		memory:    newMemoryCounter(),
	}
}

// useRedis 每次调用都重新判断，而不是在构造时固化：
// Redis 中途掉线后代码应继续工作（降级），而不是整片报错。
func (c *Counter) useRedis() bool { return common.RedisEnabled() }

func (c *Counter) key(key string) string { return c.namespace + ":" + key }

// Backend 报告**选用**的后端，供 /api/status 与运维排查使用。
//
// 它反映的是配置与启动期探测的结果，不含运行期单次调用的成败：
// 启动时连通、之后 Redis 掉线时它仍返回 "redis"，而 Incr/Get 已经在
// 回落到内存。要判断"此刻 Redis 是否真在服务"，得看错误日志而不是这里。
func (c *Counter) Backend() string {
	if c.useRedis() {
		return "redis"
	}
	return "memory"
}

// Incr 原子自增并返回自增后的值。Redis 路径失败时回落到内存路径，
// 保证调用方永远拿得到结果（计数精度不如可用性重要）。
func (c *Counter) Incr(key string) (int64, error) {
	if c.useRedis() {
		if v, err := common.RedisIncr(c.key(key), c.ttl); err == nil {
			return v, nil
		}
	}
	return c.memory.Incr(key, c.ttl), nil
}

// Get 读取当前值。内存后端的 TTL 是滑动窗口（与 Redis 路径的 EXPIRE 语义一致）。
//
// Redis 出错时回落到内存，与 Incr 对称。不对称会造出一个很难查的现象：
// 掉线期间 Incr 写进了内存，而 Get 去读 Redis 得到 miss，刚加上的计数
// 立刻"不存在"，看日志还以为没写进去。
func (c *Counter) Get(key string) (int64, bool) {
	if c.useRedis() {
		raw, err := common.RedisGet(c.key(key))
		if err == nil {
			if v, perr := parseCounter(raw); perr == nil {
				return v, true
			}
		}
	}
	return c.memory.Get(key)
}

// memoryCounter 是进程内实现。不需要精确的过期语义（惰性判断即可）：
// 多实例一致性靠定时同步，不靠缓存。
type memoryCounter struct {
	mu   sync.Mutex
	data map[string]counterEntry
}

type counterEntry struct {
	value     int64
	expiresAt time.Time
}

func newMemoryCounter() *memoryCounter {
	return &memoryCounter{data: map[string]counterEntry{}}
}

func (m *memoryCounter) Incr(key string, ttl time.Duration) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	e, ok := m.data[key]
	if !ok || (ttl > 0 && !e.expiresAt.IsZero() && now.After(e.expiresAt)) {
		e.value = 0 // 已过期：窗口重开
	}
	e.value++
	if ttl > 0 {
		e.expiresAt = now.Add(ttl) // 与 Redis 的 INCR+EXPIRE 一致：滑动窗口
	} else {
		e.expiresAt = time.Time{}
	}
	m.data[key] = e
	return e.value
}

func (m *memoryCounter) Get(key string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.data[key]
	if !ok {
		return 0, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(m.data, key)
		return 0, false
	}
	return e.value, true
}
```

`parseCounter` 依赖 `strconv`，放在同文件底部：

```go
func parseCounter(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}
```

记得 import：

```go
import (
	"strconv"
	"sync"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/common/ ./internal/cachex/ -v`
Expected: PASS。
其中 `TestInitRedisClientUnreachableDegrades` 会真的去连 `127.0.0.1:1`，本机应在 5 秒内快速失败
（端口 1 无监听，连接被立刻拒绝），不会拖慢测试。

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/common/redis.go internal/common/redis_test.go internal/cachex/
git commit -m "feat(cachex): Redis 封装与可降级的原子计数器

未配置或连不上 Redis 时降级为进程内实现，不阻断启动。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 8: 配置注册表（`internal/setting/config`）

**Files:**
- Create: `internal/setting/config/config.go`
- Test: `internal/setting/config/config_test.go`

这是 M0 的重心。**一个结构体同时定义四件事**：类型、默认值、持久化 key、文档。
注册发生在 `init()` 里，所以任何用法错误都应该在启动期就 panic——运行时才发现
"某个配置项没注册"要难查得多。

- [ ] **Step 1: 写失败的测试**

创建 `internal/setting/config/config_test.go`：

```go
package config

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

type testSetting struct {
	Level  string  `json:"level"`
	Enable bool    `json:"enable"`
	Count  int     `json:"count"`
	Total  int64   `json:"total"`
	Factor float64 `json:"factor"`
}

func newTestSetting() testSetting {
	return testSetting{Level: "info", Enable: true, Count: 3, Total: 99, Factor: 1.5}
}

func TestRegisterAndExportDefaults(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	got := m.ExportAllConfigs()
	want := map[string]string{
		"test_setting.level":  `"info"`,
		"test_setting.enable": `true`,
		"test_setting.count":  `3`,
		"test_setting.total":  `99`,
		"test_setting.factor": `1.5`,
	}
	if len(got) != len(want) {
		t.Errorf("配置项数量 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ExportAllConfigs()[%q] = %q，期望 %q", k, got[k], v)
		}
	}

	if mods := m.Modules(); len(mods) != 1 || mods[0] != "test_setting" {
		t.Errorf("Modules() = %v，期望 [test_setting]", mods)
	}
	if n := m.FieldCount(); n != len(want) {
		t.Errorf("FieldCount() = %d，期望 %d", n, len(want))
	}
}

func TestUpdateConfigWritesAllSupportedTypes(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	rebuilds := 0
	m.OnRebuild("test_setting", func() { rebuilds++ })

	cases := []struct {
		key string
		raw string
	}{
		{"test_setting.level", `"debug"`},
		{"test_setting.enable", `false`},
		{"test_setting.count", `7`},
		{"test_setting.total", `123456789012`},
		{"test_setting.factor", `2.25`},
	}
	for _, c := range cases {
		if err := m.UpdateConfig(c.key, c.raw); err != nil {
			t.Fatalf("UpdateConfig(%q, %q) 失败: %v", c.key, c.raw, err)
		}
	}

	if s.Level != "debug" {
		t.Errorf("Level = %q，期望 debug", s.Level)
	}
	if s.Enable {
		t.Error("Enable 应为 false")
	}
	if s.Count != 7 {
		t.Errorf("Count = %d，期望 7", s.Count)
	}
	if s.Total != 123456789012 {
		t.Errorf("Total = %d，期望 123456789012", s.Total)
	}
	if s.Factor != 2.25 {
		t.Errorf("Factor = %v，期望 2.25", s.Factor)
	}
	// 每次成功写入都必须让快照重建，否则热路径读到的还是旧值。
	if rebuilds != len(cases) {
		t.Errorf("快照重建次数 = %d，期望 %d", rebuilds, len(cases))
	}

	// GetConfig 应能读回刚写入的值。
	if v, ok := m.GetConfig("test_setting.level"); !ok || v != `"debug"` {
		t.Errorf("GetConfig(level) = (%q, %v)，期望 (\"debug\", true)", v, ok)
	}
}

// 非法写入必须报错且不留副作用：管理端请求不能把运行中的配置改坏。
func TestUpdateConfigRejectsBadInputWithoutSideEffects(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	rebuilds := 0
	m.OnRebuild("test_setting", func() { rebuilds++ })

	cases := []struct {
		name string
		key  string
		raw  string
	}{
		{"未注册的模块", "nope.level", `"x"`},
		{"未注册的字段", "test_setting.nope", `"x"`},
		{"key 缺少模块前缀", "level", `"x"`},
		{"key 为空", "", `"x"`},
		{"整数项给了字符串", "test_setting.count", `"not-a-number"`},
		{"字符串项给了数字", "test_setting.level", `123`},
		{"布尔项给了对象", "test_setting.enable", `{}`},
		{"浮点项给了数组", "test_setting.factor", `[]`},
		{"整数项给了 null", "test_setting.count", `null`},
		{"字符串项给了 null", "test_setting.level", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := m.UpdateConfig(c.key, c.raw); err == nil {
				t.Fatalf("UpdateConfig(%q, %q) 应该报错", c.key, c.raw)
			}
		})
	}

	if s != newTestSetting() {
		t.Errorf("失败的写入改变了配置值：%+v", s)
	}
	if rebuilds != 0 {
		t.Errorf("失败的写入不应触发快照重建，得到 %d 次", rebuilds)
	}
}

// ValidateConfig 是"先校验、后落库"的前置检查（model.UpdateOption 依赖它）。
// 它必须同时满足两件事：(a) 不改变任何状态；(b) 接受集合与 UpdateConfig 完全一致。
func TestValidateConfigDoesNotWriteAndAgreesWithUpdate(t *testing.T) {
	newManager := func(rebuilds *int) (*Manager, *testSetting) {
		m := NewManager()
		s := newTestSetting()
		m.Register("test_setting", &s)
		m.OnRebuild("test_setting", func() { *rebuilds++ })
		return m, &s
	}

	valid := []struct{ key, raw string }{
		{"test_setting.level", `"debug"`},
		{"test_setting.enable", `false`},
		{"test_setting.count", `42`},
		{"test_setting.total", `9007199254740991`},
		{"test_setting.factor", `1.25`},
	}
	invalid := []struct{ key, raw string }{
		{"test_setting.nope", `"x"`},
		{"nope.level", `"x"`},
		{"level", `"x"`},
		{"", `"x"`},
		{"test_setting.count", `"not-a-number"`},
		{"test_setting.level", `123`},
		{"test_setting.enable", `{}`},
		{"test_setting.factor", `[]`},
		{"test_setting.count", `null`},
	}

	// (a) 校验不写任何东西：值不变、快照不重建
	rebuilds := 0
	m, s := newManager(&rebuilds)
	before := *s

	for _, c := range valid {
		if err := m.ValidateConfig(c.key, c.raw); err != nil {
			t.Errorf("ValidateConfig(%q, %q) 意外报错: %v", c.key, c.raw, err)
		}
	}
	for _, c := range invalid {
		if err := m.ValidateConfig(c.key, c.raw); err == nil {
			t.Errorf("ValidateConfig(%q, %q) 应该报错", c.key, c.raw)
		}
	}

	if *s != before {
		t.Errorf("ValidateConfig 改变了配置值：%+v → %+v", before, *s)
	}
	if rebuilds != 0 {
		t.Errorf("ValidateConfig 触发了快照重建 %d 次（它不该有任何副作用）", rebuilds)
	}

	// (b) 接受集合一致。这条断言挡住的是"校验通过但写入失败"的裂缝——
	// 那个裂缝会让 model.UpdateOption 把非法值先落进库，再被别的实例同步出去。
	rebuilds2 := 0
	m2, _ := newManager(&rebuilds2)

	for _, c := range valid {
		if err := m2.UpdateConfig(c.key, c.raw); err != nil {
			t.Errorf("ValidateConfig 放行但 UpdateConfig 拒绝: (%q, %q): %v", c.key, c.raw, err)
		}
	}
	for _, c := range invalid {
		if err := m2.UpdateConfig(c.key, c.raw); err == nil {
			t.Errorf("ValidateConfig 拒绝但 UpdateConfig 接受: (%q, %q)", c.key, c.raw)
		}
	}

	if rebuilds2 != len(valid) {
		t.Errorf("成功的写入应每次重建快照：得到 %d 次，期望 %d 次", rebuilds2, len(valid))
	}
}

// 注册期的用法错误一律 panic（fail-fast），而不是留到运行时。
func TestRegisterPanicsOnMisuse(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"重复注册", func() {
			m := NewManager()
			s := newTestSetting()
			m.Register("dup", &s)
			m.Register("dup", &s)
		}},
		{"传了结构体值而不是指针", func() {
			NewManager().Register("x", newTestSetting())
		}},
		{"传了 nil", func() {
			NewManager().Register("x", nil)
		}},
		{"模块名为空", func() {
			s := newTestSetting()
			NewManager().Register("", &s)
		}},
		{"字段缺少 json tag", func() {
			s := struct{ Level string }{}
			NewManager().Register("x", &s)
		}},
		// 用 reflect.StructOf 动态构造带重复 json tag 的类型：go vet 的 structtag
		// 检查会拒绝源码里静态写死的重复 tag，而这里要测的是 Register 的运行时行为。
		{"json tag 重复", func() {
			typ := reflect.StructOf([]reflect.StructField{
				{Name: "A", Type: reflect.TypeOf(""), Tag: reflect.StructTag(`json:"same"`)},
				{Name: "B", Type: reflect.TypeOf(""), Tag: reflect.StructTag(`json:"same"`)},
			})
			NewManager().Register("x", reflect.New(typ).Interface())
		}},
		{"不支持的字段类型", func() {
			s := struct {
				Tags []string `json:"tags"`
			}{}
			NewManager().Register("x", &s)
		}},
		{"OnRebuild 注册到不存在的模块", func() {
			NewManager().OnRebuild("nope", func() {})
		}},
		// 模块名或 tag 里出现 '.'：key 用第一个点切分，带点的名字会让不同模块
		// 撞进同一个 key。这种情况必须在注册期就拦下。
		{"模块名含点", func() {
			s := newTestSetting()
			NewManager().Register("a.b", &s)
		}},
		{"json tag 含点", func() {
			s := struct {
				Level string `json:"a.b"`
			}{}
			NewManager().Register("x", &s)
		}},
		// NaN/±Inf 编不出 JSON：MustMarshalString 会返回空串，于是 Option 表的
		// 默认值里出现一个不是 JSON 的 ""，等别处解析才报错。
		{"浮点默认值是 NaN", func() {
			s := struct {
				F float64 `json:"f"`
			}{F: math.NaN()}
			NewManager().Register("x", &s)
		}},
		{"浮点默认值是正无穷", func() {
			s := struct {
				F float64 `json:"f"`
			}{F: math.Inf(1)}
			NewManager().Register("x", &s)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("应该 panic")
				}
			}()
			c.fn()
		})
	}
}

// json:"-" 是 Go 里"明确排除该字段"的写法，不是漏写 tag：它是合法意图，
// 不该 panic，也不该出现在配置项里。
func TestRegisterSkipsExplicitlyExcludedFields(t *testing.T) {
	m := NewManager()
	s := struct {
		Level  string `json:"level"`
		Secret string `json:"-"`
		hidden string // 未导出字段同样不参与配置
	}{Level: "info", Secret: "s3cr3t"}

	m.Register("mod", &s)

	got := m.ExportAllConfigs()
	if _, ok := got["mod.secret"]; ok {
		t.Error(`json:"-" 的字段不该出现在配置里`)
	}
	if got["mod.level"] != `"info"` {
		t.Errorf(`ExportAllConfigs()["mod.level"] = %q，期望 "info"`, got["mod.level"])
	}
	if n := m.FieldCount(); n != 1 {
		t.Errorf("FieldCount() = %d，期望 1", n)
	}
}

// 匿名结构体的 reflect.Type.Name() 是空串。报错信息里不能出现「字段 .B」
// 这种指不出是哪个模块的写法——注册期 panic 是给人看的，得能定位。
func TestRegisterErrorNamesAnonymousStructReadably(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("应该 panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic 值应为 string，得到 %T", r)
		}
		if strings.Contains(msg, "字段 .") {
			t.Errorf("报错信息里没有结构体名，定位不到模块：%s", msg)
		}
		if !strings.Contains(msg, "Level") {
			t.Errorf("报错信息应指出是哪个字段：%s", msg)
		}
	}()
	s := struct{ Level string }{}
	NewManager().Register("x", &s)
}

// 多个模块各自的 key 不能互相污染。
func TestMultipleModulesAreIsolated(t *testing.T) {
	m := NewManager()
	a := struct {
		Level string `json:"level"`
	}{Level: "a"}
	b := struct {
		Level string `json:"level"`
	}{Level: "b"}
	m.Register("mod_a", &a)
	m.Register("mod_b", &b)

	if err := m.UpdateConfig("mod_a.level", `"changed"`); err != nil {
		t.Fatal(err)
	}
	if a.Level != "changed" {
		t.Errorf("mod_a.Level = %q，期望 changed", a.Level)
	}
	if b.Level != "b" {
		t.Errorf("mod_b 被误改：Level = %q", b.Level)
	}
	if v, _ := m.GetConfig("mod_b.level"); v != `"b"` {
		t.Errorf("GetConfig(mod_b.level) = %q，期望 \"b\"", v)
	}
	if v, ok := m.GetConfig("mod_missing.level"); ok {
		t.Errorf("未注册模块不该返回 ok，得到 %q", v)
	}
}

// 注册的名字出现在错误信息里，方便定位是谁注册错了。
func TestRegisterErrorMentionsField(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("应该 panic")
		}
		msg := r.(string)
		if !strings.Contains(msg, "json tag") {
			t.Errorf("panic 信息应说明缺少 json tag，实际：%s", msg)
		}
	}()
	s := struct{ Level string }{}
	NewManager().Register("x", &s)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/setting/config/`
Expected: FAIL — `undefined: NewManager`。

- [ ] **Step 3: 写实现**

创建 `internal/setting/config/config.go`：

```go
// Package config 是反射驱动的配置注册表。
//
// 需求是：几百个配置项，要能在运行时热更新、能通过控制台修改、能多节点同步，
// 还要有类型与校验。常见做法是 yaml 文件 + 手写解析 + 手写校验 + 手写默认值
// + 手写文档，五份东西要同步维护，必然漂移。
//
// 这里把五份合成一份：
//
//	类型       ← Go 结构体字段类型
//	默认值     ← 变量初始值
//	持久化 key ← 模块名 + json tag（自动生成，不会手写错）
//	文档       ← 结构体与字段注释
//
// 注册发生在 init() 期，所以任何用法错误都直接 panic：注册错误属于开发期问题，
// 越早炸越好。
package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/yyyCode/whats-api/internal/common"
)

// typeLabel 给匿名结构体一个可读的名字：elem.Type().Name() 对它返回空串，
// 报错信息就成了「字段 .B」，看不出是哪个模块出的问题。
func typeLabel(t reflect.Type) string {
	if n := t.Name(); n != "" {
		return n
	}
	return t.String()
}

// 支持的字段类型（M0）。扩展这里的同时要扩展 setField 与 encodeField。
var supportedKinds = map[reflect.Kind]bool{
	reflect.String:  true,
	reflect.Bool:    true,
	reflect.Int:     true,
	reflect.Int64:   true,
	reflect.Float64: true,
}

type field struct {
	jsonName string
	index    int
}

type module struct {
	value   reflect.Value // 注册时传入的结构体指针
	fields  map[string]field
	rebuild []func()
}

// Manager 是配置注册表本体。
type Manager struct {
	mu      sync.RWMutex
	modules map[string]*module
}

// GlobalConfig 是进程级单例：配置域在各自的 init() 里往它注册。
// 这是"进程级单例"，只在注册期写入，之后只读。
var GlobalConfig = NewManager()

func NewManager() *Manager {
	return &Manager{modules: map[string]*module{}}
}

// Register 注册一个配置域，必须在 init() 中调用。
//
// 违反约定一律 panic：模块名为空/重复、不是非 nil 结构体指针、
// 导出字段缺 json tag、json tag 重复、字段类型不支持。
func (m *Manager) Register(name string, cfg any) {
	if name == "" {
		panic("config: 模块名不能为空")
	}

	if strings.Contains(name, ".") {
		panic(fmt.Sprintf("config: 模块名 %q 不能含 '.'——配置 key 是「模块名.json tag」，"+
			"名字里的点会让 key 拆不开，不同模块会撞进同一个 key", name))
	}

	rv := reflect.ValueOf(cfg)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic(fmt.Sprintf("config: Register(%q) 需要非 nil 的结构体指针，得到 %T", name, cfg))
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		panic(fmt.Sprintf("config: Register(%q) 需要指向结构体的指针，得到 %s", name, elem.Kind()))
	}

	fields := make(map[string]field, elem.NumField())
	for i := 0; i < elem.NumField(); i++ {
		sf := elem.Type().Field(i)
		if !sf.IsExported() {
			continue // 未导出字段不参与配置（也不报错：它可能只是内部状态）
		}

		rawTag := sf.Tag.Get("json")
		// json:"-" 是 Go 里"明确排除该字段"的写法（json:"-," 才是名为 "-" 的字段），
		// 不是漏写 tag，所以按未导出字段同样跳过而不是报错。
		if rawTag == "-" {
			continue
		}
		jsonName := strings.Split(rawTag, ",")[0]
		if jsonName == "" {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s.%s 缺少 json tag——"+
				"配置 key 由「模块名.json tag」生成，缺 tag 会让持久化 key 漂移",
				name, typeLabel(elem.Type()), sf.Name))
		}
		if strings.Contains(jsonName, ".") {
			panic(fmt.Sprintf("config: 模块 %q 的 json tag %q 不能含 '.'——"+
				"它就是 key 的分隔符", name, jsonName))
		}
		if _, dup := fields[jsonName]; dup {
			panic(fmt.Sprintf("config: 模块 %q 的 json tag %q 重复", name, jsonName))
		}
		if !supportedKinds[sf.Type.Kind()] {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s 类型 %s 不支持"+
				"（M0 支持 string/bool/int/int64/float64）", name, sf.Name, sf.Type.Kind()))
		}
		// 非有限的浮点默认值必须在注册期就炸：json.Marshal 编不出 NaN/±Inf，
		// MustMarshalString 会返回空串，于是 ExportAllConfigs（Option 表默认值的
		// 来源）里出现一个不是 JSON 的 ""，等别处解析才报错，现场已经远离原因。
		if sf.Type.Kind() == reflect.Float64 {
			if f := elem.Field(i).Float(); math.IsNaN(f) || math.IsInf(f, 0) {
				panic(fmt.Sprintf("config: 模块 %q 的字段 %s 默认值是 %v，无法序列化成 JSON"+
					"（NaN/±Inf 不是合法 JSON 数值）", name, sf.Name, f))
			}
		}
		fields[jsonName] = field{jsonName: jsonName, index: i}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.modules[name]; dup {
		panic(fmt.Sprintf("config: 模块 %q 重复注册", name))
	}
	m.modules[name] = &module{value: rv, fields: fields}
}

// OnRebuild 注册该域的快照重建回调，由 UpdateConfig 在写锁内调用。
//
// 回调里应该做「值拷贝 + atomic.Pointer.Store」：于是热路径读到的永远是一个
// 完整一致的版本，而不是正在被反射写入的中间态。
func (m *Manager) OnRebuild(name string, hook func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.modules[name]
	if !ok {
		panic(fmt.Sprintf("config: OnRebuild(%q) 的模块尚未注册", name))
	}
	mod.rebuild = append(mod.rebuild, hook)
}

// ModuleKey 生成持久化 key：<模块名>.<jsonField>。
func ModuleKey(moduleName, jsonName string) string { return moduleName + "." + jsonName }

// ExportAllConfigs 返回所有注册域的**当前值**（初始化阶段即默认值），
// key = <module>.<jsonField>，value = JSON 编码。
func (m *Manager) ExportAllConfigs() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]string, m.fieldCountLocked())
	for name, mod := range m.modules {
		elem := mod.value.Elem()
		for jsonName, f := range mod.fields {
			out[ModuleKey(name, jsonName)] = encodeField(elem.Field(f.index))
		}
	}
	return out
}

// lookupLocked 在持锁状态下解析 key。
//
// ★ 调用约定：必须在持锁时调用。它返回模块元数据与字段下标，
// 而**写入**由调用方自己做——ValidateConfig 持读锁（它不写真实字段），
// UpdateConfig 持写锁（它要写）。把这两个锁语义混在一个函数里，
// 迟早会出现"持读锁却写字段"的并发 bug。
func (m *Manager) lookupLocked(key string) (*module, field, error) {
	modName, jsonName, ok := splitKey(key)
	if !ok {
		return nil, field{}, fmt.Errorf("配置 key %q 格式非法，应为 <module>.<field>", key)
	}
	mod, ok := m.modules[modName]
	if !ok {
		return nil, field{}, fmt.Errorf("未注册的配置模块 %q", modName)
	}
	f, ok := mod.fields[jsonName]
	if !ok {
		return nil, field{}, fmt.Errorf("模块 %q 没有配置项 %q", modName, jsonName)
	}
	return mod, f, nil
}

// GetConfig 返回单个配置项的当前值（JSON 编码）。
func (m *Manager) GetConfig(key string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return "", false
	}
	return encodeField(mod.value.Elem().Field(f.index)), true
}

// ValidateConfig 校验一个新值是否可接受，但**不写入任何状态**。
//
// 为什么它在 UpdateConfig 之外单独存在：配置的真实写入路径是
// 校验 → 落库 → 改内存（见 model.UpdateOption）。校验必须先于落库——
// 否则一个非法值会先落进库里、再被别的实例同步过去，一处错变成处处错。
// 因此"校验"必须能独立于"写入"被调用。
//
// 它把值解析进一个 reflect.New(...).Elem() 临时副本，类型与真实字段**完全相同**，
// 因此不存在"校验通过但写入失败"的裂缝：能装进副本的值就能装进字段。
func (m *Manager) ValidateConfig(key, rawJSON string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return err
	}

	// 只读字段的 Type（在 RLock 下安全），写入目标是这份临时副本。
	scratch := reflect.New(mod.value.Elem().Field(f.index).Type()).Elem()
	if err := setField(scratch, rawJSON); err != nil {
		return fmt.Errorf("配置项 %q: %w", key, err)
	}
	return nil
}

// UpdateConfig 按 key 写回注册的结构体，并触发该域的快照重建。
//
// 未知 key、类型不符、数值越界都返回错误且**不写入**——
// 管理端的一次非法请求不能把运行中的配置改坏。
//
// 注意它不做"先校验再写入"的两段式：解析本身就是校验，
// 而 setField 保证失败时不部分写入。需要"先确认再落库"的调用方
// （如 model.UpdateOption）用 ValidateConfig 做前置校验。
func (m *Manager) UpdateConfig(key, rawJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mod, f, err := m.lookupLocked(key)
	if err != nil {
		return err
	}
	if err := setField(mod.value.Elem().Field(f.index), rawJSON); err != nil {
		return fmt.Errorf("配置项 %q: %w", key, err)
	}

	// 在写锁内重建快照：读者要么看到旧版本、要么看到新版本，不会看到中间态。
	for _, hook := range mod.rebuild {
		hook()
	}
	return nil
}

// Modules 返回已注册的模块名（已排序），供 /api/status 展示。
func (m *Manager) Modules() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.modules))
	for name := range m.modules {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FieldCount 返回注册的配置项总数。
func (m *Manager) FieldCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fieldCountLocked()
}

func (m *Manager) fieldCountLocked() int {
	n := 0
	for _, mod := range m.modules {
		n += len(mod.fields)
	}
	return n
}

// splitKey 把 "module.field" 拆开。注意字段名本身可能含点（json tag 允许），
// 所以按**第一个**点切分。
func splitKey(key string) (moduleName, jsonName string, ok bool) {
	idx := strings.Index(key, ".")
	if idx <= 0 || idx == len(key)-1 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}

func encodeField(fv reflect.Value) string {
	switch fv.Kind() {
	case reflect.String:
		return common.MustMarshalString(fv.String())
	case reflect.Bool:
		return common.MustMarshalString(fv.Bool())
	case reflect.Int, reflect.Int64:
		return common.MustMarshalString(fv.Int())
	case reflect.Float64:
		return common.MustMarshalString(fv.Float())
	default:
		return ""
	}
}

// setField 把 JSON 文本写进字段。类型不符返回错误，绝不部分写入。
func setField(fv reflect.Value, rawJSON string) error {
	if !fv.CanSet() {
		return errors.New("字段不可写")
	}

	// null 必须显式拒掉：json.Unmarshal 对 null 不报错，而下面每种类型都是
	// 先解析到零值局部变量再赋值——于是 null 会把配置静默改成零值
	// （sync_frequency 60 → 0 这种）。管理端的一次非法请求不该悄悄改坏配置。
	// 将来若支持指针类型字段（null 表示"未设置"），要在这里放行。
	if strings.TrimSpace(rawJSON) == "null" {
		return errors.New("null 不是合法取值")
	}

	switch fv.Kind() {
	case reflect.String:
		var v string
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望字符串: %w", err)
		}
		fv.SetString(v)

	case reflect.Bool:
		var v bool
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望布尔值: %w", err)
		}
		fv.SetBool(v)

	case reflect.Int, reflect.Int64:
		var v int64
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望整数: %w", err)
		}
		// 32 位构建上 int 是 int32，SetInt 越界会直接 panic，
		// 所以先查一次。amd64 上这个分支不可达（int 为 64 位），
		// 属于防御性代码，测试不覆盖也不假装覆盖。
		if fv.OverflowInt(v) {
			return fmt.Errorf("整数 %d 超出 %s 的表示范围", v, fv.Kind())
		}
		fv.SetInt(v)

	case reflect.Float64:
		var v float64
		if err := common.UnmarshalString(rawJSON, &v); err != nil {
			return fmt.Errorf("期望浮点数: %w", err)
		}
		if fv.OverflowFloat(v) {
			return fmt.Errorf("浮点数 %v 超出 %s 的表示范围", v, fv.Kind())
		}
		fv.SetFloat(v)

	default:
		return fmt.Errorf("不支持的字段类型 %s", fv.Kind())
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/setting/config/ -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/setting/config/
git commit -m "feat(setting): 反射驱动的配置注册表

结构体 + json tag + Register 同时定义类型、默认值、持久化 key 与文档；
非法写入报错且不留副作用；快照重建回调在写锁内触发。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 9: 第一个配置域与原子快照（`internal/setting/system_setting`）

**Files:**
- Create: `internal/setting/system_setting/system_setting.go`
- Test: `internal/setting/system_setting/system_setting_test.go`

**这里是设计文档对上游架构的唯一一处加固，值得理解清楚：**

上游的写法是"反射把值写进注册的结构体，业务代码直接读那个结构体"。这在并发下是 data race——
配置更新正在反射写 `LogLevel` 字符串头的时候，访问日志中间件可能正在读它。

本项目的做法：注册的结构体只是**反射写入的目标**，业务代码一律读 `Get()` 返回的
`atomic.Pointer` 快照。`OnRebuild` 回调在写锁内做值拷贝 + 原子替换，于是：

- 读侧完全无锁（`atomic.Pointer.Load`）；
- 读者要么看到旧版本、要么看到新版本，**不可能读到中间态**；
- 写侧仍然只有一条路径（`config.Manager`）。

- [ ] **Step 1: 写失败的测试**

创建 `internal/setting/system_setting/system_setting_test.go`：

```go
package system_setting

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

func TestDefaults(t *testing.T) {
	got := Get()
	if got.LogLevel != "info" {
		t.Errorf("LogLevel = %q，期望 info", got.LogLevel)
	}
	if !got.EnableAccessLog {
		t.Error("EnableAccessLog 默认应为 true")
	}
	if got.SlowRequestThresholdSeconds != 3 {
		t.Errorf("SlowRequestThresholdSeconds = %v，期望 3", got.SlowRequestThresholdSeconds)
	}
	if got.StatusMessage == "" {
		t.Error("StatusMessage 默认不应为空")
	}
}

// 注册表里必须能导出这一域的默认值——这是 Option 表默认值的来源。
func TestRegisteredInGlobalConfig(t *testing.T) {
	all := config.GlobalConfig.ExportAllConfigs()
	for _, key := range []string{
		"system_setting.log_level",
		"system_setting.status_message",
		"system_setting.enable_access_log",
		"system_setting.slow_request_threshold_seconds",
	} {
		if _, ok := all[key]; !ok {
			t.Errorf("缺少配置项 %q；现有：%v", key, all)
		}
	}
}

// 改配置后热路径必须立刻看到新值（验收标准③的单元测试那一半）。
func TestUpdatePublishesNewSnapshot(t *testing.T) {
	before := Get()

	if err := config.GlobalConfig.UpdateConfig("system_setting.status_message", `"改过了"`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}

	after := Get()
	if after.StatusMessage != "改过了" {
		t.Errorf("StatusMessage = %q，期望 改过了", after.StatusMessage)
	}
	// 快照必须是**新对象**：旧快照不可被就地修改，否则读侧就白做了。
	if before == after {
		t.Fatal("更新后应发布新的快照指针，而不是就地改旧对象")
	}
	if before.StatusMessage == "改过了" {
		t.Error("旧快照被就地修改了——不可变快照的约定被破坏")
	}
	if after.EnableAccessLog != Default.EnableAccessLog {
		t.Error("未修改的字段不应被连带改动")
	}

	// 恢复默认值，避免污染同包其他测试（TestConcurrentReadDuringUpdates
	// 只接受默认值或 msg-N，见 plan 里同款清理约定）。
	if err := config.GlobalConfig.UpdateConfig(
		"system_setting.status_message", strconv.Quote(Default.StatusMessage)); err != nil {
		t.Fatal(err)
	}
}

// 布尔与浮点也要能热更新。
func TestUpdateBooleanAndFloat(t *testing.T) {
	if err := config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `false`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if Get().EnableAccessLog {
		t.Error("EnableAccessLog 应为 false")
	}

	if err := config.GlobalConfig.UpdateConfig("system_setting.slow_request_threshold_seconds", `1.25`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if got := Get().SlowRequestThresholdSeconds; got != 1.25 {
		t.Errorf("慢请求阈值 = %v，期望 1.25", got)
	}

	// 恢复默认值，避免影响同包其他测试与后续断言
	if err := config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `true`); err != nil {
		t.Fatal(err)
	}
	if err := config.GlobalConfig.UpdateConfig("system_setting.slow_request_threshold_seconds", `3`); err != nil {
		t.Fatal(err)
	}
}

// 这条测试是原子快照契约的核心：并发读 + 并发写，且必须用 -race 跑。
//
// 断言的不是"没有 race"（那是 -race 的活），而是"读者读到的永远是已发布的完整版本"：
// 任何一次读到的 status_message 都必须是默认值或某个完整写过的 msg-N，
// 绝不允许出现空串或半截字符串。
func TestConcurrentReadDuringUpdates(t *testing.T) {
	const iterations = 200
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			msg := strconv.Quote(fmt.Sprintf("msg-%d", i))
			if err := config.GlobalConfig.UpdateConfig("system_setting.status_message", msg); err != nil {
				t.Errorf("UpdateConfig 失败: %v", err)
				return
			}
		}
	}()

	isPublished := func(s string) bool {
		return s == Default.StatusMessage || strings.HasPrefix(s, "msg-")
	}
	for i := 0; i < iterations; i++ {
		snap := Get()
		if snap == nil {
			t.Fatal("快照不应为 nil")
		}
		if !isPublished(snap.StatusMessage) {
			t.Fatalf("读到未发布的值 %q，说明存在中间态可见", snap.StatusMessage)
		}
	}

	<-done

	// 收尾：恢复默认，避免污染后续测试
	if err := config.GlobalConfig.UpdateConfig(
		"system_setting.status_message", strconv.Quote(Default.StatusMessage)); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/setting/system_setting/`
Expected: FAIL — `undefined: Get`。

- [ ] **Step 3: 写实现**

创建 `internal/setting/system_setting/system_setting.go`：

```go
// Package system_setting 是 M0 唯一的配置域。
//
// 配置域按「变更的人是谁」划分，而不是按技术分层：这个域放的是运维会改的运行时开关。
// 将来 billing_setting 归财务、model_setting 归模型负责人——分域即权限边界。
package system_setting

import (
	"sync/atomic"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

// SystemSetting 是运维可热更新的运行时开关。
//
// 这一个结构体同时承担四件事：类型（字段类型）、默认值（Default）、
// 持久化 key（模块名 + json tag）、文档（字段注释）。
// 加一个配置项只需要加一个字段——不需要改 schema、校验、默认值或文档的第二份定义。
type SystemSetting struct {
	// LogLevel 日志级别：debug / info / warn / error。改完立即生效。
	LogLevel string `json:"log_level"`

	// StatusMessage 会出现在 /api/status 里。
	// 它的存在意义是给验收标准③一个可观察点：改它能在不重启的情况下被 HTTP 看到。
	StatusMessage string `json:"status_message"`

	// EnableAccessLog 关掉后不再输出访问日志（请求计数仍然继续统计）。
	EnableAccessLog bool `json:"enable_access_log"`

	// SlowRequestThresholdSeconds 访问日志里慢请求的判定阈值（秒）。
	SlowRequestThresholdSeconds float64 `json:"slow_request_threshold_seconds"`
}

// Default 是本域的默认值。
//
// 默认值写在代码里，不写进数据库、也不写进 GORM tag：
// 只有"管理员显式改过"的项才落库，这样将来调默认值能影响所有未显式修改的实例。
var Default = SystemSetting{
	LogLevel:                    "info",
	StatusMessage:               "M0 技术骨架运行中",
	EnableAccessLog:             true,
	SlowRequestThresholdSeconds: 3,
}

// registered 是反射写入的目标，只允许配置系统访问。
// 业务代码一律用 Get()——直接读它会读到正在被写入的中间态。
var registered = Default

// current 是热路径读的不可变快照。
var current atomic.Pointer[SystemSetting]

func init() {
	d := Default
	current.Store(&d)

	config.GlobalConfig.Register("system_setting", &registered)
	config.GlobalConfig.OnRebuild("system_setting", func() {
		// 本回调由 config.Manager 在写锁内调用，所以这里读 registered 没有并发写。
		// 值拷贝 + 原子替换：读者从此只会看到完整版本。
		snap := registered
		current.Store(&snap)
	})
}

// Get 返回当前快照。返回值是共享的只读对象，**不要修改它的字段**。
func Get() *SystemSetting { return current.Load() }
```

- [ ] **Step 4: 运行测试确认通过（含 race 检测）**

Run: `go test -race ./internal/setting/system_setting/ -v`
Expected: PASS，且没有 race 报告。

Run: `go test -race ./...`
Expected: 全绿。

- [ ] **Step 5: 确认依赖方向没被破坏**

Run: `go list -deps ./internal/common/ | grep whats-api`
Expected: `internal/common`、`internal/logger`。
`common` 只允许依赖 logger/constant 这两个叶子，出现 `setting`、`cachex`、`model` 等上层包就是成环了。

Run: `go list -deps ./internal/setting/config/ | grep whats-api`
Expected: `internal/logger`、`internal/common`、`internal/setting/config`（logger 经 common 传递依赖而来）。

Run: `go list -deps ./internal/cachex/ | grep whats-api`
Expected: `internal/cachex`、`internal/common`、`internal/logger`。

- [ ] **Step 6: Commit**

```bash
git add internal/setting/system_setting/
git commit -m "feat(setting): 第一个配置域与原子快照读

注册结构体只作反射写入目标，业务代码读 atomic.Pointer 快照，
使热路径读无锁且不会读到中间态。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Task 10: 基础层收尾——整理依赖与全量验证

**Files:**
- Modify: `go.mod`、`go.sum`（由 `go mod tidy` 整理）

- [ ] **Step 1: 整理依赖**

Run: `go mod tidy`
Expected: 无报错。此时 `go.mod` 里只剩基础层真正 import 的依赖
（`shopspring/decimal`、`go-redis/redis/v8`，以及它们的传递依赖）。

- [ ] **Step 2: 全量验证**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: 三条命令全部通过，零输出错误。

- [ ] **Step 3: 检查 go.mod 的 go directive**

Run: `head -5 go.mod`
Expected: 包含 `go 1.23.0`（工具链可以是 `toolchain go1.23.2`）。若 `go mod tidy` 把 `go` 行抬高了，
手工改回 `go 1.23.0` 并重跑 Step 2。

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: 整理基础层依赖

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 基础层完成的标志

```
□ go build ./...      通过
□ go vet ./...        通过
□ go test -race ./... 全绿
□ internal/logger 与 internal/constant 不依赖本项目其他包；internal/common 只依赖 internal/logger
  （internal/constant 是给 model 层用的持久化枚举，common 不引用它）
□ 配置项的默认值可由 config.GlobalConfig.ExportAllConfigs() 导出
□ 改一个配置项，system_setting.Get() 立即返回新快照（且旧快照未被就地修改）
□ Redis 未配置/连不上时 counter.Backend() == "memory"
```

**下一步**：`2026-10-12-m0-runtime.md` —— 数据层（三库兼容、七张表、幂等迁移、Option 写入路径）、
HTTP 层（中间件链、路由分组、`/api/status`）、启动与优雅停机、交付物。
