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
| 计费预扣/结算/退款、消费日志的**写入**（表结构 M0 已建） | M2 |
| 管理面写接口（改渠道、改令牌、改配置的 HTTP 端点）、鉴权、前端 | M3 |
| Ability 索引的**重建与消费**、优先级加权随机、渠道健康管理（表结构 M0 已建） | M4 |
| 系统任务框架、Prometheus/pprof 暴露、审计日志 | M5 |
| 协议互转（RelayKit）、表达式定价、JS 插件 | M6（按需） |

**M0 一次建齐核心表结构**（决策记录）：主库 `users` / `tokens` / `channels` / `abilities` / `options` / `setup`，
日志库 `logs`。理由是表结构与其消费逻辑不同——**结构一改就要写定向迁移，而加表/加列是幂等的**，
所以把"形状"提前定死更便宜。约束：

- 字段**以 `11-blueprint.md` §4 的最小 DDL 骨架为准，不自行扩列**（不引入那里没有的字段）；
- 模型在 M0 **只有 struct 定义，没有任何 CRUD / 查询层 / 缓存**；写路径随各自的里程碑进来
  （users/tokens/channels 在 M3，abilities 在 M4，logs 在 M2）；
- 表里的持久化枚举（`channels.type`、`logs.type`、`status`、`role`）是 **ABI：只增不改**，
  数值在 `internal/constant` 集中定义并用测试钉住（见 §5.5）。

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
    ├── constant/constants.go     持久化枚举（role/status/log type…）：ABI，只增不改
    ├── common/
    │   ├── constants.go          版本号、默认端口、缓存 namespace 前缀
    │   ├── env.go                InitEnv()：环境变量 → 启动期常量
    │   ├── json.go               Marshal / Unmarshal / MarshalIndent 封装
    │   ├── quota_math.go         记账常量 + 数值安全函数族 + QuotaClamp
    │   └── redis.go              go-redis v8 封装 + 未配置时的退化标志
    ├── logger/logger.go          log/slog + 可热更新级别
    ├── cachex/counter.go         Counter：Redis 与进程内自动切换（M0 唯一的缓存原语，见 §6.1）
    ├── setting/
    │   ├── config/config.go      ConfigManager：Register / ExportAllConfigs / UpdateConfig / ValidateConfig / OnRebuild
    │   └── system_setting/       第一个配置域（含原子快照 Get()）
    ├── model/
    │   ├── main.go               DB / LOG_DB、chooseDB、Migrate、migrateLOGDB、lockForUpdate、方言 helper
    │   ├── user.go               User
    │   ├── token.go              Token
    │   ├── channel.go            Channel
    │   ├── ability.go            Ability（复合主键）
    │   ├── log.go                Log（落在 LOG_DB）
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
app → router → controller → { middleware, model } → { setting/config, cachex } → { common, constant }

logger：横切工具，只依赖标准库，任何层都可以 import（包括 common）
```

- 即：`logger`、`constant` 不 import 本项目其他包（叶子）；`common` 只依赖这两个叶子
  （Redis 初始化期连不上要立刻告警，那一步 logger 已经就绪；`InitEnv` 之所以只能攒 `EnvWarnings`，
  是因为它跑在 logger 之前）；`setting` 与 `cachex` 只依赖 `common`/`logger`；
  `model` 依赖 `setting/config`、`constant` 与 `common`；`middleware` 依赖 `setting` 与 `logger`；
  `controller` 依赖 `model`。
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
    // 2  model.InitDB()               ★致命：chooseDB → AutoMigrate（仅 master）
    // 3  model.CheckSetup()           Setup 表首次安装标志（仅 master 写入）
    // 4  model.InitOptionMap()        ★必须先于任何读配置的逻辑
    // 5  model.InitLogDB()            LOG_SQL_DSN 为空则复用主库；显式配置却连不上 → 致命
    // 6  common.InitRedisClient()     非致命：失败降级内存缓存
    // 7  go model.SyncOptions(ctx, SYNC_FREQUENCY)
    // 8  cachex.NewCounter(...)       M0 唯一的缓存原语（无 cachex.Init，见 §6.1）
    // 9  gin.New() → 全局中间件 → router.SetRouter(engine) → srv.ListenAndServe()（goroutine）
    // 10 <-ctx.Done() 或 ListenAndServe 返回错误
    // 11 srv.Shutdown(SHUTDOWN_TIMEOUT) → wg.Wait() 等后台 goroutine → model.CloseDB() → CloseRedis()
}
```

