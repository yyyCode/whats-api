# 09 · 异步任务与插件体系

> 从"聊天"到"生成一段视频"，请求模型从**同步 3 秒**变成**异步 5 分钟**。这带来四类新问题：任务状态持久化、轮询与结算、超时与退款、以及"新供应商以每周一个的速度出现"如何不拖垮发版节奏。本篇讲这三层机制。

## 1. 三层异步工作

| 层 | 做什么 | 周期 | 实现 |
|---|---|---|---|
| **系统任务** | 渠道测试、模型列表更新、任务轮询的**调度** | 分钟级 | `SystemTask` + DB 租约 |
| **异步 relay 任务** | 用户提交的视频/音乐/MJ 生成 | 秒级轮询 | `Task` + `TaskPollingAdaptor` |
| **JS 插件** | 定义"怎么和某家视频厂商对话" | 按需热更新 | `pkg/jsplugin` + `plugins/tasks/*` |

**这三层是正交的**，理解这个分解比理解任何一层都重要：

```
调度层（谁在什么时刻跑）  ← 系统任务框架，与业务无关
   ↓
业务层（任务的状态与结算） ← Task 模型 + 轮询 + 计费
   ↓
适配层（怎么对接某家厂商） ← Go 适配器 或 JS 插件
```

## 2. 系统任务框架：DB 租约的分布式调度

多实例部署下，"每分钟跑一次渠道测试"不能真的每个实例都跑。项目没有引入 Redis 锁或外部调度器，而是用**数据库租约**：

```go
// model/system_task.go
type SystemTask struct {
	TaskID    string
	Type      string         // log_cleanup / channel_test / model_update / midjourney_poll / async_task_poll
	Status    string
	ActiveKey string         // ★ 每种类型唯一 → 天然去重
	Payload, State, Result
	LockedBy  string
	// ...
}

type SystemTaskLock struct {   // ★ 每种类型一条租约
	Type       string
	LockedBy   string
	LockedUntil time.Time
}
```

### 运行机制（`service/system_task.go`）

```
StartSystemTaskRunner()
  ├─ runSystemTaskScheduler()    按注册的周期决定是否该跑
  ├─ runSystemTaskClaimPass()    尝试抢占（抢 DB 租约）
  └─ runWithLeaseHeartbeat()     ★ 执行期间心跳续租；
                                  租约丢失 → 取消 handler 的 context
                                  （model.ErrSystemTaskLockLost）
EnqueueSystemTask()              手动触发（如管理员点"测试所有渠道"）
```

**四个关键设计：**

**① `ActiveKey` 唯一约束做去重。** 同类型的任务只会有一条活跃记录，天然防止重复创建。

**② 租约心跳能"取消正在跑的过期任务"。** 如果一个节点卡死（比如 GC 停顿、网络分区），它的租约到期后，别的节点会接管。此时**原节点的心跳续租会失败，从而主动取消自己的执行**——避免"两个节点同时跑同一个任务"。

这是分布式锁里最容易漏的一环：**加锁不够，还要有"失去锁时停止工作"的机制。** 很多实现只做到"抢锁后不管了"，结果脑裂时两个节点同时写数据。

**③ master-only 执行 + `Enabled()` 空闲检测。** `Enabled()` 会判断"现在真的有事要做吗"（比如没有未完成的 MJ 任务就不创建轮询任务），从而**避免产生无意义的 DB 行**。

**④ 任务带运行历史**（`Status`/`Result`/`Payload`/`State`），管理员可以在控制台看到"上次渠道测试什么时候跑的、结果如何"。**调度器不只要能跑，还要能被观测。**

### 注册一个定时任务

```go
// controller/system_task_handlers.go:19
RegisterScheduledSystemTasks()
  ├─ channelTestHandler       ← 周期来自 monitor_setting
  ├─ modelUpdateHandler       ← 更新上游模型列表
  ├─ midjourneyPollHandler    ← 15 秒，且仅在存在未完成 MJ 任务时
  └─ asyncTaskPollHandler     ← 15 秒，调用 service.RunTaskPollingOnce
```

**这个框架的通用价值：** 任何"周期性 + 多节点只能跑一次 + 需要可观测 + 需要手动触发"的后台工作，都可以复用同一套。项目里渠道测试、模型同步、任务轮询就是这样统一的。

**自建建议：** 不要一开始就上 Redis 分布式锁或外部队列。**先用 DB 租约**——它零依赖、可观测（能查表看到谁持有）、语义清晰，几千 QPS 的场景完全够用。

## 3. 异步 relay 任务：状态机 + 轮询 + 恰好一次结算

