# 01 · 全景：这个项目到底在解决什么问题

> 本文是整套分析的第一篇。目标不是复述代码，而是先建立一个"骨架认知"：一个 AI 网关由哪几块组成、它们之间的依赖方向、哪些是本质复杂度、哪些只是这个项目的实现选择。

## 1. 一句话定义

**它把几十家上游模型服务，收敛成一套 API + 一套账号体系 + 一套计费账本，再用一个控制台把它们运营起来。**

拆开看是三个身份：

| 身份 | 对谁负责 | 核心产物 |
|---|---|---|
| 协议网关 | 下游客户端（OpenAI/Claude/Gemini SDK、Cursor、Cherry Studio…） | 四种协议互转、SSE 转发 |
| 资源编排器 | 上游供应商（OpenAI、Anthropic、Bedrock、通义…） | 渠道、模型映射、负载均衡、故障转移 |
| 运营系统 | 平台方与用户 | 用户/令牌/分组、配额与账本、审计日志、控制台 |

这三个身份对应三组完全不同的复杂度，代码里也是分开的。**新手自建平台最常见的失败是只做了第一个身份**（写个反向代理就以为成了网关），然后在加第二个身份时发现抽象选错了，被迫重写。

## 2. 本质职责拆解（自建时按这个清单规划）

一个可运营的 AI 网关，绕不开这五件事：

1. **统一协议**：下游说什么协议、上游要什么协议，两者自由组合。
2. **路由与编排**：一个模型名 → 一个可用渠道 → 一次（或多次）上下游调用。
3. **计量与计费**：把上游返回的 usage 变成钱，且不发生透支、重复扣费、超卖。
4. **鉴权与访问控制**：谁是用户、这把 key 能用哪些模型、能花多少钱、来自哪些 IP。
5. **运营控制面**：让管理员能改配置、看日志、测渠道、管余额，而不用重启服务。

第 3 和第 4 是**真正难的部分**，也是这类项目 90% 的 bug 所在。协议转换听起来花哨，其实是纯函数，最好写、最好测（所以这个项目把它拆成了独立 module）。

## 3. 模块划分与依赖方向

仓库是 **3 个 Go module**（没有 `go.work`，用 `replace` 指向本地目录）：

| module | 路径 | Go 版本 | 职责 |
|---|---|---|---|
| 根模块 | `github.com/QuantumNous/new-api` | 1.26 | 宿主：HTTP、DB、渠道、计费、鉴权、控制台 |
| `relaykit/` | `.../relaykit` | 1.25.1 | 四协议请求/响应/流式互转（纯数据转换） |
| `tokenkit/` | `.../tokenkit` | 1.26 | 文本与图片 token 计数/估算 |

依赖方向是**单向**的，这条规则被写进了项目规范（`AGENTS.md`）并被 CI 式检查：

```
根模块 ──→ relaykit      relaykit 不得反向 import 根模块
根模块 ──→ tokenkit      tokenkit 不得依赖 relaykit
relaykit ✗ tokenkit      （两者互不依赖）
```

**为什么值得学这个设计：** 协议转换是这个项目里最值钱、最通用、最容易单独复用的部分。把它做成独立 module，意味着：

- 可以用 `GOWORK=off go build ./...` 单独验证；
- 能被别的 Go 网关直接 `go get` 嵌进去（README 里明确了这个用意）；
- 宿主的所有副作用（HTTP、SSE 读写、鉴权、计费）通过**钩子/接口**注入，而不是让 relaykit 反向依赖它们。

具体的注入点：
- `relayconvert.SetMediaResolver(...)`：跨协议转换需要下载图片时，由宿主提供下载函数；
- `kitutil.SetCodec(...)`：JSON 编解码实现可替换（宿主换成自己的 `common.Marshal` 体系）；
- `kitutil.SetLogging(...)`：转换器的诊断日志交给宿主；
- `convmeta.Meta` 接口：转换需要的上下文（模型名、推理设置、工具编码记录、流式状态），宿主用 `RelayInfo` 直接实现，避免 relaykit 知道宿主的请求对象。

> **可复用的判断标准：** 一段代码没有 DB、没有 HTTP、没有全局配置、没有时间/随机数依赖，它就能独立成 module。协议转换满足，计费不满足（要读定价表、写账本），所以计费留在宿主。

## 4. 技术选型与代价