`main.go` 只做三件事：`signal.NotifyContext(SIGINT, SIGTERM)` → `app.Run(ctx)` → 按错误码 `os.Exit`。

**为什么 `Run(ctx)` 独立成函数**：本机（Windows/Git Bash）无法向原生进程投递 SIGTERM，
把停机路径做成可被测试直接取消 ctx 驱动，才能真实验证"在途请求跑完 + DB 关闭 + 后台 goroutine 退出"。
信号处理只是 `Run(ctx)` 外面的一层薄壳。为了让"在途请求跑完"这条**可观测**，
`Run` 拆成 `Run(ctx)` 与 `RunWithOptions(ctx, Options{InjectRoutes func(*gin.Engine)})`——
M0 没有任何慢到能观察到停机排水的生产端点，测试靠注入一个慢端点来验证。
生产代码永远不传 `InjectRoutes`。

**关键顺序约束（全部写进代码注释）**：

1. `InitOptionMap()` 必须在任何读配置的模块之前——包括 `InitLogDB`（日志库 DSN 将来可配置）。
2. **停机顺序：先 `srv.Shutdown`（等在途请求），再 `wg.Wait()`（等后台 goroutine），最后 `CloseDB()`。**
   顺序颠倒会让后台 goroutine 在停机期写到已关闭的连接上——
   那类错误只在停机瞬间偶发，平时测得再多次也遇不到。
3. **致命 vs 降级**：

   | 初始化 | 失败后果 | 理由 |
   |---|---|---|
   | `InitDB` / 迁移 | **致命** | 主库连不上没有任何业务可谈 |
   | `CheckSetup` | **致命** | setup 是安装标志，写不进去说明库不可用 |
   | `InitOptionMap` | **致命** | 配置未装载时读到的默认值会静默覆盖管理员配置 |
   | `InitLogDB`（显式配了 `LOG_SQL_DSN`） | **致命** | 账本是审计凭证；接不上就必须让人知道，而不是静默丢日志 |
   | `InitLogDB`（复用主库） | 不可能失败 | 主库已在用 |
   | `dotenv.Load` / `InitRedisClient` | **降级** | 本地没 `.env`、没 Redis 都是正常状态 |

4. 后台 goroutine 全部接收 `ctx`，`Run` 返回前用 `sync.WaitGroup` 等它们退出。

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

// ValidateConfig 只校验不写入：与 UpdateConfig 的接受集合完全一致（同走 setField）
func (m *Manager) ValidateConfig(key, rawJSON string) error

// OnRebuild 注册某域的"重建快照"回调；在 Manager 写锁内被调用，实现方负责原子发布
func (m *Manager) OnRebuild(name string, hook func())

