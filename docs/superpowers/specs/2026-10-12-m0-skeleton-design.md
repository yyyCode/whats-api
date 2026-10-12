# M0 · 技术骨架 — 设计文档

> 依据：`docs/architecture/11-blueprint.md` §3「M0」、`docs/architecture/01-overview.md` §6「启动时序」、
> `docs/architecture/02-request-lifecycle.md`、`docs/architecture/08-data-model-and-settings.md`。
> 本文是 M0 的实现契约；架构层面的取舍以那套分析文档为准，本文只记录**在本仓库落地时的具体决定**。

- 日期：2026-10-12
- 分支：`feat/m0-skeleton`
- 模块路径：`github.com/yyyCode/whats-api`
- Go 版本：`go 1.23`（跟随本机 go1.23.2 windows/amd64）

---

## 1. 目标与边界

**目标**：能跑起来一个空壳，但目录结构、依赖方向、配置与数据层的形状定下来。

**M0 做**：

```
main.go              初始化顺序：env → config → db → option → cache → router → server → 优雅停机
router/              路由分组骨架（/v1 转发面 vs /api 管理面）
middleware/          recovery / request-id / version / route-tag / logger / cors
model/               InitDB + AutoMigrate + 迁移只在 master 执行
setting/             配置注册表（struct + tag + Register）+ Option 表 + SyncOptions
common/              JSON 封装、数值安全（QuotaFromFloat 系列）、Redis 封装
controller/          一个 /api/status 探活接口
```

**M0 明确不做**（做了就是负债，见 blueprint §2 不做清单）：

| 不做 | 归属 |
|---|---|
| `/v1` 任何 relay 端点、`middleware/auth.go`、`middleware/distributor.go` | M1 |
| 计费预扣/结算/退款、账本表 | M2 |
| 管理面写接口（改渠道、改令牌、改配置的 HTTP 端点）、鉴权、前端 | M3 |
| Ability 索引表、优先级加权随机、渠道健康管理 | M4 |
| 系统任务框架、Prometheus/pprof 暴露、审计日志 | M5 |
| 协议互转（RelayKit）、表达式定价、JS 插件 | M6（按需） |

**M0 只建 M0 真正读写的两张表**：`options`、`setup`。user / token / channel / ability / log
等到使用它们的里程碑再加——`AutoMigrate` 加表幂等，提前建是纯负债。

**明确不做的事**：不引入 `relaykit` / `tokenkit` 独立 module（M0 没有协议转换与 token 计数）。

---

## 2. 包布局与依赖规则

选择 **`internal/` + 显式依赖**。包名与文档保持一致，便于对照阅读；`internal/` 由编译器守住"这是宿主、不是库"的边界。

```
whats-api/
├── main.go                       入口：信号 → ctx → app.Run(ctx)
├── go.mod  go.sum
├── .env.example  .gitignore  README.md
├── Dockerfile  docker-compose.yml
├── cmd/optctl/main.go            开发工具：读写 Option 表（验收脚本依赖它）
├── scripts/m0-acceptance.sh      M0 验收脚本
├── docs/architecture/            既有参考文档
├── docs/superpowers/specs/       设计与实施文档
└── internal/
    ├── app/app.go                Run(ctx)：启动时序 + HTTP 生命周期 + 优雅停机
    ├── common/
    │   ├── constants.go          版本号、默认端口、缓存 namespace 前缀
    │   ├── env.go                InitEnv()：环境变量 → 启动期常量
    │   ├── json.go               Marshal / Unmarshal / MarshalIndent 封装
    │   ├── quota_math.go         记账常量 + 数值安全函数族 + QuotaClamp
    │   └── redis.go              go-redis v8 封装 + 未配置时的退化标志
    ├── logger/logger.go          log/slog + 可热更新级别
    ├── cachex/                   HybridCache[V]：Redis 与进程内自动切换
    │   ├── cache.go  memory.go  redis.go
    ├── setting/
    │   ├── config/config.go      ConfigManager：Register / ExportAllConfigs / UpdateConfig / OnRebuild
    │   └── system_setting/       第一个配置域（含原子快照 Get()）
    ├── model/
    │   ├── main.go               DB / LOG_DB、chooseDB、Migrate、lockForUpdate、方言常量
    │   ├── option.go             Option / InitOptionMap / UpdateOption / updateOptionMap / SyncOptions
    │   └── setup.go              Setup / CheckSetup
    ├── middleware/               recovery.go request-id.go version.go route-tag.go logger.go cors.go
    ├── controller/
    │   ├── response.go           统一响应 envelope + 错误形状
    │   └── status.go             GET /api/status
    └── router/
        ├── main.go               SetRouter(engine)
        ├── api-router.go         /api 管理面
        ├── relay-router.go       /v1 转发面（仅分组骨架）
        └── web-router.go         NoRoute 兜底
```