| 领域 | 选择 | 代价 / 注意 |
|---|---|---|
| Web 框架 | Gin 1.9 | 生态最大；中间件模型简单，适合做"洋葱式"请求流水线 |
| ORM | GORM v2 | 多数据库支持好；但 raw SQL 的方言差异要自己兜（见 08 篇） |
| 数据库 | SQLite / MySQL≥5.7.8 / PostgreSQL≥9.6 **三库同时支持** | 这是本项目最重的工程约束之一，直接决定了很多写法 |
| 日志库 | 独立配置的 log DB，额外支持 ClickHouse | 日志量远大于业务数据，物理隔离是刚需 |
| 缓存 | Redis + 进程内缓存双模 | `REDIS_CONN_STRING` 未配时退化为内存缓存，靠定时同步保证多节点最终一致 |
| 鉴权 | Session + JWT + API Token + WebAuthn + TOTP + OAuth/OIDC | 认证是企业级网关的标配，也是最容易出安全事故的地方 |
| 授权 | Casbin（`service/authz/`） | 资源-动作型权限矩阵，策略热重载 |
| 计费表达式 | `expr-lang/expr`（`pkg/billingexpr/`） | 让定价可配置成表达式，而不是硬编码公式 |
| JSON | `gjson` / `sjson`（免反序列化的字段读写） | 转发路径上频繁改单个字段时，比 struct 往返便宜得多 |
| 前端 | React 19 + Rsbuild + TanStack Router/Query/Table + Zustand + Tailwind 4 + Base UI | 见 10 篇；注意它用的是 `tsgo`/`oxlint`/`oxfmt`/Bun，不是常规 npm+Vite 组合 |
| 异步任务 | JS 插件（moejs 运行时）+ Go 侧轮询框架 | 让新增视频/音乐供应商不必改 Go 代码 |
| 可观测 | Prometheus + Pyroscope + pprof + 自研 perf_metrics | 网关是长连接密集型服务，性能剖析必须内建 |

## 5. 目录地图

```
main.go                  进程入口：初始化 → 后台任务 → HTTP 服务 → 优雅停机
router/                  路由注册（按功能拆文件：relay / api / web / task / video / plugin）
middleware/              41 个中间件：认证、限流、渠道分发、性能保护、请求体清理…
controller/              HTTP handler：relay 面 + 管理面
relay/                   ★ 转发内核（248 个文件）
  ├── channel/            每个供应商一个目录（~38 个）+ adapter.go + api_request.go
  ├── common/             RelayInfo / ChannelMeta：贯穿全链路的请求上下文
  ├── helper/             SSE 扫描、模型映射、价格计算、请求校验
  └── constant/           RelayMode 与路径映射
relaykit/                ★ 独立 module：四协议互转
tokenkit/                独立 module：token 计数
service/                 业务逻辑（111 个文件）：渠道选择、计费、任务、订阅…
model/                   GORM 模型与缓存（112 个文件）：channel / user / token / log / option…
setting/                 运行时可配置项，分域组织（billing / operation / model / console…）
pkg/                     内部工具库：billingexpr / jsplugin / wsmanager / cachex / perf_metrics
constant/ types/ dto/    常量、共享类型、数据传输对象
plugins/tasks/           JS 任务插件（每个供应商一个 plugin.js）
web/                     React 控制台（1386 个文件），构建产物被 go:embed 进二进制
electron/                桌面端壳
```

规模参考：后端 ~1033 个 Go 文件、前端 1386 个文件。**不要试图一次读完**，按本篇第 2 节的五件事分头看。

## 6. 启动时序

`main.go` 的顺序本身就是一份"依赖关系说明书"。分两个阶段：

### 阶段 A：`InitResources()`（资源初始化，按序）

```
1  godotenv.Load(".env")                // 环境变量
2  common.InitEnv()                     // 配置装载
3  logger.SetupLogger()
4  ratio_setting.InitRatioSettings()    // 定价倍率
5  service.InitHttpClient()             // 出站 HTTP 客户端（长连接、超时策略）
6  model.InitDB()                       // 主库（致命）
7  authz.Init(model.DB)                 // Casbin 授权（依赖 DB）
8  model.InitPasswordEncryption()       // 可选的密码登录加密
9  model.CheckSetup()                   // 首次安装标志
10 model.InitOptionMap()                // ★ 运行时配置表（必须先于一切读取配置的逻辑）
11 model.InitLogDB()                    // 独立日志库（含 ClickHouse）
12 common.InitRedisClient()
13 perfmetrics.Init() / StartSystemMonitor()
14 i18n.Init()                          // 非致命
15 oauth.LoadCustomProviders()          // 从 DB 载入自定义 OAuth（非致命）
16 service.StartAuthArtifactCleanup()
```

**注意第 10 步的位置：** 大量模块在自己的 `init()` 或构造时读配置，所以"配置表加载"必须在它们之前。这是这类项目最容易踩的顺序坑。

### 阶段 B：`main()` 主体