// Modules / FieldCount：供 /api/status 展示（不暴露具体值）
func (m *Manager) Modules() []string
```

支持的字段类型（M0）：`string`、`bool`、`int`、`int64`、`float64`。

**不支持的字段类型在 `Register` 时就 panic，而不是留到 `UpdateConfig` 报错。**
理由：`Register` 只在 `init()` 里被调用，那里出现一个注册不进去的字段是**编码错误**
（写配置域的人以为这个字段可配置，实际永远改不了），必须在启动时就炸出来；
放到 `UpdateConfig` 报错则意味着服务能正常跑、直到有人尝试改那一项才发现。
fail-fast 的位置应该在"错误被引入的地方"，而不是"错误被发现的地方"。

**`ValidateConfig` 为什么单独存在**：配置的写入路径是 `校验 → 落库 → 改内存`
（见 §4.4）。校验必须先于落库——否则一个非法值会先落进库里，再被别的实例同步过去，
一处错误变成处处错误。因此"校验"必须能独立于"写入"被调用。

它把值解析进 `reflect.New(fv.Type()).Elem()` 这个**与真实字段同类型**的临时副本，
再交给同一个 `setField`。类型相同保证了不存在"校验通过但写入失败"的裂缝：
能装进副本的值，就一定能装进字段。

`ValidateConfig` 持**读锁**、`UpdateConfig` 持**写锁**，两者的 key 解析共用
`lookupLocked(key) (*module, field, error)`；写入动作留在各自调用方，
避免出现"持读锁却写真实字段"的并发 bug。

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
                                                    无法应用的脏行：WARN 并跳过（不阻断启动）

改配置：UpdateOption(key, value)                     ★ 唯一写入路径
        ├─ config.GlobalConfig.ValidateConfig()      未注册 key / 类型不符 → 拒绝（此时还没落库）
        ├─ saveOption(key, value)                    upsert 落库（失败则内存一个字节都不改）
        └─ updateOptionMap(key, value)
              ├─ config.GlobalConfig.UpdateConfig()  反射写入注册结构体 + 在写锁内 OnRebuild → 原子发布快照
              ├─ OptionMap[key] = value              内存 map（同步比较基准）
              └─ applySideEffects(key)               非结构化副作用开关（M0：日志级别）

热更新：SyncOptions(ctx, freq)                        定时从 DB 重载全部 Option 行 → 只对变化项走 updateOptionMap
```

- **顺序不可颠倒：校验 → 落库 → 改内存。** 先改内存再落库的话，落库失败会留下
  "这个实例与库不一致"的状态，重启后表现为"配置莫名其妙回退了"，而且排查时看不到任何报错。
- **校验必须在落库之前完成**，因为库里的脏值会被别的实例同步过去——一处错变成处处错。
  这正是 §4.2 里 `ValidateConfig` 需要独立存在的原因：只有它能做到"确认可接受之后再落库"。
- 内存 map 与运行时结构体**必须一起改**，且只有 `updateOptionMap` 一个分发点。
- `applySideEffects` 是一个集中 switch（文档的"一个大 switch"）。M0 里**只有 `system_setting.log_level`
  需要副作用**（调 `logger.SetLevel`）；`status_message`、`enable_access_log`、
  `slow_request_threshold_seconds` 三个字段没有副作用，改完由 `middleware` 与 `/api/status` 直接读快照即可生效。
- `saveOption` 用 `clause.OnConflict{Columns: [{key}], DoUpdates: ["value"]}`，
  让 GORM 按方言生成 `ON DUPLICATE KEY UPDATE`（MySQL）或 `ON CONFLICT DO UPDATE`（PG/SQLite）——
  一份代码三库通用的 upsert。

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
func chooseDB(dsn string, isLog bool) (gorm.Dialector, dbKind, error)
```

| DSN | 驱动 | 备注 |
|---|---|---|
| `postgres://` / `postgresql://` | `gorm.io/driver/postgres` | 强制 `PreferSimpleProtocol=true`（兼容 PgBouncer/Neon） |
| `clickhouse://` / `tcp://` / `http://` / `https://` | 识别但不实现 | 主库遇到 → 返回"仅日志库允许"错误；日志库遇到 → 返回"ClickHouse 支持尚未引入（M6）"错误。分支先留好，**不假装支持** |
| 空 或 `local` / `local:<path>` | `github.com/glebarez/sqlite`（纯 Go，免 CGO） | 空 → `whats-api.db`；`local` → 默认路径；`local:<path>` → 该路径 |
| 其他 | `gorm.io/driver/mysql` | DSN 缺 `parseTime` 时补 `parseTime=true` |