**依赖方向（自上而下，同层不互相依赖）**：

```
app → router → controller → { middleware, model } → { setting/config, cachex } → { common, logger }
```

- 即：`common` 与 `logger` 不 import 本项目其他包；`setting` 与 `cachex` 只依赖 `common`/`logger`；
  `model` 依赖 `setting/config` 与 `common`；`middleware` 依赖 `setting` 与 `logger`；`controller` 依赖 `model`。
- `middleware` 不 import `controller`；`controller` 不 import `router`。
- `setting/config` 与 `setting/*_setting` 不 import `model`（避免与 `model/option.go` 成环）：
  DB 的读写由 `model/option.go` 发起，配置域只提供 schema 与快照。

**全局变量规则**（避免"选了显式依赖却处处全局"的假 DI）：

| 状态 | 形态 | 理由 |
|---|---|---|
| 环境变量派生的启动期常量（端口、超时、是否 master、同步频率、版本） | `common` 包级变量，启动期写一次，之后只读 | 不可变，无竞争 |
| 进程级单例句柄（`model.DB`、`model.LOG_DB`、配置注册表、缓存后端、logger） | 包级变量，`Init*` 写一次，之后只读 | 真实单例；测试通过重新 `Init` 指向临时库注入 |
| 请求级 / 运行期可变状态 | 显式参数传递，或 `atomic.Pointer` 快照 | 这是并发正确性的关键，不允许全局裸变量 |
| 配置域结构体（会被反射写入） | **只通过该域的 `Get() *T` 快照读取**，不允许直接引用可写实例 | 防止热更新时的 data race |

---

## 3. 启动时序

严格保持 blueprint 文档中步骤的**相对顺序**（跳过 M0 范围外的步骤）：

```go
// internal/app/app.go
func Run(ctx context.Context) error {
    // 1  godotenv.Load(".env")        非致命：文件不存在只记 debug
    //    common.InitEnv()            环境变量 → 派生常量
    //    logger.SetupLogger()        初始级别取 LOG_LEVEL
    // 2  model.InitDB()               ★致命：chooseDB → AutoMigrate（仅 master）→ 定向迁移
    // 3  model.CheckSetup()           Setup 表首次安装标志
    // 4  model.InitOptionMap()        ★必须先于任何读配置的逻辑
    // 5  model.InitLogDB()            LOG_SQL_DSN 为空则复用主库
    // 6  common.InitRedisClient()     非致命：失败降级内存缓存
    // 7  cachex.Init()                依据 RedisEnabled 选后端
    // 8  go model.SyncOptions(ctx, SYNC_FREQUENCY)
    // 9  gin.New() → 全局中间件 → router.SetRouter(engine)
    // 10 srv.ListenAndServe()（goroutine）
    // 11 <-ctx.Done() → srv.Shutdown(SHUTDOWN_TIMEOUT) → 关 DB → 等后台 goroutine 退出
}
```

`main.go` 只做三件事：`signal.NotifyContext(SIGINT, SIGTERM)` → `app.Run(ctx)` → 按错误码 `os.Exit`。

**为什么 `Run(ctx)` 独立成函数**：本机（Windows/Git Bash）无法向原生进程投递 SIGTERM，
把停机路径做成可被测试直接取消 ctx 驱动，才能真实验证"在途请求跑完 + DB 关闭 + 后台 goroutine 退出"。
信号处理只是 `Run(ctx)` 外面的一层薄壳。

**关键顺序约束（写进代码注释）**：

1. `InitOptionMap()` 必须在任何读配置的模块之前——包括 `InitLogDB`（日志库 DSN 将来可配置）。
2. `InitDB` 是唯一的致命初始化；其余（Redis、日志库、dotenv）失败都降级。
3. 后台 goroutine 全部接收 `ctx`，`Run` 返回前用 `sync.WaitGroup` 等它们退出，避免停机期写已关闭的 DB。

---

