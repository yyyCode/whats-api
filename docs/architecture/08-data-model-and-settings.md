# 08 · 数据模型、多数据库兼容与配置系统

> 一个自托管项目最大的工程约束是"用户想用什么数据库就用什么数据库"。这直接决定了很多写法。本篇讲模型的整体形状、三库兼容的实战规则，以及本项目最巧妙的一处设计——反射驱动的配置系统。

## 1. 模型总览

按业务域分组（不是按文件顺序）：

### 身份与权限
| 模型 | 文件 | 说明 |
|---|---|---|
| `User` | `model/user.go:79` | 用户名、密码、角色、状态、额度、已用额度、分组、邮箱、邀请码、`AuthVersion`（会话失效版本） |
| `Token` | `model/token.go:14` | 转发 API Key（见 07 篇） |
| `UserAccessToken` | `model/user_access_token.go:41` | 控制台 PAT（`nap_`，只存哈希） |
| `UserSession` | `model/user_session.go:42` | 服务端会话控制平面 |
| `AuthFlow` | `model/auth_flow.go:45` | 一次性认证仪式状态（OAuth/WebAuthn） |
| `ExternalIdentityClaim` | `model/external_identity_claim.go:21` | 外部身份声明 |
| `UserOAuthBinding` / `CustomOAuthProvider` | `user_oauth_binding.go` / `custom_oauth_provider.go` | 绑定与自定义提供方 |
| `TwoFA` / `TwoFABackupCode` / `PasskeyCredential` | `twofa.go` / `passkey.go` | 二次因子 |
| `CasbinRule` / `AuthzRole` | `casbin_rule.go` / `authz_role.go` | 授权策略与角色 |

### 资源与路由
| 模型 | 说明 |
|---|---|
| `Channel` | 上游渠道（见 05 篇） |
| `Ability` | **(group, model, channel) 反范式选路索引**（见 05 篇） |
| `Model` / `Vendor` / `PrefillGroup` | 模型目录元数据、供应商目录、分组预设 |

### 计费与账务
| 模型 | 说明 |
|---|---|
| `Log` | 消费/充值/管理/系统/错误/退款/登录记录 |
| `Redemption` | 兑换码（唯一 key、额度、过期时间、使用者） |
| `TopUp` | 支付订单（唯一交易号、金额、渠道、状态） |
| `SubscriptionOrder` / `UserSubscription` / `SubscriptionPlan` / `SubscriptionPreConsumeRecord` | 订阅套餐链路 |
| `QuotaData` | 看板/排行榜的聚合统计 |

### 异步与运维
| 模型 | 说明 |
|---|---|
| `Task` | 异步生成任务（平台、动作、状态、进度、`PrivateData`、`Data`） |
| `Midjourney` | MJ 任务记录 |
| `TaskPlugin` | JS 任务插件（key+version 唯一、来源、哈希、启用、激活） |
| `SystemInstance` | 多实例存活登记 |
| `SystemTask` / `SystemTaskLock` | 后台任务与 **DB 租约**（见 09 篇） |
| `PerfMetric` | 性能指标 |
| `AuditLog` | 管理/安全审计（独立迁移 `MigrateAuditLogs`） |
| `Option` / `Setup` / `Checkin` | 配置、初始化标记、签到 |

**观察：`Log` 和 `AuditLog` 是两类不同的东西。** 消费日志是**账务凭证**（面向用户可见、驱动计费），审计日志是**安全凭证**（面向管理员、不可篡改、记录谁改了什么）。不要合并成一张表——它们的保留策略、索引需求、脱敏要求完全不同。

## 2. 多数据库兼容（三库同时支持）

这是本项目最重的工程约束：**所有数据库代码必须同时在 SQLite、MySQL ≥ 5.7.8、PostgreSQL ≥ 9.6 上工作。** 项目规范（`AGENTS.md`）要求：任何可能影响数据库行为的改动，**都必须用真实的三种数据库验证**，单元测试、mock、只看代码都不算。

### 驱动与选择

`model/main.go:140` 的 `chooseDB(envName, isLog)` 按 DSN 前缀选驱动：