**M0 只本地验证 SQLite**：本机没有 MySQL / PostgreSQL / ClickHouse 服务。
`chooseDB` 的前缀分派、主库拒 ClickHouse、日志库拒未实现驱动，都用单元测试覆盖（断言返回的驱动类型与错误），
**不谎称连过真实 MySQL/PG**。三库端到端验证留到有真实实例时（文档 §8 要求的那条纪律）。

### 5.2 方言 helper（一次到位，避免 M1/M2 回头改）

```go
// 名字以 For 结尾的是纯函数：传入方言名，返回该方言的字面量。
// 后四个是不带 For 的薄包装，它们读当前活连接的方言。
func keyColFor(dialect string) string      // 保留字列 key 的引用写法：PG 用 "key"，MySQL/SQLite 用 `key`
func trueValFor(dialect string) string     // "1"/"0" 或 "true"/"false"
func falseValFor(dialect string) string
func lockForUpdateFor(dialect string, tx *gorm.DB) *gorm.DB  // MySQL/PG 加 FOR UPDATE；SQLite 原样返回

func commonKeyCol() string
func commonTrueVal() / commonFalseVal() string
func lockForUpdate(tx *gorm.DB) *gorm.DB
func isSQLite() / isPostgres() / isMySQL() bool
```

**三处与文档的差异，都是为了不留死代码或让方言分支可测：**

1. **不实现 `commonGroupCol`。** 文档需要它是因为库里有字面名为 `group` 的保留字列。
   本项目按 `11-blueprint.md` §4 的 DDL 骨架，所有分组列统一命名 `group_name`
   （`column:group_name`），从根上避开这个保留字。真正还需要 helper 的是 `key`
   （`tokens.key`、`channels.key`），故保留 `commonKeyCol`。将来若真引入字面 `group` 列，
   再把 `commonGroupCol` 补上。
2. **实现为函数而非变量。** 方言在 `InitDB` 之后才知道；若做成包级变量，
   在 `InitDB` 之前被读到就是空串——静默出错。函数取用没有这个初始化顺序耦合。
3. **核心逻辑做成"传入方言名"的纯函数**（`*For(dialect, ...)`），不带 `For` 的版本只是
   读活连接再转调。这样 PostgreSQL 与 MySQL 两个分支**在没有真实 PG/MySQL 实例的机器上也能测**：
   断言 PG 下生成 `"key"` 与 `true`、且 `lockForUpdateFor` 确实挂上了 `FOR UPDATE` 子句。
   否则这两条分支在 M0 只能靠"看起来对"。

**M0 的现实**：这些 helper 在 M0 只有测试调用（M0 没有裸 SQL，GORM 自己会为标识符加引号）。
它们的价值在于 M1/M2 写条件 UPDATE 与裸 SQL 时**不必回头改数据层约定**，
所以用测试钉住行为而不是等用到才写。

`lockForUpdate` 的存在理由：**绝不用 `tx.Set("gorm:query_option", "FOR UPDATE")`**——GORM v2 会静默忽略它，
"看起来有锁、实际没有"是并发保护里最阴险的一类 bug。

### 5.3 迁移

```go
func Migrate() error {
    if !common.IsMasterNode { logger.Info("非 master 节点，跳过迁移"); return nil }
    if err := DB.AutoMigrate(mainModels()...); err != nil { return err }
    return nil
}

// 主库六张表
func mainModels() []any {
    return []any{&User{}, &Token{}, &Channel{}, &Ability{}, &Option{}, &Setup{}}
}

// 日志库单独迁移（LOG_SQL_DSN 为空时 LOG_DB == DB，等于在主库建这张表）
func migrateLOGDB() error {
    if !common.IsMasterNode { return nil }
    return LOG_DB.AutoMigrate(&Log{})
}
```