## 4. 配置系统

M0 的重心，照 `08-data-model-and-settings.md` §3 落地。

### 4.1 存储

```go
type Option struct {
    Key   string `gorm:"primaryKey"`
    Value string
}
```

不写 `gorm:"default:..."`（规则③：GORM 布尔/默认值 tag 会让 AutoMigrate 反复 ALTER TABLE）。
业务默认值一律来自代码里的结构体初始值。

### 4.2 注册表（`internal/setting/config`）

```go
type Manager struct {
    modules map[string]*module   // name → { ptr, fields, rebuildHooks }
    mu      sync.RWMutex
}

var GlobalConfig = NewManager()

// Register 在 init() 中调用；cfg 必须是非 nil 结构体指针。
// 约束（启动期 fail-fast）：
//   - 同一 name 重复注册 → panic
//   - 非指针 / 非结构体 → panic
//   - 存在没有 json tag 的导出字段 → panic（key 由 module + json tag 生成，缺 tag 会导致命名漂移）
func (m *Manager) Register(name string, cfg any)

// ExportAllConfigs 返回所有注册域的默认值：key = "<module>.<jsonField>", value = JSON 编码
func (m *Manager) ExportAllConfigs() map[string]string

// UpdateConfig 按 json tag 定位字段并写回；未知 key / 类型不符 返回错误且不写入
func (m *Manager) UpdateConfig(key, rawJSON string) error

// OnRebuild 注册某域的"重建快照"回调；在 Manager 写锁内被调用，实现方负责原子发布
func (m *Manager) OnRebuild(name string, hook func())

// Modules / FieldCount：供 /api/status 展示（不暴露具体值）
func (m *Manager) Modules() []string
```

支持的字段类型（M0）：`string`、`bool`、`int`、`int64`、`float64`。
其他类型在 `UpdateConfig` 中返回明确的 unsupported 错误；扩展点写在 `updateField` 一处。

### 4.3 原子快照（对文档草稿的一处加固）

文档草稿的形状是"反射把值写进注册的结构体，中间件直接读结构体"。这在并发下是 data race：
`UpdateOption` 反射写 `LogLevel` 字段的同时，访问日志中间件正在读它。

按同一篇文档 §3「原子快照」收口，每个配置域暴露无锁读：

```go
// internal/setting/system_setting/system_setting.go
type SystemSetting struct {
    LogLevel                    string  `json:"log_level"`
    StatusMessage               string  `json:"status_message"`
    EnableAccessLog             bool    `json:"enable_access_log"`
    SlowRequestThresholdSeconds float64 `json:"slow_request_threshold_seconds"`
}

var defaultValue = SystemSetting{
    LogLevel: "info", StatusMessage: "M0 技术骨架运行中",
    EnableAccessLog: true, SlowRequestThresholdSeconds: 3,
}

var (
    current    atomic.Pointer[SystemSetting] // 热路径读的不可变快照
    registered = defaultValue                // ★ 反射写入目标；只允许在 Manager 写锁内被访问
)

func init() {
    d := defaultValue
    current.Store(&d)
    config.GlobalConfig.Register("system_setting", &registered) // schema、默认值、持久化 key 的唯一来源
    config.GlobalConfig.OnRebuild("system_setting", func() {
        snap := registered // 值拷贝；本回调由 Manager 在写锁内调用，故此处无并发写
        current.Store(&snap)
    })
}

// Get 返回当前不可变快照；热路径读无锁，且绝不会读到中间态。
// 约定：registered 只由配置系统写，业务代码一律不许直接引用它。
func Get() *SystemSetting { return current.Load() }
```

于是同一份 `SystemSetting` 结构体同时承担四件事（文档所称"五份合成一份"）：类型、默认值、持久化 key、
文档；而**读侧是 lock-free 的快照**。

`Register` 传入的实例与快照是两个对象，必须由 `OnRebuild` 把它们连起来——这是本项目对文档模式的唯一修订。

### 4.4 读写流程与唯一写入路径