| DSN 前缀 | 驱动 | 备注 |
|---|---|---|
| `postgres://` / `postgresql://` | `gorm.io/driver/postgres` | 强制 `PreferSimpleProtocol`（兼容 PgBouncer / Neon 连接池） |
| `clickhouse://` / `tcp://` / `http(s)://` | `gorm.io/driver/clickhouse` | **仅日志库允许**（主库直接拒绝） |
| `local...` | `github.com/glebarez/sqlite`（纯 Go，无 CGO） | 单机默认 |
| 其他 | `gorm.io/driver/mysql` | 自动加 `parseTime=true` |

### 六条实战规则（都是踩过坑总结出来的）

**① 标识符引用风格不同 → 统一封装**

```go
// PostgreSQL 用 "column"，MySQL/SQLite 用 `column`
commonGroupCol   // 处理保留字列 group
commonKeyCol     // 处理保留字列 key
commonTrueVal / commonFalseVal
```

**② `SELECT ... FOR UPDATE` 必须用共享 helper**

```go
lockForUpdate(tx)   // MySQL/PostgreSQL 发 FOR UPDATE；SQLite 跳过（不支持该语法）
```

项目专门注明：**不要用 GORM v1 时代的 `tx.Set("gorm:query_option", "FOR UPDATE")`** —— GORM v2 会**静默忽略**它，锁根本没加，代码看起来"有锁"实际没有。这是最阴险的一类 bug：并发保护看起来存在，实际缺失。

**③ GORM 布尔默认值是坑**

```go
// ❌ 避免：gorm:"default:true"
// MySQL 与 PostgreSQL 对布尔默认值的规范化方式不同，
// 会导致 AutoMigrate 在每次重启时反复发 ALTER TABLE
```

对策：**业务默认值在代码里设（构造函数/hook/请求归一化），不要写进 GORM tag。**

**④ SQLite 不支持 `ALTER COLUMN`**

迁移里必须写 `ALTER TABLE ... ADD COLUMN`，并在 `model/main.go` 里按数据库类型分支。

**⑤ 不要用数据库特有 JSON 类型**

没有 `TEXT` 回退的 JSON 列类型不能碰。

**⑥ 主键生成交给 GORM**

不要手写 `AUTO_INCREMENT` / `SERIAL` —— 三库方言不同，但 GORM 的模型约定能统一处理。

### 迁移策略

```go
func migrateDB() {
	DB.AutoMigrate( 所有模型... )              // 自动迁移（幂等）
	// 然后是「定向迁移」：AutoMigrate 表达不了的变更
	migrateTokenKeyUniqueness()
	migratePrefillGroupUniqueness()
	migrateSubscriptionPlanPriceAmount()
	migrateTokenModelLimitsToText()
	migrateOptionPrimaryKey()
	InitializeUserAuthVersions()
	EnsureLegacyAccessTokenRetireAt()
	ensureSubscriptionPlanTableSQLite()        // SQLite 特例
}
```

**三条关键决策：**

1. **迁移只在 master 节点执行**（`common.IsMasterNode`）。多实例同时 AutoMigrate 会互相打架。
2. **AutoMigrate + 定向迁移并存**。AutoMigrate 处理"加表/加列"，但改类型、加唯一约束、数据回填必须自己写。**别指望 AutoMigrate 处理一切。**
3. **迁移必须可重复执行（幂等）**，且要验证"新库初始化"和"从上一版本升级"两条路径都通过。

### 日志库分离

```go
InitDB()     // 主库（SQL_DSN）
InitLogDB()  // 日志库（LOG_SQL_DSN）；未配置时 LOG_DB = DB
migrateLOGDB()  // 独立迁移；ClickHouse 建 MergeTree 表 + 可选 TTL
```

ClickHouse 上的四处特化（都封在 `model/log.go` 里）：

| 问题 | 处理 |
|---|---|
| 没有自增 ID | `assignDisplayLogIds` 手动分配展示用 ID |
| 必须显式排序 | 固定 `created_at DESC, request_id DESC` |
| LIKE 语法/转义不同 | `sanitizeClickHouseLikePattern` |
| 删数据是 mutation | `ALTER TABLE ... DELETE ... SETTINGS mutations_sync = 1` + TTL 配置 |