- **不预留 `targetedMigrations` 空切片。** 文档草稿里它是"AutoMigrate 管不到的手写 DDL"
  （例如 SQLite 改列必须重建表）的挂载点，但 M0 的所有变更 AutoMigrate 都能表达，
  留一个空注册表等于留一段只有测试会调用的死代码。等 M1/M2 真的需要手写 DDL 时再引入，
  那时它有真实内容、也知道该长什么样。
- 幂等：六张表 + 日志表的建表与 `setup` 首次写入必须可重复执行；验收脚本连续启动三次。
- `logs` 走 `LOG_DB`，不进 `mainModels()`——这是 08 篇"日志库分离"的接缝，
  M0 就把 `logs` 放在日志库，等于**当下就验证了这条接缝是通的**（测试：`LOG_SQL_DSN` 指向另一个
  SQLite 文件时，`logs` 表出现在日志库而主库没有它）。
- ClickHouse 的 MergeTree/TTL 特化不在 M0（驱动都没引，见 §5.1）。

### 5.4 setup

```go
type Setup struct {
    ID            int    `gorm:"primaryKey"`
    Version       string `gorm:"size:32"`
    InitializedAt int64
}
func CheckSetup() error   // 空表则写入第一行；已存在则拒绝（ID 固定为 1）
```

### 5.5 M0 建的表与枚举

字段**以 `11-blueprint.md` §4 的最小 DDL 骨架为准**（不自行扩列）；"M0 状态"列说明该表在本里程碑是否被读写。
所有模型统一用 `ID int` 作主键（GORM `primaryKey`），复合主键表见 `abilities`。

| 表 | 库 | M0 状态 | 关键列（**加粗 = 持久化枚举，ABI**） |
|---|---|---|---|
| `users` | 主 | 仅建表 | `username` UNIQUE、`password_hash`、**`role`**、**`status`**、`quota`、`used_quota`、`group_name`、`email`、`auth_version` |
| `tokens` | 主 | 仅建表 | `key` UNIQUE、`user_id`、**`status`**、`expired_time`、`remain_quota`、`unlimited_quota`、`model_limits_enabled`、`model_limits`(TEXT)、`allow_ips`、`used_quota`、`group_name`、`accessed_at` |
| `channels` | 主 | 仅建表 | **`type`**、`key`、`base_url`、`models`、`group_name`、`priority`、`weight`、`tag`、**`status`**、`model_mapping` / `param_override` / `header_override` / `setting`（全 TEXT） |
| `abilities` | 主 | 仅建表 | 复合主键 (`group_name`, `model_name`, `channel_id`)、`enabled`、`priority`、`weight`、`tag` |
| `logs` | **日志库** | 仅建表 | `user_id`、**`type`**、`model_name`、`quota`、`prompt_tokens`、`completion_tokens`、`use_time`、`is_stream`、`channel_id`、`group_name`、`token_name`、`ip`、`request_id`、`other`(TEXT) |
| `options` | 主 | ★读写 | `key` PK、`value` |
| `setup` | 主 | ★读写 | `id` PK、`version`、`initialized_at` |

**贯穿所有表的五条约定**（照 08 篇规则）：

1. 时间戳统一 `int64` unix 秒（前端 JS 不丢精度、三库行为一致），不用 `time.Time`；
2. 金额/额度统一 `int64` 整数，永不 float；
3. 所有 JSON 型字段用 `TEXT`（`model_mapping`、`param_override`、`header_override`、`setting`、
   `model_limits`、`other`），不碰数据库特有 JSON 类型；Go 侧以 `string` 承载、显式编解码；
4. **业务默认值写在代码里，不写进 GORM tag**：`enabled`、`unlimited_quota`、`status`、`priority`、
   `weight` 等由写入方赋值（规则③：`gorm:"default:true"` 会让 AutoMigrate 每次重启发 ALTER TABLE）；