```
1  kitutil.SetLogging/SetSystemErrorLogging   // 把 relaykit 的日志钩子接到宿主 logger
2  defer model.CloseDB()
3  model.InitChannelCache()  + go model.SyncChannelCache(SyncFrequency)   // ★ 渠道缓存
4  wsmanager.StartSubscriber()                // WebSocket 跨节点消息订阅
5  model.GetPricing()                         // 预热定价缓存（必须在渠道缓存之后）
6  go model.SyncOptions(SyncFrequency)        // ★ 配置热更新
7  go controller.SyncTaskPlugins()            // 任务插件热同步
8  go authz.StartPolicySync(SyncFrequency)    // 授权策略热重载（多节点传播）
9  go model.UpdateQuotaData()                 // 看板数据
10 service.StartCodexCredentialAutoRefreshTask()   // 上游凭证自动续期
11 service.StartSubscriptionQuotaResetTask()       // 订阅额度周期重置
12 service.StartSystemInstanceReporter()           // 多实例存活上报
13 service.GetTaskAdaptorFunc = ...                // 打破 service → relay 的 import 环
14 controller.RegisterScheduledSystemTasks() + service.StartSystemTaskRunner()
15 if BATCH_UPDATE_ENABLED: model.InitBatchUpdater()  // ★ 批量落库
16 if ENABLE_PPROF: pprof + Monitor()
17 common.StartPyroScope()
18 gin.New() → ConfigureTrustedProxies → CustomRecovery → RequestId → Version → I18n → Logger
19 InjectUmami/GoogleAnalytics（改写内嵌 index.html 占位符）
20 router.SetRouter(server, WebAssets{buildFS, indexPage})
21 ListenAndServe(:PORT)
22 阻塞等待 SIGINT/SIGTERM → srv.Shutdown(默认 120s)
```

三个值得抄的设计：

- **第 13 步的 `GetTaskAdaptorFunc` 函数变量注入**：`service` 需要调用 `relay` 的能力，但 `relay` 又依赖 `service`，形成 import 环。解法不是拆包，而是在 `service` 里留一个函数变量，在 `main` 里赋值。轻量、显式、无框架魔法。
- **第 14 步的调度器**：周期性任务（渠道测试、模型列表更新、异步任务轮询）统一注册进"系统任务"框架，带 **DB 租约去重**（多 master 只有一个节点执行）和运行历史。多副本部署下这是刚需。
- **第 22 步的停机策略**：SSE 流可能跑几分钟，所以给 120 秒（`SHUTDOWN_TIMEOUT_SECONDS`）优雅退出，并在退出前把内存里的看板数据落库，避免重启丢数据。

## 7. 部署形态（复杂度的现实来源）

`docker-compose.yml` 暴露了三种部署档次，架构上都能跑同一份代码：

| 档次 | 形态 | 关键环境变量 |
|---|---|---|
| 单机 | SQLite + 内存缓存 | 无 |
| 标准 | PostgreSQL/MySQL + Redis | `SQL_DSN`、`REDIS_CONN_STRING` |
| 大规模 | 主库 + **独立日志库（ClickHouse）** + 多实例 | `LOG_SQL_DSN`、`LOG_SQL_CLICKHOUSE_TTL_DAYS`、`SESSION_SECRET`、`NODE_NAME`、`SYNC_FREQUENCY` |

多实例部署带出的连锁设计：`SESSION_SECRET` 必须共享、渠道缓存靠 `SYNC_FREQUENCY` 定时同步、授权策略要跨节点传播、系统任务要靠 DB 租约防重复执行、审计日志要带 `NODE_NAME` 标识来源。

**自建建议：** 第一版只做"单机 SQLite + 内存缓存"，但**所有配置项都从第一天就走数据库表 + 内存缓存 + 定时同步**这条路，而不是写死在 yaml 里。否则等到要多实例时，配置热更新、缓存失效、租约这些机制都得重做。

## 8. 你的第一版骨架

如果现在从零开始，按依赖顺序落地：

```
1. model 层     ：user / token / channel / ability / log / option 六张核心表 + AutoMigrate
2. option 系统  ：配置表 + 内存 map + 定时 sync（先于所有业务逻辑）
3. relay 最小闭环：/v1/chat/completions → 选渠道 → 转发 → 透传 SSE
4. token 鉴权   ：API key → 用户 + 分组 + 模型白名单
5. 计费闭环     ：预扣 → 结算 → 失败退款（先只做 token 计价）
6. 控制台       ：渠道 CRUD + 令牌 CRUD + 日志查询
7. 之后按需加   ：协议互转（引入 relaykit 或自己写）、异步任务、插件体系
```

第 3 步故意先不做协议转换：**先让"同协议透传"跑通全链路**（鉴权、选渠道、计费、日志），再引入转换。否则你会在协议细节里迷路，而链路上的账务错误更致命。

## 9. 本篇索引

- 启动入口：`main.go`
- 路由总装：`router/main.go`
- 三个 module：`go.mod` / `relaykit/go.mod` / `tokenkit/go.mod`
- 部署样例：`docker-compose.yml`（环境变量即配置清单）

→ 继续阅读：[02 · 请求生命周期](02-request-lifecycle.md)