```
启动：InitOptionMap()
        ├─ config.GlobalConfig.ExportAllConfigs()   默认值（含所有域的初始快照）
        └─ loadOptionsFromDatabase()                DB 行覆盖默认值 → updateOptionMap(key, value)

改配置：UpdateOption(key, value)                     ★ 唯一写入路径
        ├─ validateOptionValue(key, value)           未注册 key → 拒绝
        ├─ saveOption(key, value)                    落库（失败则不改变内存）
        └─ updateOptionMap(key, value)
              ├─ OptionMap[key] = value              内存 map（多节点同步用）
              ├─ handleConfigUpdate(key, value)      反射写入注册结构体 + 触发 OnRebuild（原子发布快照）
              └─ applySideEffects(key)               非结构化副作用开关（M0：日志级别）

热更新：SyncOptions(ctx, freq)                        定时从 DB 重载全部 Option 行 → updateOptionMap
```

- 内存 map 与运行时结构体**必须一起改**，且只有 `updateOptionMap` 一个分发点。
- `applySideEffects` 是一个集中 switch（文档的"一个大 switch"），M0 只处理
  `system_setting.log_level`（调 `logger.SetLevel`）与 `system_setting.enable_access_log`（无需副作用，
  由中间件读快照）。`status_message` / `slow_request_threshold_seconds` 也无副作用，同样读快照。
- `validateOptionValue`：key 必须存在于注册表；值必须能解析成该字段类型。

### 4.5 SyncOptions 的多实例语义

`SyncOptions` 每次重载**全量**选项行（不是增量），这样即使某实例错过了中间某次变更也能收敛。
只对 `OptionMap` 中值发生变化的 key 调用 `updateOptionMap`，避免无谓的重建与日志噪声。
`SYNC_FREQUENCY` 小于 5 秒时抬到 5 秒并告警（防止把业务库当轮询靶子）。

---

## 5. 数据层

照 `08-data-model-and-settings.md` §2 的六条规则。

### 5.1 驱动选择

```go
// internal/model/main.go
func chooseDB(envName string, isLog bool) (gorm.Dialector, dbKind, error)
```

| DSN | 驱动 | 备注 |
|---|---|---|
| `postgres://` / `postgresql://` | `gorm.io/driver/postgres` | 强制 `PreferSimpleProtocol=true`（兼容 PgBouncer/Neon） |
| `clickhouse://` / `tcp://` / `http://` / `https://` | `gorm.io/driver/clickhouse` | **仅日志库**；主库遇到直接返回错误 |
| 空 或 `local` / `local:<path>` | `github.com/glebarez/sqlite`（纯 Go，免 CGO） | 空 → `whats-api.db`；`local` → 默认路径；`local:<path>` → 该路径 |
| 其他 | `gorm.io/driver/mysql` | DSN 缺 `parseTime` 时补 `parseTime=true` |

**M0 只本地验证 SQLite**：本机没有 MySQL / PostgreSQL / ClickHouse 服务。
`chooseDB` 的分派与"主库拒绝 ClickHouse"用单元测试覆盖（断言返回的驱动类型与错误），
**不谎称连过真实 MySQL/PG**。三库端到端验证留到有真实实例时（文档 §8 要求的那条纪律）。

### 5.2 方言 helper（一次到位，避免 M1/M2 回头改）

```go
// 保留字列：PostgreSQL 用 "group"，MySQL/SQLite 用 `group`
func commonGroupCol() string   // 包内保留字 group 的引用写法
func commonKeyCol() string     // 保留字 key 的引用写法
func commonTrueVal() string    // 按 dialect 返回 "1"/"0" 或 "true"/"false"
func commonFalseVal() string
func lockForUpdate(tx *gorm.DB) *gorm.DB  // MySQL/PG 发 FOR UPDATE；SQLite 原样返回
func isSQLite() / isPostgres() / isMySQL() bool
```

**实现形态说明**：文档里这三个是变量，本项目实现为**函数**——方言在 `InitDB` 后才知道，
用函数取可以避免包级变量与 `InitDB` 的初始化顺序耦合（变量若在 `InitDB` 前被读取就是空串，静默出错）。

`lockForUpdate` 的存在理由：**绝不用 `tx.Set("gorm:query_option", "FOR UPDATE")`**——GORM v2 会静默忽略它，
"看起来有锁、实际没有"是并发保护里最阴险的一类 bug。

### 5.3 迁移

```go
func Migrate() error {
    if !common.IsMasterNode { logger.Info("非 master 节点，跳过迁移"); return nil }
    if err := DB.AutoMigrate(&Option{}, &Setup{}); err != nil { return err }
    for _, m := range targetedMigrations { if err := m(DB); err != nil { return err } }
    return nil
}
```