5. 主键交给 GORM（`gorm:"primaryKey"`），不手写 `AUTO_INCREMENT` / `SERIAL`。

**枚举（`internal/constant`，ABI：只增不改）**：

```
Role:          User=1,  Admin=10, Root=100
UserStatus:    Enabled=1, Disabled=2
ChannelStatus: Enabled=1, ManuallyDisabled=2, AutoDisabled=3
TokenStatus:   Enabled=1, Disabled=2, Expired=3, Exhausted=4
LogType:       Unknown=0, Consume=1, Topup=2, Manage=3, System=4, Error=5, Refund=6, Login=7
```

**这是本项目自定的编号**，不追求与 New API 数值一致——数值一旦落库就是 ABI，
对齐一个外部项目的编号没有收益、只有风险。测试钉住这些数值：改编号必须让测试失败，
因为线上库里已经存着旧值（`ChannelStatus` 的 `ManuallyDisabled` / `AutoDisabled` 从一开始就分开，
正是为了 M4 那条"手动禁用的渠道不会被自动恢复"）。

`RebuildAbilities()`（整表重建）是 08 篇明确要求的兜底，但它是 M4 的职责，M0 不实现——
此时没有渠道写入路径，它没有数据源。M4 必须先补它再补选路。

---

## 6. 缓存与 Redis 退化

```go
// internal/common/redis.go
var (
    RedisEnabled bool          // REDIS_CONN_STRING 有效且 PING 通过
    RDB          *redis.Client // 未启用时为 nil
)
func InitRedisClient()   // 非致命：连接失败 → RedisEnabled=false + WARN，继续启动
func NormalizeRedisURL(raw string) string
func RedisGet / RedisSet / RedisDel / RedisIncr / RedisKeyCacheSeconds
func CloseRedis() error
```

```go
// internal/cachex — M0 只有一种原语：计数器
type Counter struct { namespace string; ttl time.Duration }
func NewCounter(namespace string, ttl time.Duration) *Counter
func (c *Counter) Incr(key string) (int64, error)  // Redis: INCR + EXPIRE 走 pipeline
func (c *Counter) Get(key string) (int64, bool)
func (c *Counter) Backend() string                  // "redis" / "memory"
```

- Redis 可用 → 走 Redis；不可用 → 进程内实现（`sync.Map` + 滑动 TTL）。
- 两种后端的语义差异（TTL 精度、原子性）封在实现内部；`Incr` 在内存后端用
  `sync.Mutex` + 计数器保证原子。Redis 路径运行期失败时**逐次降级到内存**，
  而不是让一次 Redis 抖动升级成请求失败。
- **M0 的真实用途**：`requests_total` 计数器（`/api/status` 暴露）。
  这样"拔掉 Redis 也能启动并继续工作"不是空话，是可断言的观察点。
- 不使用 Redis 做配置同步（那是 `SyncOptions` 的 DB 轮询路径，M0 不引入 Redis 发布订阅）。

### 6.1 相对文档草稿的缩减：`HybridCache[V]` 推迟

文档草稿（08 篇 §5）的 `HybridCache[V]` + `ValueCodec[V]` 是通用缓存容器，
配套 `RedisHSetObj` / `RedisHGetObj` 两个哈希对象读写函数。**M0 不实现它们。**

理由：M0 既没有缓存对象要存，也没有第二个使用者。一个只有测试调用的泛型容器
加编解码接口，是典型的"提前抽象"——它的接口形状是靠猜的，而猜错的抽象
比没有抽象更贵（后面每个使用点都要绕过它或改它）。

M0 真正需要的能力只有一件：**一个跨实例的计数器**，于是只做 `cachex.Counter`。
它已经完整覆盖了这条验收标准要证的东西：