**架构启示：** 日志表是"只增不改、按时间查、量极大"的负载，和业务表的"按 ID 查、量小、频繁更新"完全不同。**物理分离是必要的**，而且分离后适配层要做方言隔离——所以 ClickHouse 的怪癖只允许出现在日志模型文件里，不能泄漏到业务代码。

## 3. 配置系统（本项目最巧妙的一处设计）

需求：**几百个配置项，要在运行时热更新，要能通过控制台修改，要能多节点同步，还要有类型和校验。**

### 存储：一张 K/V 表

```go
type Option struct {
	Key   string `gorm:"primaryKey"`
	Value string
}
```

### 类型注册：反射驱动的 ConfigManager

```go
// setting/config/config.go:14
type ConfigManager struct {
	configs map[string]any
	mutex   sync.RWMutex
}
var GlobalConfig = NewConfigManager()
```

各配置域在自己的包里用 `init()` 注册结构体：

```go
// setting/operation_setting/monitor_setting.go:11
type MonitorSetting struct {
	AutoTestChannelEnabled bool    `json:"auto_test_channel_enabled"`
	AutoTestChannelMinutes float64 `json:"auto_test_channel_minutes"`
	ChannelTestMode        string  `json:"channel_test_mode"`
	ChannelTestConcurrency int     `json:"channel_test_concurrency"`
}

var monitorSetting = MonitorSetting{ ... 默认值 ... }

func init() {
	config.GlobalConfig.Register("monitor_setting", &monitorSetting)
}
```

**Key 的命名规则：`<module>.<jsonField>`**，例如 `monitor_setting.channel_test_concurrency`。**结构体标签即配置 schema，注册即声明，无需写第二份配置定义。**

这是本项目最值得抄的模式，它同时解决了四件事：

| 问题 | 解法 |
|---|---|
| 配置项的**类型**从哪来 | Go 结构体字段类型 |
| **默认值**从哪来 | 变量初始值 |
| **持久化 key** 怎么定 | 模块名 + json tag（自动，不会手写错） |
| **文档** 从哪来 | 结构体 + 注释 |

对比一下常见做法：yaml 文件 + 手写解析 + 手写校验 + 手写默认值 + 手写文档 —— 五份东西要同步维护。**反射注册把五份合成了一份。**

### 读写流程

```
启动：
  InitOptionMap()
    ├─ 从 common.* 全局变量初始化
    ├─ config.GlobalConfig.ExportAllConfigs()   ← 所有注册模块的默认值
    └─ loadOptionsFromDatabase()                ← DB 里的 Option 行覆盖默认值
                                                 经 updateOptionMap 分发

管理端改配置：
  controller/option.go:UpdateOption
    → model.UpdateOption
        ├─ validateOptionValue(...)            校验
        ├─ 特殊处理（request policy / passkey domain / model pricing）
        ├─ 持久化 Option 行
        └─ updateOptionMap(key, value)         ★ 同时更新内存与运行时全局变量

热更新（多节点同步）：
  go model.SyncOptions(SyncFrequency)          定时从 DB 重载
```

`updateOptionMap`（`model/option.go:345`）是**唯一的分发点**：

```go
func updateOptionMap(key, value string) {
	common.OptionMapRWMutex.Lock()
	common.OptionMap[key] = value
	common.OptionMapRWMutex.Unlock()

	handleConfigUpdate(key, value)   // 反射写入注册的 config 模块
	// 再一个大 switch 处理「非结构化的」运行时开关：
	// *Enabled、ChannelDisableThreshold、RetryTimes ...
}
```

**关键设计：内存 map 与运行时变量必须一起改。** 如果只改 map，代码里读的是全局变量，配置就不生效；如果只改变量，多节点同步就拿不到。**"配置只有一条写入路径"是热更新系统不出错的前提。**

### 原子快照：避免"读一半配置"

有一类配置是**相互关联**的（重试次数、重试状态码、禁用状态码、亲和性设置…）。如果分散读取，可能在更新的中间态读到不一致的组合。项目用原子快照解决：

```go
// model/request_policy.go:24
type RequestPolicySnapshot struct {
	Affinity        operation_setting.ChannelAffinitySetting
	RetryTimes      int
	RetryCodes      []operation_setting.StatusCodeRange
	DisableCodes    []operation_setting.StatusCodeRange
	DisableKeywords []string
	CheckText       bool
	TextKeywords    []string
	AutoDisable     bool
	Options         map[string]string
}

var requestPolicySnapshot atomic.Pointer[RequestPolicySnapshot]   // ★
```

