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
| `go.mod` | 模块定义，`go 1.23` |
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
Expected: 生成 `go.mod`，内容为 `module github.com/yyyCode/whats-api` 与 `go 1.23`。
如果它写的 go directive 高于 1.23（取决于本机 go 版本，本机是 1.23.2，所以应是 `go 1.23`），
手工改成 `go 1.23` 再继续。

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
	InitEnv()

	if SyncFrequency != MinSyncFrequencySeconds || len(EnvWarnings) != 0 {
		t.Errorf("恰好等于下限不应告警：sync=%d warnings=%v", SyncFrequency, EnvWarnings)
	}
}

// 非法数值不致命：回退默认值继续启动，而不是启动不了。
func TestInitEnvFallsBackOnInvalidNumbers(t *testing.T) {
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
// 它只依赖 internal/logger 与 internal/constant——两个只依赖标准库的叶子包。
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
```

- [ ] **Step 4: 写 `env.go`**

创建 `internal/common/env.go`：

```go
package common

import (
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

	Port = envInt("PORT", DefaultPort)
	GinMode = envStr("GIN_MODE", "release")
	CORSAllowOrigins = envList("CORS_ALLOW_ORIGINS", DefaultCORSAllowOrigins)

	SQLDSN = envRaw("SQL_DSN")
	LogSQLDSN = envRaw("LOG_SQL_DSN")
	RedisConnString = envRaw("REDIS_CONN_STRING")

	NodeType = envStr("NODE_TYPE", DefaultNodeType)
	NodeName = envRaw("NODE_NAME")
	IsMasterNode = NodeType != "slave"

	SyncFrequency = envInt("SYNC_FREQUENCY", DefaultSyncFrequencySeconds)
	if SyncFrequency < MinSyncFrequencySeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SYNC_FREQUENCY=%d 低于下限 %d 秒，已抬到下限", SyncFrequency, MinSyncFrequencySeconds))
		SyncFrequency = MinSyncFrequencySeconds
	}

	ShutdownTimeout = envInt("SHUTDOWN_TIMEOUT_SECONDS", DefaultShutdownTimeoutSeconds)
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
// 一个写错的端口不该让服务完全起不来，但也不该被静默接受成 0。
func envInt(key string, def int) int {
	v := envRaw(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envList 读取逗号分隔的列表，逐项去空白，空项丢弃。
func envList(key, def string) []string {
	raw := envStr(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{def}
	}
	return out
}
```

`fmt` 是 `EnvWarnings` 那条 append 用到的，记得加进 import：

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
Expected: PASS，五个测试全通过。

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
	v, _ := clampQuota(math.Round(f))
	return v
}

func QuotaRoundChecked(f float64) (int, QuotaClamp) {
	return clampQuota(math.Round(f))
}

func QuotaRoundStrict(f float64) (int, error) {
	return QuotaFromFloatStrict(math.Round(f))
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
		return 0, fmt.Errorf("解析金额 %q 失败: %w", s, err)
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
	if d.IsZero() {
		return 0, ClampNone
	}
	rounded := d.Round(0) // shopspring 的 Round 是 half away from zero
	if rounded.IsNegative() {
		return 0, ClampUnderflow
	}
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
	"errors"
	"testing"
)

// Redis 未启用时，底层函数必须明确报错，而不是空指针崩掉或静默返回零值——
// 静默返回零值会让上层"降级"变成"数据丢失"。
func TestRedisFunctionsFailClearlyWhenDisabled(t *testing.T) {
	RedisEnabled = false
	RDB = nil

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
func TestInitRedisClientWithoutConfigDegrades(t *testing.T) {
	RedisEnabled = true // 故意留下脏状态，验证 Init 会把它清掉
	RedisConnString = ""
	t.Cleanup(func() { RedisEnabled = false; RDB = nil })

	InitRedisClient()

	if RedisEnabled {
		t.Error("未配置 REDIS_CONN_STRING 时 RedisEnabled 必须为 false")
	}
	if RDB != nil {
		t.Error("未配置时不应建立连接")
	}
}

// 配置了一个连不上的地址：必须降级而不是启动失败——这是第二条验收标准。
func TestInitRedisClientUnreachableDegrades(t *testing.T) {
	RedisConnString = "127.0.0.1:1" // 保留端口，必然连不上
	t.Cleanup(func() { RedisEnabled = false; RDB = nil; RedisConnString = "" })

	InitRedisClient()

	if RedisEnabled {
		t.Error("连接失败时 RedisEnabled 必须为 false")
	}
	if RDB != nil {
		t.Error("连接失败时不应留下 client")
	}
}

func TestNormalizeRedisURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"redis://127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"rediss://host:6380", "rediss://host:6380"},
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

func TestCounterUsesMemoryBackendWhenRedisDisabled(t *testing.T) {
	common.RedisEnabled = false
	common.RDB = nil

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
	common.RedisEnabled = false
	common.RDB = nil

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
	common.RedisEnabled = false

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
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/yyyCode/whats-api/internal/logger"
)

// ErrRedisDisabled 表示 Redis 未启用（未配置，或配置了但连不上）。
// 调用方据此走进程内缓存——这正是"拿掉 Redis 也能启动"的实现方式。
var ErrRedisDisabled = errors.New("redis 未启用")

var (
	// RedisEnabled 为 true 时 RDB 可用。所有使用方都必须先看这个标志。
	RedisEnabled bool
	// RDB 在未启用时为 nil。
	RDB *redis.Client
)

// InitRedisClient 初始化 Redis。**非致命**：
//
//	未配置 REDIS_CONN_STRING → 直接用进程内缓存
//	配置了但解析失败/连不上 → 告警并降级为进程内缓存
//
// 两种失败都不阻断启动：缓存是加速手段，不该成为可用性的单点。
func InitRedisClient() {
	RedisEnabled = false
	if RDB != nil {
		_ = RDB.Close()
		RDB = nil
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

	RDB = client
	RedisEnabled = true
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

func CloseRedis() error {
	if RDB == nil {
		return nil
	}
	return RDB.Close()
}

func RedisGet(key string) (string, error) {
	if !RedisEnabled || RDB == nil {
		return "", ErrRedisDisabled
	}
	return RDB.Get(context.Background(), key).Result()
}

// RedisSet 写入键值并设 TTL。ttl <= 0 表示不过期。
func RedisSet(key, value string, ttl time.Duration) error {
	if !RedisEnabled || RDB == nil {
		return ErrRedisDisabled
	}
	return RDB.Set(context.Background(), key, value, ttl).Err()
}

func RedisDel(key string) error {
	if !RedisEnabled || RDB == nil {
		return ErrRedisDisabled
	}
	return RDB.Del(context.Background(), key).Err()
}

// RedisIncr 原子自增，并刷新 TTL（滑动窗口）。
// 走 pipeline 是为了让 INCR 与 EXPIRE 一次往返完成，避免"自增了但没设过期"的中间态。
func RedisIncr(key string, ttl time.Duration) (int64, error) {
	if !RedisEnabled || RDB == nil {
		return 0, ErrRedisDisabled
	}
	ctx := context.Background()
	pipe := RDB.TxPipeline()
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
func (c *Counter) useRedis() bool { return common.RedisEnabled && common.RDB != nil }

func (c *Counter) key(key string) string { return c.namespace + ":" + key }

// Backend 报告当前实际后端，供 /api/status 与运维排查使用。
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
func (c *Counter) Get(key string) (int64, bool) {
	if c.useRedis() {
		raw, err := common.RedisGet(c.key(key))
		if err != nil {
			return 0, false
		}
		v, err := parseCounter(raw)
		if err != nil {
			return 0, false
		}
		return v, true
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
		{"json tag 重复", func() {
			s := struct {
				A string `json:"same"`
				B string `json:"same"`
			}{}
			NewManager().Register("x", &s)
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
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/yyyCode/whats-api/internal/common"
)

// 支持的字段类型（M0）。扩展这里的同时要扩展 setField 与 fieldOf。
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

		jsonName := strings.Split(sf.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s.%s 缺少 json tag——"+
				"配置 key 由「模块名.json tag」生成，缺 tag 会让持久化 key 漂移",
				name, elem.Type().Name(), sf.Name))
		}
		if _, dup := fields[jsonName]; dup {
			panic(fmt.Sprintf("config: 模块 %q 的 json tag %q 重复", name, jsonName))
		}
		if !supportedKinds[sf.Type.Kind()] {
			panic(fmt.Sprintf("config: 模块 %q 的字段 %s 类型 %s 不支持"+
				"（M0 支持 string/bool/int/int64/float64）", name, sf.Name, sf.Type.Kind()))
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
//   而**写入**由调用方自己做——ValidateConfig 持读锁（它不写真实字段），
//   UpdateConfig 持写锁（它要写）。把这两个锁语义混在一个函数里，
//   迟早会出现"持读锁却写字段"的并发 bug。
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
Expected: `internal/common`、`internal/setting/config`。

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
Expected: 包含 `go 1.23`（或 1.23.x）。若 `go mod tidy` 把它抬高了，手工改回 `go 1.23` 并重跑 Step 2。

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
□ internal/logger、internal/constant 不依赖本项目其他包；internal/common 只依赖这两个叶子
□ 配置项的默认值可由 config.GlobalConfig.ExportAllConfigs() 导出
□ 改一个配置项，system_setting.Get() 立即返回新快照（且旧快照未被就地修改）
□ Redis 未配置/连不上时 counter.Backend() == "memory"
```

**下一步**：`2026-10-12-m0-runtime.md` —— 数据层（三库兼容、七张表、幂等迁移、Option 写入路径）、
HTTP 层（中间件链、路由分组、`/api/status`）、启动与优雅停机、交付物。