### 任务状态机（`model/task.go:50`）

```
NOT_START → QUEUED → SUBMITTED → IN_PROGRESS → SUCCESS
                                             ↘ FAILURE
```

`TaskStatus.ToVideoStatus()` 把它映射成对外 API 的视频状态。**内部状态与对外状态分开映射**是必要的：内部状态会随供应商增加而细化，对外状态要保持稳定。

`Task` 模型的关键字段：`TaskID`（供应商侧 ID）、`Platform`、`UserId`、`ChannelId`、`Action`、`Status`、`Progress`、`SubmitTime/StartTime/FinishTime`、`Properties`、`PrivateData`（★ 存 `BillingContext` 计价快照）、`Data`（结果）。

### 提交

`relay/relay_task.go` 的提交流程：

```
1. 解析请求 → 选渠道（插件绑定的渠道，见 §4）
2. 计费：ModelPriceHelperPerCall（按次）或任务用量表达式
   → info.ForcePreConsume = true → PreConsumeBilling 全额预扣
3. 调上游提交接口（TaskAdaptor.BuildRequestURL/Header/Body → DoRequest → ParseResponse）
4. ★ 任务落库（durable）之后再 SettleBilling
```

**第 4 步的顺序很重要：** 只有任务被持久化后，预扣才算"落定"。否则上游提交成功但本地入库失败 → 用户被扣款却查不到任务。

`ResolveOriginTask` 支持 remix / 续作（基于已有任务创建新任务）。

### 轮询

`service/task_polling.go`：

```go
type TaskPollingAdaptor interface {
	Init(...)
	FetchTask(...)          // 查上游任务状态
	ParseTaskResult(...)    // 解析成内部状态
	AdjustBillingOnComplete(...)  // 完成后调整计费
}
type BatchTaskPollingAdaptor interface { ... }  // 批量查，减少上游请求数

func RunTaskPollingOnce(ctx, report)   // 由 async_task_poll 系统任务每 15 秒调用
```

轮询是**批量**的（`BatchTaskPollingAdaptor`）：一次向上游查多个任务，而不是每任务一次请求。**这是异步网关的性能命门**——1 万个在途任务如果每个都单独轮询，上游会先封了你。

### ★ 恰好一次结算

任务完成可能被多次观察到（多个节点的轮询、重复的批次）。项目用 **CAS 状态转换** 选举执行者：

```go
// service/task_polling.go:607-627
win := task.UpdateWithStatus(...)   // 条件更新：只有状态真正发生转换时才返回 true
if !win { return }                  // 输的节点直接跳过，不结算
finalizeTerminalTask(...)           // 赢家执行结算
```

结算优先级（`settleTaskBillingOnComplete`）：

```
① 分层/表达式快照（有 TieredSnapshot 时按快照重算）
② 保持按次计费
③ 适配器的 AdjustBillingOnComplete（供应商特有的调整，如实际时长）
④ 按 token 重算（RecalculateTaskQuotaByTokens）
失败 → RefundTaskQuota
```

**"跨时间的计费必须存快照"** 在这里体现得最直接：任务提交时和完成时可能相隔几十分钟，期间管理员可能改了价格。计价要素存在 `task.PrivateData.BillingContext`（`TaskBillingContext`：ModelPrice、GroupRatio、ModelRatio、OtherRatios、OriginModelName、PerCallBilling、TieredSnapshot），完成时按它算。

### 超时清扫

```go
sweepTimedOutTasks(ctx, report)   // service/task_polling.go:70
  → 用 CAS 把超时任务置为失败
  → RefundTaskQuota 退款
```

**没有清扫器的异步系统迟早会积压一堆"永远在跑"的任务**，把用户的钱一直冻着。清扫是必需的。

## 4. JS 插件体系：让新增供应商不用发版

### 为什么用 JS

供应商适配器是 Go 代码（见 03 篇），但**视频/音乐类供应商的差异主要在协议细节和流程编排上**，且新增速度极快。把它们做成 Go 适配器意味着：每接一家 → 改代码 → 编译 → 发版 → 升级所有实例。

所以这部分被做成了 **JS 插件**：用 moejs 运行，插件源码存在数据库里，**可以在控制台安装、切换版本、启用/禁用，所有节点 30 秒内自动同步**。

### 引擎的约束（`pkg/jsplugin/engine.go`）