每次写入时**整体重建**这个快照并原子替换指针。热路径读 `Load()` 拿到的永远是完整一致的版本。

**这是无锁读 + 一致性的标准解法**（immutable snapshot + atomic pointer swap），非常适合"读极多写极少"的配置场景。**比 RWMutex 更好的地方在于：读侧完全无锁、不会阻塞写、也绝不会读到中间态。**

### 配置的组织

```
setting/
├── config/              配置管理器本体
├── system_setting/      OIDC、Passkey、法务条款、抓取、制品
├── operation_setting/   监控、亲和性、支付、额度、状态码范围
├── ratio_setting/       模型/分组倍率
├── model_setting/       各模型族特有设置（claude/gemini/grok…）
├── billing_setting/     计费模式与表达式
├── console_setting/     控制台外观
├── performance_setting/ 性能
└── task_pricing_setting/ 任务计价
```

**按"变更的人是谁"来分域，而不是按技术分层。** 运营改 operation_setting，财务改 billing_setting，模型负责人改 model_setting —— 分域即权限边界。

## 4. 缓存层

### 两级 API

```go
// 底层：common/redis.go（go-redis v8）
RedisGet / RedisSet / RedisDel
RedisHSetObj / RedisHGetObj      结构体 ↔ hash（反射）
RedisIncr / RedisHIncrBy / RedisHSetField   保留 TTL 的 H 操作
RedisKeyCacheSeconds() = SyncFrequency

// 上层：pkg/cachex（Redis 与内存自动切换）
HybridCache[V]   // Redis 可用时用 Redis，否则用 samber/hot 的内存 LRU
Namespace        // key 前缀
ValueCodec[V]    // 值编解码（如 IntCodec）
```

`HybridCache` 的双模设计让"单机小内存部署"和"集群部署"用同一份代码，代价是必须把两种后端的语义差异（TTL 精度、原子性）封在内部。

### 缓存清单与失效模式

| 缓存对象 | 位置 | 失效方式 |
|---|---|---|
| 渠道选路索引 | 进程内 `model/channel_cache.go` | `InitChannelCache` 重建 / 变更时增量更新 / 定时 `SyncChannelCache` |
| Token（按 key） | Redis hash + **栅栏** | 变更时立栅栏 + 版本号（见 07 篇 §5） |
| User | Redis（`model/user_cache.go`） | 版本化 schema + `AuthVersion` |
| 配置 | 进程内 `OptionMap` | 定时 `SyncOptions` |
| 渠道亲和性 | `cachex.HybridCache[int]` | TTL |
| 定价目录 | 1 分钟 TTL + 显式 `InvalidatePricingCache` | 双检锁 |
| 磁盘缓存 | `common/disk_cache.go` | 启动时清理旧文件 |

### 两种失效模式 + 一个加锁顺序的教训

**显式失效**（写时生效）：`CacheUpdateChannel` / `CacheUpdateChannelStatus` / `InvalidatePricingCache`。
**TTL 刷新**（最终一致）：`SyncChannelCache` / `SyncOptions` / `authz.StartPolicySync`。

**两者都需要。** 显式失效保证单节点立刻正确，TTL 刷新保证多节点最终收敛。缺了前者会有短暂错误，缺了后者会有永久不一致。

**并且项目在代码注释里记录了一个真实的死锁**：`channel_cache.go` 注明 **`channelSyncLock` 必须在失效定价缓存之前释放**，否则会形成 AB-BA 死锁（渠道锁等定价锁，定价锁等渠道锁）。

> **自建建议：缓存之间的加锁顺序必须写进代码注释或架构文档。** 这类死锁只在并发下偶发，事后极难复现和定位。

另一个细节：**缓存条目要带 schema 版本**（`UserBase.CacheSchema`、`userSessionCacheSchema`）。改了缓存结构后，旧格式的条目解码会失败 → 按版本号自动失效，而不是 panic 或读到错乱数据。

## 5. 自建骨架

### 配置系统（照抄这个模式）