- `targetedMigrations` 在 M0 为空切片，但**框架与调用点先建好**（M1/M2 会往里加）。
- 幂等：`options` / `setup` 的建表与首次写入必须可重复执行；验收脚本连续启动三次。
- `InitLogDB()`：`LOG_SQL_DSN` 为空 → `LOG_DB = DB`（`SharedWithMain=true`）；否则独立连接 + 独立迁移。
  ClickHouse 的 MergeTree/TTL 特化不在 M0（日志表实体要到 M2/M5 才出现）。

### 5.4 setup

```go
type Setup struct {
    ID            uint   `gorm:"primaryKey"`
    Version       string
    InitializedAt int64
}
func CheckSetup() error   // 空表则写入第一行；已存在则拒绝（ID 固定为 1）
```

---

## 6. 缓存与 Redis 退化

```go
// internal/common/redis.go
var (
    RedisEnabled bool          // REDIS_CONN_STRING 有效且 PING 通过
    RDB          *redis.Client // 未启用时为 nil
)
func InitRedisClient()   // 非致命：连接失败 → RedisEnabled=false + WARN，继续启动
func RedisGet / RedisSet / RedisDel / RedisIncr / RedisHSetObj / RedisHGetObj / RedisKeyCacheSeconds
```

```go
// internal/cachex
type ValueCodec[V any] interface { Encode(V) string; Decode(string) (V, error) }
type HybridCache[V any] struct { ... }
func New[V any](namespace string, codec ValueCodec[V], ttl time.Duration) *HybridCache[V]
func (c *HybridCache[V]) Get(key string) (V, bool)
func (c *HybridCache[V]) Set(key string, v V)
func (c *HybridCache[V]) Incr(key string) (int64, error)
func (c *HybridCache[V]) Delete(key string)
```

- Redis 可用 → 走 Redis；不可用 → 进程内实现（`sync.Map` + 惰性过期）。
- 两种后端的语义差异（TTL 精度、原子性）封在实现内部；`Incr` 在内存后端用 `atomic` 保证原子。
- **M0 的真实用途**：`requests_total` 计数器（`/api/status` 暴露）。
  这样"拔掉 Redis 也能启动并继续工作"不是空话，是可断言的观察点。
- 不使用 Redis 做配置同步（那是 `SyncOptions` 的 DB 轮询路径，M0 不引入 Redis 发布订阅）。

---

## 7. 中间件与路由

### 7.1 中间件顺序

```
gin.New()（不用 gin.Default，避免它自带的日志中间件与自研访问日志重复）
  ├─ CustomRecovery         panic → 统一 500 JSON + 记录堆栈（不泄漏内部细节）
  ├─ RequestId              X-Request-Id：入站沿用，缺失则生成；写回响应头 + 注入 context
  ├─ Version                响应头 X-Whats-Api-Version
  ├─ RouteTag               /v1 → "relay"，/api → "api"，其他 → "web"（供日志/指标按面聚合）
  ├─ AccessLog              结构化访问日志：method/path/status/latency_ms/size/request_id/route_tag/client_ip；超阈值打 slow 标记
  └─ CORS                   CORS_ALLOW_ORIGINS（默认 *）
```

**一处与文档的差异**：文档的 `RouteTag` 是**按路由组**挂的（`relayV1Router.Use(...)`）。
M0 把它做成全局中间件、按路径前缀推断标签——因为 M0 还没有真正的 relay 端点，
按组挂会得到"一个没有 handler 的组"。M1 加入 `/v1` 端点时，如果需要更精确的标签（例如
`/v1/tasks` 与 `/v1/chat/completions` 分开聚合），再改为按组挂载即可，`AccessLog` 读的是同一份 context key。

`AccessLog` 与 `CORS` 读 `system_setting.Get()` 快照：`EnableAccessLog` 可热关闭日志，
`SlowRequestThresholdSeconds` 决定 slow 标记。**不读可写结构体，避免 race。**

### 7.2 路由分组

```go
// internal/router
func SetRouter(engine *gin.Engine) {
    setApiRouter(engine)     // /api  管理面
    setRelayRouter(engine)   // /v1   转发面（仅骨架）
    setWebRouter(engine)     // NoRoute 兜底
}
```

- `/v1` 组在 M0 **只有分组与标记，不注册任何端点**（严格 M0 范围）。它存在的意义是把
  "两个面从一开始就分开"这条决策固化成代码形状，M1 直接往里加 handler。
