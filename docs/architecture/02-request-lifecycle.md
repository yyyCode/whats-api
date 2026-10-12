# 02 · 请求生命周期：从一个字节到一次上游调用

> 网关的所有正确性问题（超时、重试、计费、限流）最终都发生在一条请求链路上。理解这条链路的**顺序**，比理解任何单个模块都重要。本篇把 `POST /v1/chat/completions` 拆开走一遍。

## 1. 路由的三层注册

入口是 `router.SetRouter`（`router/main.go`），它把不同"面"的路由分别装进同一个 `gin.Engine`：

| 关注面 | 文件 | 前缀 | 说明 |
|---|---|---|---|
| 转发面 | `router/relay-router.go` | `/v1`、`/v1beta`、`/mj`、`/pg` | 对下游客户端的 OpenAI/Claude/Gemini 兼容 API |
| 管理面 | `router/api-router.go`（+ `channel-router.go`、`authz-router.go`） | `/api` | 控制台所有 CRUD |
| 任务面 | `task-router.go`、`video-router.go`、`task-plugin-protocol-router.go` | `/v1/tasks`、`/v1/videos` | 异步任务提交与查询 |
| 插件动态路由 | `plugin-router.go` | 由插件声明 | 返回一个 dispatcher，兜住插件注册的路径 |
| 前端 SPA | `web-router.go` | `NoRoute` 兜底 | 返回内嵌的 `index.html`，并对 `/v1`、`/api`、`/assets` 主动 404 |
| 遗留计费面 | `dashboard.go` | `/dashboard`、`/v1/dashboard` | 老版本兼容 |

**三个可抄的决策：**

1. **`/api` 与 `/v1` 彻底分离**。管理面的错误格式是"给人看的"（带 `message_key` 供前端 i18n），转发面的错误格式是"给 SDK 看的"（OpenAI/Claude 的错误 JSON）。同一个中间件很难同时满足两者，所以从一开始就分成两组路由、两套错误语义。
2. **SPA 用 `NoRoute` 兜底**，但显式排除 API 前缀。这样前端路由（`/dashboard/...`）可以直接刷新而不 404，同时 `/v1/foo` 这种写错的 API 路径不会诡异地返回 HTML。
3. **支持 `FRONTEND_BASE_URL`**：非 master 节点可以把所有页面请求 302 到独立的前端域名，实现"前端静态资源与网关计算分离部署"。

## 2. 中间件链：完整顺序

这是本项目的核心工程资产之一。顺序不是随意的，每一层的相对位置都有理由。

### 全局层（`main.go` 中 `server.Use`）

| 顺序 | 中间件 | 作用 |
|---|---|---|
| 1 | `gin.CustomRecovery` | panic → 统一 500 JSON |
| 2 | `RequestId()` | 注入 request id，贯穿日志与审计 |
| 3 | `Version()` | 响应头带版本 |
| 4 | `I18n()` | 解析请求语言，为错误消息选语言 |
| 5 | `SetUpLogger(server)` | 访问日志，按 `route_tag` 区分面 |

### 引擎层（`SetRelayRouter` 内）

| 顺序 | 中间件 | 作用 |
|---|---|---|
| 6 | `CORS()` | 跨域 |
| 7 | `DecompressRequestMiddleware()` | 解压 gzip 请求体（转发场景必须支持） |
| 8 | `BodyStorageCleanup()` | 请求结束后释放请求体缓存，防内存泄漏 |
| 9 | `StatsMiddleware()` | 活跃连接数统计（并发水位观测） |

### `/v1` 组层

| 顺序 | 中间件 | 作用 |
|---|---|---|
| 10 | `RouteTag("relay")` | 打标签，日志/指标按面聚合 |
| 11 | `SystemPerformanceCheck()` | CPU/内存/磁盘超阈值时直接 503，**保护自身而非上游** |
| 12 | `TokenAuth()` | ★ API Key 认证 + IP 白名单 + 用户启用态 + 模型权限 |
| 13 | `ModelRequestRateLimit()` | 按分组 + 模型的请求数/成功数限流 |