```go
// 1. 定义 + 注册（每个配置域一个文件）
type BillingSetting struct {
	Mode       string `json:"mode"`
	Expr       string `json:"expr"`
	PreConsumeMultiplier float64 `json:"pre_consume_multiplier"`
}
var billingSetting = BillingSetting{ Mode: "ratio", PreConsumeMultiplier: 1.0 }

func init() { config.GlobalConfig.Register("billing_setting", &billingSetting) }

// 2. 唯一写入路径
func UpdateOption(key, value string) error {
	if err := validateOptionValue(key, value); err != nil { return err }
	if err := saveOption(key, value); err != nil { return err }
	applyOption(key, value)      // ★ 内存 + 运行时变量一起改
	return nil
}

// 3. 关联配置用原子快照
var policySnapshot atomic.Pointer[PolicySnapshot]
func rebuildPolicy() { policySnapshot.Store(build()) }
func currentPolicy() *PolicySnapshot { return policySnapshot.Load() }
```

### 迁移策略

```go
func Migrate(db *gorm.DB) error {
	if !isMaster() { return nil }              // 只有 master 迁移
	if err := db.AutoMigrate(models...); err != nil { return err }
	for _, m := range targetedMigrations {      // 幂等、可重复执行
		if err := m(db); err != nil { return err }
	}
	return nil
}
```

### 三库兼容检查清单

```
□ 没有裸 SQL 的标识符（go 保留字列用封装常量）
□ 行锁走 lockForUpdate()（SQLite 自动跳过）
□ 没有 GORM boolean default tag 承载业务默认值
□ 迁移里没有裸 ALTER COLUMN（SQLite 改用 ADD COLUMN）
□ 没有数据库特有 JSON 类型（或都有 TEXT 回退）
□ 主键交给 GORM，无 AUTO_INCREMENT/SERIAL
□ 新库初始化 + 从上一版本升级 + 重复启动三次 均验证过
```

## 6. 坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| `tx.Set("gorm:query_option","FOR UPDATE")` | GORM v2 静默忽略 → 并发保护形同虚设 | 共享 `lockForUpdate` helper |
| `gorm:"default:true"` 承载业务默认 | 每次重启都 ALTER TABLE | 默认值写在代码里 |
| 多节点同时 AutoMigrate | 迁移互相冲突 | 只允许 master 迁移 |
| 配置只改内存 map 不改运行时变量 | 配置看起来生效实际没生效 | 单一写入路径，两者一起改 |
| 关联配置分散读取 | 读到中间态组合 | 原子快照整体替换 |
| 缓存无版本号 | 结构改了以后读到错乱数据 | 版本化 schema，不匹配即失效 |
| 缓存之间加锁顺序不一致 | AB-BA 死锁（偶发、难查） | 固定加锁顺序 + 写进注释 |
| 日志表与业务表同库 | 日志量压垮业务库 | 独立 LOG_DB（可上 ClickHouse） |
| 迁移不幂等 | 重启即失败 | 所有迁移可重复执行，并验证三次启动 |

## 7. 本篇索引

| 关注点 | 文件 |
|---|---|
| DB 初始化与迁移 | `model/main.go`（`chooseDB`、`InitDB`、`InitLogDB`、`migrateDB`、`migrateLOGDB`） |
| 锁与方言 helper | `model/main.go`（`lockForUpdate`、`commonGroupCol`、`commonKeyCol`） |
| 主要模型 | `model/{user,token,channel,ability,log,redemption,topup,subscription,task}.go` |
| 配置表与分发 | `model/option.go`（`Option`、`InitOptionMap`、`UpdateOption`、`updateOptionMap`、`SyncOptions`） |
| 配置管理器 | `setting/config/config.go`（`ConfigManager`、`GlobalConfig`） |
| 配置域 | `setting/*_setting/` |
| 原子快照 | `model/request_policy.go` |
| Redis 封装 | `common/redis.go`、`pkg/cachex/*` |
| 缓存实现 | `model/channel_cache.go`、`model/token_cache.go`、`model/user_cache.go` |
| 审计日志 | `model/audit_log.go` |
| 规范来源 | `AGENTS.md`（数据库兼容章节，本质是一份 bug 清单） |

→ 继续阅读：[09 · 异步任务与插件体系](09-async-tasks-and-plugins.md)