- `NoRoute`：`/v1/*`、`/api/*`、`/assets/*` 主动返回 404 JSON（避免将来 SPA 兜底把写错的 API 路径吞成 HTML）；
  其他路径 M0 返回 404 JSON 并提示"前端资源尚未构建（M3）"。
- `/api` 与 `/v1` 的错误形状从一开始就不同：`/api` 是 `{success,message,data}` 信封（将来带 `message_key` 供 i18n），
  `/v1` 将来是 OpenAI/Claude 形状的错误 JSON。M0 只实现前者。

### 7.3 `/api/status`

M0 唯一的业务端点，既是探活也是**验收的观察窗**（管理面写接口是 M3，所以热更新必须在这里可被观察到）：

```json
{ "success": true, "message": "", "data": {
  "version": "v0.1.0-m0",
  "uptime_seconds": 12,
  "node_type": "master", "node_name": "",
  "is_master": true,
  "db":     { "driver": "sqlite", "dsn_kind": "file", "ok": true },
  "log_db": { "driver": "sqlite", "shared_with_main": true, "ok": true },
  "cache":  { "backend": "memory", "redis_configured": false },
  "config": { "modules": ["system_setting"], "registered_keys": 4 },
  "setup_complete": true,
  "status_message": "M0 技术骨架运行中",
  "log_level": "info",
  "enable_access_log": true,
  "slow_request_threshold_seconds": 3,
  "requests_total": 42
} }
```

**不含任何敏感信息**（无 DSN 全文、无密钥、无配置项全量值）。最后四个字段来自 `system_setting` 快照，
配置一改即变——验收标准③的落点。

---

## 8. 数值安全（`internal/common/quota_math.go`）

虽然计费闭环是 M2，但按 blueprint M0 交付项，整数账本的数值安全要**先做好**。

```go
const (
    QuotaPerUnit   = 500 * 1000  // 50 万 quota = 1 美元
    MaxQuota       = math.MaxInt32
    MaxWalletQuota = 1<<53 - 1   // JS 安全整数上限，前端不丢精度
    USD2RMB        = 7.3         // 仅展示用
)

type QuotaClamp uint8
const (ClampNone QuotaClamp = iota; ClampOverflow; ClampUnderflow; ClampNaN)
func (c QuotaClamp) Saturated() bool
func (c QuotaClamp) String() string
func (c QuotaClamp) AdminMap() map[string]any   // 写入日志 admin_info.quota_saturation

func QuotaFromFloat(f float64) int                                  // 静默饱和（展示类）
func QuotaFromFloatChecked(f float64) (int, QuotaClamp)             // 饱和 + 留痕（结算/日志）
func QuotaFromFloatStrict(f float64) (int, error)                   // 溢出即错（预扣）
func QuotaRound(f float64) int
func QuotaRoundChecked(f float64) (int, QuotaClamp)
func QuotaRoundStrict(f float64) (int, error)
func QuotaFromDecimal(s string) int                                 // shopspring/decimal，避免字符串价格走 float
func QuotaFromDecimalChecked(s string) (int, QuotaClamp)            // 溢出/NaN 留痕
func QuotaFromDecimalStrict(s string) (int, error)                  // 溢出/解析失败即错（预扣）
```

**饱和语义（本项目的决定，写进账本语义前请复核）**：

| 情况 | 基础版 | Checked | Strict |
|---|---|---|---|
| `NaN` | 0 | `0` + `ClampNaN` | error |
| `+Inf` / 超 `MaxQuota` | `MaxQuota` | `MaxQuota` + `ClampOverflow` | error |
| `-Inf` / 负数 | 0 | `0` + `ClampUnderflow` | error（负数在计费语境下是 bug） |
| 正常 | round half away from zero | 同左，`ClampNone` | 同左 |

理由：结算时把 `NaN` 饱和到 `MaxQuota` 会给出荒谬的巨额扣费；饱和到 0 配合 `QuotaClamp` 留痕，
让调用方必须显式处理（文档 I4「没有 usage 时不按 0 计费」靠的就是调用方检查标志后走退款）。
**约定：任何 Checked 调用点拿到 `Saturated() == true` 都必须留痕，不允许静默丢弃。**

`QuotaRound` 用于分层计费，保证预扣与结算不差 ±1 quota。

---

## 9. 依赖