### 子组层（JSON 转发子组）

| 顺序 | 中间件 | 作用 |
|---|---|---|
| 14 | `Distribute()` | ★ 读请求体取 model/group → 选渠道 → 写入 Gin context |

最后进入 handler：`controller.Relay(c, types.RelayFormatOpenAI)`。

**为什么 `Distribute` 在最内层？** 因为它需要读**请求体**（拿到 `model` 和 `group`），而读请求体是昂贵且不可重复的操作。把它放在认证和限流之后，意味着被拒绝的请求不会付出解析 body 的代价。这是"廉价校验在前、昂贵操作在后"的经典排序。

**为什么 `SystemPerformanceCheck` 在认证之前？** 过载保护的目的是**尽快把请求推走**，越早越省资源。

## 3. Distribute：选渠道做了什么

`middleware/distributor.go` 的 `Distribute()` 是转发面最关键的一环，职责链：

```
1. 组装渠道约束（service.GetChannelConstraints：按分组/模型/渠道过滤）
2. 解析请求体里的 ModelRequest{Model, Group}（只解析需要的字段）
3. 处理"用户指定渠道"（pinned channel）
4. 解析模型映射（model_mapping，见 05 篇）
5. service.SelectChannelForRequest(...) → 选中一个 model.Channel
6. SetupContextForSelectedChannel(...)：把渠道的 type/id/key/baseurl/映射/覆盖项
   全部写进 gin.Context（constant.ContextKeyChannel*）
7. 失败时记录 request policy 终止原因
```

**设计要点：中间件只负责"选"，不负责"用"。** 选中的渠道被放进 `gin.Context` 的若干 key 里，后续由 `relay/common.RelayInfo.InitChannelMeta(c)` 读取并物化成 `ChannelMeta` 结构。这样做的收益是：

- 选渠道逻辑与转发逻辑解耦，二者可以独立演进；
- 重试时只需重新调用 `getChannel` 并重新 `InitChannelMeta`，不需要重建请求对象。

**代价**：context key 是弱类型的字符串常量，跨层传递时容易"谁写的、谁读的"说不清。如果你自建，建议用一个显式的 `RequestContext` 结构体在中间件间传递，而不是散落的 context key。

## 4. controller.Relay：转发主循环

`controller/relay.go` 的 `Relay()` 是所有非任务转发的唯一入口，骨架如下（简化自真实实现）：

```go
func Relay(c *gin.Context, relayFormat types.RelayFormat) {
	// ① 读取并校验请求体（含请求体大小限制、格式校验）
	request, err := helper.GetAndValidateRequest(c, relayFormat)

	// ② 构造请求级上下文：这一层把 Gin context 里散落的状态
	//    汇聚成一个 RelayInfo（见 03 篇）
	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)

	// ③ ★ 计费预扣：在发起上游请求之前就把钱冻住
	PrepareRequestBilling(c, relayInfo)
	defer RefundFailedRequestBilling(c, relayInfo, ...)   // ④ 失败退款

	// ⑤ 重试循环
	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		relayInfo.StreamStatus = nil          // 每次尝试重置流状态
		relayInfo.RetryIndex = retryParam.GetRetry()

		channel, err := getChannel(c, relayInfo, retryParam)   // 重新选渠道
		if err != nil { break }

		// 按协议分派到对应的"模态 handler"
		switch relayFormat {
		case types.RelayFormatOpenAIRealtime: err = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:         err = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:         err = geminiRelayHandler(c, relayInfo)
		default:                              err = relayHandler(c, relayInfo)
		}
		if err == nil { return }

		// ⑥ 决定是否重试 + 上报渠道错误
		relayInfo.LastError = err
		decision := service.DecideRelayRetry(c, err, remaining)
		processChannelError(c, ..., err, relayInfo)   // 可能自动禁用渠道
		if decision.Action != "retry" { break }
	}
}
```

### 双维度分派

注意这里有**两层 switch**，是两个正交的维度：