- Redis 可用 → 计数在 Redis 里（多实例共享）；
- Redis 不可用/配错/运行期挂掉 → 自动降级为进程内计数，服务照常启动与运行；
- `/api/status` 的 `cache.backend` 与 `requests_total` 让这件事**可被观察**。

`HybridCache[V]` / `ValueCodec[V]` / `RedisHSetObj` / `RedisHGetObj` 推迟到
第一个真正需要缓存对象的里程碑（M1 的 token 鉴权缓存、M4 的渠道索引缓存），
那时它们的接口形状由真实使用点决定。同一原则也适用于 §5.2 的
`commonGroupCol`（M0 没有裸 SQL 用它）与 §5.3 的 `targetedMigrations`
（M0 的变更 AutoMigrate 全能表达）：**空注册表不是接缝，是死代码。**

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
type Deps struct {
    Requests  *cachex.Counter // /api/status 的 requests_total
    StartedAt time.Time       // uptime_seconds
}

func SetRouter(engine *gin.Engine, deps Deps) {
    engine.Use(/* 六层全局中间件，顺序见 §7.1 */)

    setAPIRouter(engine, deps)   // /api  管理面
    setRelayRouter(engine)       // /v1   转发面（仅骨架，不注册端点）
    setWebRouter(engine)         // NoRoute 兜底
}
```

进程级依赖（计数器、启动时间）由 `app.Run` 创建并显式传入，不经全局变量——
与 §2 的"全局变量规则"一致：只有真单例句柄才允许做包级变量。

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

| 依赖 | 用途 | 版本 |
|---|---|---|
| `github.com/gin-gonic/gin` | HTTP | v1.12.0 |
| `gorm.io/gorm` | ORM | v1.31.2 |
| `gorm.io/driver/mysql` | MySQL | v1.6.0 |
| `gorm.io/driver/postgres` | PostgreSQL | v1.6.3 |
| `github.com/glebarez/sqlite` | 纯 Go SQLite（免 CGO） | v1.11.0（modernc.org/sqlite v1.23.1） |
| `github.com/go-redis/redis/v8` | Redis（文档指定 v8） | v8.11.5 |
| `github.com/shopspring/decimal` | `QuotaFromDecimal` | v1.5.0 |
| `github.com/joho/godotenv` | `.env` | v1.5.1 |

**不引入**：测试框架（用标准 `testing`）、日志库（用标准 `log/slog`）、内存 LRU 库（`cachex` 自己实现）、
ClickHouse 驱动。ClickHouse 的 DSN 前缀在 `chooseDB` 里被**识别但拒绝**（主库→"仅日志库允许"、
日志库→"尚未引入"），失败信息明确指向未来里程碑，而不是静默按 MySQL 去连。

**风险已消除（规划期实测）**：上表全部依赖在 `go1.23.2 windows/amd64` 下 `go mod tidy` + `go build ./...`
均成功（临时模块实测，退出码 0）。纯 Go SQLite 驱动可用，**不需要 CGO 回退**，
Docker 构建可保持 `CGO_ENABLED=0`。上表版本即为实测通过的版本，实施时按此锁定。

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
| `internal/common/env_test.go` | 零环境变量时的默认值；非法数值回退默认值；`SYNC_FREQUENCY` 下限钳制并产生告警 |
| `internal/logger/logger_test.go` | `SetLevel` 立即生效（不是下次重载才生效）；`SetupLogger` 之前调用 `Logger()` 不 panic |
| `internal/cachex/counter_test.go` | Redis 不可用时的内存后端；并发 `Incr` 不丢计数（`-race` 下跑）；不同 key 互不干扰 |
| `internal/setting/config/config_test.go` | key 生成（`module.jsonField`）、`ExportAllConfigs`、反射写回五种类型、未知 key 报错、类型不符报错且不写入、**`ValidateConfig` 无副作用且接受集合与 `UpdateConfig` 一致**、缺 json tag / 类型不支持时 `Register` panic、重复注册 panic |
| `internal/setting/system_setting/system_setting_test.go` | 快照原子替换；并发读到的一定是完整版本（`-race` 下跑）；旧快照不被改写 |
| `internal/model/main_test.go` | `chooseDB` 前缀分派（含主库拒 ClickHouse、日志库拒未实现驱动）、`lockForUpdate` 在 SQLite 下不产生 `FOR UPDATE`（且在 MySQL/PG 下**确实产生**——挡住 GORM v2 静默忽略 `gorm:query_option` 的坑）、`AutoMigrate` 连续三次幂等、非 master 不迁移、`logs` 落在 `LOG_SQL_DSN` 指定的库而主库无此表、**列名与复合主键**（列名是 ABI；同时钉住"无 `deleted_at` 软删除列"） |
| `internal/constant/constants_test.go` | 枚举数值 golden 断言：任何重编号都必须让测试失败（ABI 保护） |
| `internal/model/option_test.go` | `InitOptionMap` 默认值 → DB 覆盖、脏数据跳过不阻断启动；`UpdateOption` 单一路径三者同时变；**非法输入一个字节都不写**（库里不留行）；`log_level` 副作用立即生效；`SyncOptions` 在外部改库后收敛且**只应用变化项** |
| `internal/middleware/middleware_test.go` | request id 入站沿用/缺失生成/可从 context 读取；panic → 500 JSON **且不泄漏堆栈**；route tag 按前缀分派；访问日志可热关闭但**计数继续**；CORS 通配、白名单回显带 `Vary`、白名单外不回显 ACAO |
| `internal/controller/status_test.go` | `/api/status` 的 JSON 形状逐字段断言（验收③的观察窗）；配置改动立即反映；**响应体不含 DSN 全文与密钥** |
| `internal/router/router_test.go` | `/api/status` 可达且中间件链生效；`/v1/*` 与 `/api/*` 未实现路径返回 JSON 404，**不被 SPA 兜底吞成 200+HTML** |
| `internal/app/app_test.go` | `Run(ctx)` 取消后：在途请求拿到完整响应、DB 句柄已关闭并置空、后台 goroutine 已退出；端口占用时快速返回错误 |
| `cmd/optctl/main_test.go` | `set` → `get` 往返；`list` 含全部注册项；非法输入报错且**不落库**（它是唯一能从进程外改配置的入口） |

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
4 feat(model): 七张核心表模型（含落在日志库的 logs）、三库兼容的 DB 初始化与幂等迁移
5 feat(model): Option 表与配置唯一写入路径（UpdateOption / SyncOptions）
6 feat(http): 中间件链、路由分组与 /api/status
7 feat(app): 启动时序与优雅停机
8 test: M0 验收脚本
9 docs: README、环境变量清单与实施文档
```

实施时每个任务内部还会按 TDD 再拆提交（"写测试 → 实现 → 通过"各一次），
因此实际提交数会多于上表的 9 条。这是有意的：**每条提交都应该是可编译、
可测试的状态**，便于 review 时逐条回滚。

### 实施计划（本设计的两份计划文件）

| 计划 | 范围 | 完成标志 |
|---|---|---|
| `plans/2026-10-12-m0-foundation.md` | 基础层：模块骨架、常量、环境变量、JSON、数值安全、logger、缓存、配置注册表与快照 | `go build ./... && go vet ./... && go test -race ./...` 全绿（尚无可运行的二进制） |
| `plans/2026-10-12-m0-runtime.md` | 运行时与交付：七个模型与迁移、配置写入路径、中间件、controller、router、`app.Run`、optctl、Docker、README、验收脚本 | 四条验收标准（本机可验证部分）通过 |

拆成两份的理由：**每份计划结束时都处于一个可独立验证的状态**。
基础层全绿之后再开始写运行时，任何失败都能立刻定位到"是这一层引入的"。

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