| 依赖 | 用途 | 版本约束 |
|---|---|---|
| `github.com/gin-gonic/gin` | HTTP | v1.10.x（Go 1.23 兼容） |
| `gorm.io/gorm` | ORM | v1.25.x |
| `gorm.io/driver/mysql` / `driver/postgres` | 三库 | 与 gorm 主版本匹配 |
| `github.com/glebarez/sqlite` | 纯 Go SQLite（免 CGO） | 需其 `modernc.org/sqlite` 依赖支持 Go 1.23 |
| `github.com/go-redis/redis/v8` | Redis（文档指定 v8） | v8.11.x |
| `github.com/shopspring/decimal` | `QuotaFromDecimal` | v1.4.x |
| `github.com/joho/godotenv` | `.env` | v1.5.x |

**不引入**：测试框架（用标准 `testing`）、日志库（用标准 `log/slog`）、内存 LRU 库（`cachex` 自己实现）。
`gorm.io/driver/clickhouse` 只在 M0 的 `chooseDB` 分支里被识别（返回"仅日志库允许"的错误），
实际驱动依赖推迟到真的接 ClickHouse 时再引。

**风险与退路**：`github.com/glebarez/sqlite` 传递依赖的 `modernc.org/sqlite` 新版本若要求 Go > 1.23，
则回退为 `gorm.io/driver/sqlite`（CGO）。本机有 gcc 13.2，可编译；代价是 Docker 构建需非 scratch 基础镜像
且 `CGO_ENABLED=1`。**此风险在实施第 1 步就验证，不拖到收尾。**

---

## 10. 验收标准与测试策略

### 10.1 文档定义的四条验收标准

| # | 标准 | 本机可验证性 | 验证手段 |
|---|---|---|---|
| ① | `docker run` 起来能用 SQLite 跑通，且不配任何环境变量 | ⚠️ 本机无 docker | 等价替代：清空环境变量直接跑二进制；`Dockerfile`/`compose` 写好但**未构建验证**，标注需你在有 docker 的环境复核 |
| ② | 拿掉 Redis 也能启动（退化为内存缓存） | ✅ | 验收脚本：故意配一个不可达的 `REDIS_CONN_STRING`，断言仍启动 + `cache.backend == "memory"` |
| ③ | 改一个配置项，不重启即生效，且多实例在 `SYNC_FREQUENCY` 后收敛 | ✅ | 单实例立即生效 → `model/option_test.go`（断言 map + 结构体 + 快照三者同时变）；多实例收敛 → 验收脚本跑两个实例共享同一 SQLite，`optctl` 写库后轮询另一实例的 `/api/status` |
| ④ | 进程收到 SIGTERM 后能优雅停机 | ⚠️ 本机无法投递 SIGTERM | in-process 测试驱动 `Run(ctx)` 的取消路径（在途请求跑完 + DB 关闭 + 后台 goroutine 退出）；端到端信号验证标注需 Linux/docker |

**不谎称验证过的**：①②的 Docker 形态、④的真实信号、以及 MySQL/PostgreSQL/ClickHouse 的真实连接。

### 10.2 测试清单

| 测试文件 | 保护的真实契约 |
|---|---|
| `internal/common/quota_math_test.go` | 三档语义表驱动：正常/±Inf/NaN/超界/负数/round-half/`MaxWalletQuota` 边界；`AdminMap` 内容 |
| `internal/setting/config/config_test.go` | key 生成（`module.jsonField`）、`ExportAllConfigs`、反射写回四种类型、未知 key 报错、类型不符报错且不写入、缺 json tag 时 `Register` panic、重复注册 panic |
| `internal/setting/system_setting/system_setting_test.go` | 快照原子替换；并发读到的一定是完整版本（`-race` 下跑） |
| `internal/model/main_test.go` | `chooseDB` 前缀分派（含主库拒 ClickHouse）、`lockForUpdate` 在 SQLite 下不产生 `FOR UPDATE`、`AutoMigrate` 连续三次幂等、非 master 不迁移 |
| `internal/model/option_test.go` | `InitOptionMap` 默认值 → DB 覆盖；`UpdateOption` 单一路径三者同时变；`SyncOptions` 在外部改库后收敛；未知 key 被拒 |
| `internal/app/app_test.go` | `Run(ctx)` 取消后：在途请求拿到完整响应、DB 已关闭、后台 goroutine 已退出 |