**维度一：协议格式 `types.RelayFormat`**（客户端说哪种协议）

```
openai, claude, gemini, openai_responses, openai_realtime,
openai_alpha_search, openai_audio, openai_image, rerank, embedding, task, mj_proxy
```

**维度二：请求模态 `RelayMode`**（要做什么事），在 `relayHandler` 里再分派：

```go
func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	switch info.RelayMode {
	case RelayModeImagesGenerations, RelayModeImagesEdits: err = relay.ImageHelper(c, info)
	case RelayModeAudioSpeech, RelayModeAudioTranslation,
	     RelayModeAudioTranscription:                      err = relay.AudioHelper(c, info)
	case RelayModeRerank:                                  err = relay.RerankHelper(c, info)
	case RelayModeEmbeddings:                              err = relay.EmbeddingHelper(c, info)
	case RelayModeResponses, RelayModeResponsesCompact:    err = relay.ResponsesHelper(c, info)
	case RelayModeAlphaSearch:                             err = relay.AlphaSearchHelper(c, info)
	default:                                               err = relay.TextHelper(c, info)
	}
	return err
}
```

**为什么要两个维度？** 因为它们在概念上确实不同：

- *协议格式* 决定**输入端**怎么解析、**输出端**怎么编码（OpenAI Chat 还是 Claude Messages 的 JSON 形状）；
- *模态* 决定**中间做什么事**（聊天、画图、语音、重排、嵌入）。

一个 `/v1/messages`（Claude 协议）请求，可能是"聊天"（`TextHelper`），也可能是"rerank"；一个图片生成请求可能来自 OpenAI 协议也可能来自其他协议。二维正交，写成一个 switch 会爆炸，所以拆成两层。

**自建提示：** 这两个枚举是你系统里最重要的"路由主键"。**先把它们定清楚，再写任何 handler**。本项目把它们分别放在 `relaykit/types/relay_format.go`（跨模块共享）和 `relay/constant/relay_mode.go`（宿主内），这个放置位置本身也值得借鉴：跨模块的放 relaykit，宿主专有的留在宿主。

## 5. 模态 handler 的统一形状

以最常走的 `TextHelper`（`relay/compatible_handler.go`）为例，每个模态 handler 都是同一套四步：

```go
func TextHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	// ① 模型名映射：客户端模型名 → 上游模型名（含别名链、推理后缀）
	helper.ModelMappedHelper(c, info, request)
	helper.ApplyReasoningModelSuffix(info, request)

	// ② 取适配器并初始化（每次尝试都重新 Init）
	adaptor := GetAdaptor(info.ApiType)      // switch on APIType
	adaptor.Init(info)

	// ③ 请求转换：下游协议 → 上游协议
	convertedRequest, err := adaptor.ConvertOpenAIRequest(c, info, request)

	// ④ 发请求 + 处理响应
	body, _ := common.Marshal(convertedRequest)
	resp, err := adaptor.DoRequest(c, info, bytes.NewReader(body))
	usage, apiErr := adaptor.DoResponse(c, resp, info)   // 内部完成 SSE 转发 + usage 提取
	...
}
```

四步对应四个关注点：**模型身份 → 适配器装配 → 协议转换 → 传输与结算**。任何新供应商都只需要在 `relay/channel/<provider>/` 下实现这四步，不需要碰链路上的其他部分。这套"统一形状"是后面 03、05 两篇的基础。

## 6. WebSocket 路由的特殊处理

`GET /v1/responses`（Responses WebSocket）和 `GET /v1/realtime` 是长连接，注册方式与 HTTP 路由**故意不同**：

```go
// 注册顺序很重要：WS 路由必须在 ModelRequestRateLimit 之前
relayV1Router.GET("/responses", controller.ResponsesWebSocket)
relayV1Router.Use(middleware.ModelRequestRateLimit())
wsRouter := relayV1Router.Group("")
wsRouter.Use(middleware.Distribute())
wsRouter.GET("/realtime", func(c *gin.Context) { controller.Relay(c, types.RelayFormatOpenAIRealtime) })
```