```go
Engine.Compile(source, Options)
  ├─ ★ 禁止 async/await/import（forbiddenSyntax）→ 插件必须是同步的
  ├─ runtime 池化（runtimeInstance），并发上限默认 8
  ├─ 钩子句柄缓存上限 256（maxCachedHooks）
  └─ 默认超时 5 秒
HookError / ResultError  统一清洗 JS 异常，不泄漏宿主细节
```

**"禁止 async/await/import"是一个很关键的约束。** 它的意义是：

- 插件脚本无法发起不受控的异步操作（网络请求必须走宿主提供的 `utils.fetch`）；
- 每个钩子调用都是可预测的短时同步函数，超时可严格约束；
- 不引入模块加载，沙箱边界更清晰。

**沙箱设计的一般原则：不是"禁止所有危险 API"，而是"把副作用收窄到少数几个宿主提供的入口"。** 这里网络出口就是 `pkg/jsplugin/fetch.go`，且受 `allowedHosts` 白名单限制。

### 插件清单（`pkg/jsplugin/registry.go` 的 `Meta`）

```go
type Meta struct {
	Key, Version, APIVersion string
	ChannelTypes []int         // 支持的渠道类型
	Models       []string
	FetchMode    string
	AllowedHosts []string      // ★ 网络白名单
	Upstreams    []Upstream    // vendor / new_api 两种上游形态
	Routes       []Route       // 插件自己声明的路由
	Protocols    []string
	UsageSchema  ...           // ★ 用量 schema（驱动计费）
	Auth         ...
	Capabilities []string      // 如 json-order@1、duration-auto@1
}
```

**`UsageSchema` 是最值得注意的部分：** 插件声明"我会上报哪些用量字段"（图片张数、视频秒数…），宿主据此驱动计费和 UI 渲染。项目规范里对这块有很细的要求（描述必须写"计费主体 + 单价"，因为那个字段会作为价格输入显示在界面上）。

**"扩展点自己声明计量字段，宿主持有计量规则"是插件计费的正确分工。** 如果让插件直接返回金额，就无法统一审计和配置定价。

### 版本管理与同步

```go
// model/task_plugin.go:78
type TaskPlugin struct {
	Key, Version string   // 唯一
	Source       string   // 插件源码
	SourceHash   string
	Icon         string
	Enabled      bool
	Active       bool      // ★ 同 key 只有一个版本 Active
}
```

- **内置插件**：`plugins/embed.go` 用 `//go:embed tasks` 打进二进制（`alibaba`、`doubao`、`google`、`hailuo`、`jimeng`、`kling`、`sora`、`sunoapi`、`vertex-ai`、`vidu`、`xai`）；
- **DB 覆盖**：控制台可以安装/覆盖，`GetTaskPluginSyncSnapshot()` 暴露一个 **revision hash**，各节点每 30 秒轮询比对，变了才重新加载（`controller.SyncTaskPlugins`）；
- **版本安全**：`SaveTaskPlugin` 拒绝"同 key+version 但源码变更"（版本号必须递增，防止不可复现的安装）；
- **激活/回滚**：`ActivateTaskPlugin` / `SetTaskPluginEnabled` / `DeleteTaskPluginVersion`。

**这是"内置默认 + 运行时可覆盖 + 版本化 + 增量同步"的完整方案。** 值得学的三点：

1. **内置 plugin 用 embed 进二进制** —— 开箱即用，无需联网下载；
2. **同步用 revision hash 而不是轮询全量** —— 100 个插件每 30 秒传一次源码会浪费带宽；
3. **版本+哈希双重校验** —— 保证"同一版本号对应同一份代码"。

### 与渠道的绑定

插件不是全局生效的，而是**绑定到渠道**（`setting.BindsTaskPlugin` / `TaskPluginKey` / `TaskExtendPluginKeys`，引用 `model.TaskPluginChannelRef`）：

```
请求 → middleware/task_plugin.go 解析出需要的插件
     → 在 gin.Context 里 pin 住「插件 + 端点」+ 注册表 generation
     → service/channel_select.go 用 dto.FilterTaskPluginIdentity 过滤
       → 只返回绑定了该插件的渠道
     → relay/channel/task/jsplugin/ 的适配器调用插件钩子
```

**"pin 住注册表 generation"这个细节很重要：** 一次请求在飞行中，插件可能被热更新。pin 住 generation 保证"这次请求用的还是它被路由时的那版插件"，避免请求走到一半插件换了导致行为不一致。

**自建建议：** 任何热更新系统都要考虑"在途请求怎么办"。最简单的答案就是**请求开始时快照版本，请求期间用快照**。

## 5. 三种扩展方式怎么选