测试纪律（照 blueprint §8）：**不为小改动在每层加测试文件，不写只提升覆盖率数字的测试，
不写随机输入/大循环/计时的伪压力测试。测试保护真实契约，不保护行数。**
统一以 `go test -race ./...` 运行。

---

## 11. 交付物

| 交付物 | 说明 |
|---|---|
| `internal/` 全部代码 + `main.go` | 见第 2 节文件树 |
| `cmd/optctl` | 开发工具：`get <key>` / `set <key> <value>` / `list`，直接走 `model.UpdateOption`；验收脚本与人工调试用 |
| `scripts/m0-acceptance.sh` | 驱动四条验收标准，输出 PASS/FAIL/需外部环境复核，任一 FAIL 退出码非 0 |
| `Dockerfile` | 多阶段构建；纯 Go SQLite → 可 `CGO_ENABLED=0`；非 root 用户；`VOLUME /data`；`SQL_DSN=/data/whats-api.db`；healthcheck 打 `/api/status` |
| `docker-compose.yml` | 单服务 + 数据卷，零必填环境变量；附注释掉的 redis / postgres 备选服务 |
| `README.md` | 项目定位、当前里程碑（M0）、目录结构、启动方式（本地/Docker）、**环境变量清单**、验收方法、与 `docs/architecture` 的关系、下一步 M1 |
| `.env.example` | 全部环境变量 + 默认值 + 说明 |
| `.gitignore` | `*.db`、`*.db-journal`、`.env`、`bin/`、构建产物 |

### 环境变量清单

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `3000` | HTTP 监听端口 |
| `SQL_DSN` | 空 → `whats-api.db`（SQLite） | 主库 DSN，按前缀选驱动 |
| `LOG_SQL_DSN` | 空 → 复用主库 | 日志库 DSN |
| `REDIS_CONN_STRING` | 空 → 进程内缓存 | Redis 连接串；无效即降级 |
| `NODE_TYPE` | `master` | `master` / `slave`；仅 master 执行迁移 |
| `NODE_NAME` | 空 | 实例标识，M0 仅展示 |
| `SYNC_FREQUENCY` | `60` | 配置定时同步秒数，最小 5 |
| `SHUTDOWN_TIMEOUT_SECONDS` | `120` | 优雅停机超时（SSE 可能跑几分钟，别调小） |
| `LOG_LEVEL` | `info` | 启动期日志级别，可在库配置中覆盖 |
| `GIN_MODE` | `release` | gin 运行模式 |
| `CORS_ALLOW_ORIGINS` | `*` | 允许的跨域来源，逗号分隔 |
| `SESSION_SECRET` | — | **M0 未使用**（鉴权在 M1/M3），先列入 `.env.example` 注释 |

### 提交计划（`feat/m0-skeleton` 分支）

按逻辑分层提交，便于 review 与回滚：

```
1 chore: 初始化 Go module、目录骨架与交付物脚手架
2 feat(common): 数值安全、JSON 封装、环境变量与 Redis 封装
3 feat(setting): 反射驱动的配置注册表与原子快照
4 feat(model): 三库兼容的 DB 初始化、幂等迁移与 Option 唯一写入路径
5 feat(http): 中间件链、路由分组与 /api/status
6 feat(app): 启动时序与优雅停机
7 test: M0 验收脚本
8 docs: README、环境变量清单与实施文档
```

完成后推分支并按 `pr-submission-standard` 模板开 PR（功能描述 / 实现思路 / 测试方式 三段）。

---

## 12. M0 完成定义（DoD）

```
□ go build ./...            通过
□ go vet ./...              通过
□ go test -race ./...       全绿
□ 不配任何环境变量能启动，/api/status 返回 db.driver=sqlite、cache.backend=memory
□ 配一个不可达的 REDIS_CONN_STRING 仍能启动且降级
□ 配置项改动①单实例立即生效（测试）②另一实例在 SYNC_FREQUENCY 内收敛（脚本）
□ Run(ctx) 取消后干净退出：在途请求完成、DB 关闭、后台 goroutine 全退出
□ 连续启动三次无副作用（迁移幂等）
□ Dockerfile / compose / README / 验收脚本齐备（Docker 与 SIGTERM 端到端标注需外部环境复核）
□ PR 按模板开好，描述含三段
```

**下一步**：M1（转发闭环）——`/v1/chat/completions` 透传 + SSE + 重试骨架。