差别的原因：

1. **限流语义不同**：HTTP 请求是"一次请求一次限流"，WebSocket 是"一次连接、多次消息"。对连接建立做的事前限流没有意义，所以 WS 走的限流判定被推迟到连接内的第一个 `response.create` 事件。
2. **渠道选择时机不同**：WS 连接建立时还不知道要用哪个模型，所以 `Distribute` 无法在握手阶段工作；实际渠道选择发生在收到第一个业务事件之后。
3. **升级器需要放行**：`controller/relay.go` 里的 `upgrader` 显式允许 `realtime`、`responses` 子协议，并将 `CheckOrigin` 置为 `true`（鉴权由 token 完成，不依赖 Origin）。

**自建提示：** 一旦你的网关要支持 WebSocket（Realtime/Responses），"中间件链"这个心智模型就会失效——长连接不能靠请求前置中间件处理。正确做法是把 WS 生命周期显式建模成状态机：**握手（认证）→ 首包（选渠道/绑定会话）→ 循环（转发 + 增量计费）→ 关闭（结算/退款）**。本项目就是这么做的（`relay/websocket.go`、`controller/responses_websocket.go`）。

## 7. 错误归一化

链路上所有错误最终都被压成 `*types.NewAPIError`：

- 适配器内部错误 → `types.NewAPIError`；
- 上游 HTTP 错误 → 按状态码分类（是否可重试、是否要禁用渠道）；
- 客户端错误 → 4xx，且**不退预扣**（有些场景要收"违规费"）。

这个统一错误类型同时携带：错误码、HTTP 状态码、是否可重试、是否隐藏上游细节、以及给用户看的消息。**别用 `error` 字符串做控制流**——本项目为此定义了一整套错误码枚举，重试决策、渠道禁用、审计记录全都基于它。

## 8. 自建时的顺序清单

按重要性排序，你的第一版应该这样组织中间件：

```
1. Recovery / RequestID        必须最先，否则 panic 无人接
2. 日志 + 路由标签              先打标签，后面所有日志都能分类
3. 过载保护                    越早越好，成本最低的拒绝
4. 协议反序列化 + 格式校验       需要 body，但不需要业务上下文
5. 认证（API Key → 用户/分组）   需要 body 里的 model 做权限校验，故在解析后
6. 限流                        依赖认证结果（按用户/分组限流）
7. 渠道选择                    最贵的一步（读缓存、算亲和性、查可用性）
8. 计费预扣                    必须先于上游调用，且与 7 的顺序可互换
9. handler                    上游调用 + 响应转换 + 结算
```

**不要做的事：**

- ❌ 把计费预扣放在响应阶段（上游已经花钱了，扣款失败就白送）；
- ❌ 把渠道选择放在认证之前（未授权请求也会占用选路资源，并可能泄露渠道信息）；
- ❌ 用一个中间件同时处理 HTTP 和 WebSocket（分叉会污染两边）。

## 9. 本篇索引

| 关注点 | 文件 |
|---|---|
| 路由总装 | `router/main.go`、`router/relay-router.go`、`router/api-router.go`、`router/web-router.go` |
| 全局中间件 | `main.go`（`server.Use(...)`） |
| 认证 | `middleware/auth.go`（`TokenAuth`） |
| 渠道分发 | `middleware/distributor.go`（`Distribute`、`SetupContextForSelectedChannel`） |
| 限流 | `middleware/model-rate-limit.go`、`middleware/rate-limit.go` |
| 过载保护 | `middleware/performance.go` |
| 转发入口 | `controller/relay.go`（`Relay`、`relayHandler`、`getChannel`） |
| WebSocket | `controller/responses_websocket.go`、`relay/websocket.go` |
| 模态 handler | `relay/compatible_handler.go`（`TextHelper`）、`claude_handler.go`、`gemini_handler.go` |
| 协议枚举 | `relaykit/types/relay_format.go`、`relay/constant/relay_mode.go` |

→ 继续阅读：[03 · 转发内核](03-relay-core.md)