| | Go 适配器 | JS 插件 | 纯配置（渠道字段） |
|---|---|---|---|
| 适合 | 复杂度高、需要 SDK、稳定 | 协议细节多、变更频繁 | 参数差异、header 差异 |
| 新增成本 | 改代码 + 发版 | 控制台安装，30 秒同步 | 控制台填表 |
| 性能 | 最高 | 有解释器开销 | 最高 |
| 能力边界 | 无限制 | 白名单网络出口、同步、5s 超时 | 无逻辑 |
| 本项目中的例子 | OpenAI / Claude / Gemini / Bedrock | 视频、音乐、图片任务 | 模型映射、header/param 覆盖 |

**决策顺序（自建时按这个顺序判断）：**

1. 能用**配置**表达吗？（映射表、header 覆盖、参数重写）→ 用配置
2. 是"同一协议的不同方言"吗？→ 加**选项**（如 `OpenRouterDialect`）
3. 供应商数量会持续增长且差异在协议层吗？→ **插件**
4. 需要 SDK、长连接、流式复杂逻辑吗？→ **Go 适配器**

**先穷尽配置和选项，再考虑插件。** 本项目的 `ParamOverride`/`HeaderOverride`/`ModelMapping` 三个渠道字段就能覆盖大量"新供应商"需求。

## 6. 自建路线

```
阶段 1：同步任务也能跑
  异步任务表 + 提交 + 手动查询接口（先不做自动轮询）

阶段 2：调度
  系统任务框架（DB 租约 + 心跳 + 手动触发 + 运行历史）
  把「任务轮询」注册成定时任务

阶段 3：结算正确性
  CAS 恰好一次结算 + 计价快照 + 超时清扫退款 + 轮询批量查询

阶段 4：扩展性（有真实需求再做）
  插件引擎（沙箱 + 白名单 + 超时） + 版本管理 + revision 同步
```

**第 4 阶段之前不要碰插件。** 插件系统的复杂度（沙箱、版本、同步、绑定、在途一致性）非常高，只有当"新增供应商的速度"真的成为瓶颈时才值得。

## 7. 坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| 分布式锁只有加锁没有"失锁停止" | 脑裂时双写 | 心跳 + 失锁取消 ctx |
| 任务入库前就结算 | 扣了钱查不到任务 | 先 durable 再 settle |
| 轮询逐任务发请求 | 上游限流/封禁 | 批量轮询适配器 |
| 无超时清扫 | 用户资金长期冻结 | 定时 CAS 失败 + 退款 |
| 完成时按当前价格算 | 价格变更引发账务争议 | 存计价快照 |
| 插件直接返回金额 | 无法统一定价与审计 | 插件只报用量，宿主持定价 |
| 热更新不 pin 版本 | 在途请求行为不一致 | 请求开始时快照 generation |
| 插件沙箱不限制网络 | 插件可外联任意地址 | allowedHosts 白名单 + 宿主 fetch |
| 插件版本号可重用 | 同一版本两份代码，不可复现 | 版本+哈希唯一，拒绝覆盖 |

## 8. 本篇索引

| 关注点 | 文件 |
|---|---|
| 系统任务模型 | `model/system_task.go`（`SystemTask`、`SystemTaskLock`） |
| 调度器 | `service/system_task.go`（`RegisterSystemTaskHandler`、`StartSystemTaskRunner`、`runWithLeaseHeartbeat`、`EnqueueSystemTask`） |
| 任务处理器 | `controller/system_task_handlers.go` |
| 任务模型与状态 | `model/task.go`、`model/midjourney.go` |
| 任务提交 | `relay/relay_task.go`（`TaskSubmitResult`、`ResolveOriginTask`） |
| 任务轮询 | `service/task_polling.go`（`TaskPollingAdaptor`、`RunTaskPollingOnce`、`sweepTimedOutTasks`） |
| 任务计费 | `service/task_billing.go`、`service/task_polling.go` |
| 任务制品 | `service/task_artifact_store.go`、`service/task_artifact_access.go` |
| 任务 API | `controller/task.go`、`router/task-router.go`、`router/video-router.go` |
| 插件引擎 | `pkg/jsplugin/{engine,registry,fetch}.go` |
| 插件存储与同步 | `model/task_plugin.go`、`controller/SyncTaskPlugins`、`plugins/embed.go` |
| 插件适配器 | `relay/channel/task/jsplugin/`、`middleware/task_plugin.go`、`router/plugin-router.go` |
| 插件 API 规范 | `docs/plugin-api/v1.md`、`v1.schema.json`、`v1.d.ts` |

→ 继续阅读：[10 · 前端架构](10-frontend-architecture.md)
