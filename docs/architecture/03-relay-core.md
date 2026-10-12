# 03 · 转发内核：适配器抽象、请求上下文、重试

> 这是整个项目里最核心的一层。**它决定了你"新增一个上游供应商"的成本是 30 分钟还是 3 天。** 本篇讲清楚三个东西：Adaptor 接口、RelayInfo 上下文、故障转移循环，并给出一份可以照着写的适配器骨架。

## 1. 问题的本质

N 个上游供应商 × M 种下游协议 × K 种模态。如果直接写 `if provider == "openai" && protocol == "claude" ...`，组合会爆炸。

本项目的解法是把问题切成两个正交维度，各自封装：

```
        ┌──────────── 输入侧 ────────────┐   ┌──── 输出侧（供应商适配器）────┐
客户端 ─→ 协议解析（RelayFormat） ─→ 模态处理（RelayMode） ─→ Adaptor.Convert* ─→ Adaptor.DoRequest ─→ 上游
        └────────────────────────────────┘   └──────────────────────────────┘
```

- **协议维度**（`RelayFormat`）：OpenAI Chat / OpenAI Responses / Claude Messages / Gemini 四种，两两互转，全部收敛进独立 module `relaykit`（见 04 篇）。
- **供应商维度**（`Adaptor`）：每个上游一个目录，只需实现"我要什么形状的请求""我的 URL 在哪""我的响应怎么解析成 usage"。

适配器**不需要知道下游是什么协议** —— 因为协议转换已经在它之前完成了。它拿到的是已经转好的、它自己协议的请求对象。这是整个设计最省事的一点。

## 2. Adaptor 接口

定义在 `relay/channel/adapter.go`：

```go
type Adaptor interface {
	// ---- 生命周期：每次尝试（attempt）开始时调用 ----
	Init(info *relaycommon.RelayInfo)

	// ---- 请求侧：下游协议 → 上游协议 ----
	ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error)
	ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error)
	ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error)
	ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.OpenAIResponsesRequest) (any, error)
	ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error)
	ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error)
	ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error)
	ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error)

	// ---- 传输侧 ----
	GetRequestURL(info *relaycommon.RelayInfo) (string, error)
	SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error
	DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error)

	// ---- 响应侧：解析 + 转发 + 提取 usage ----
	DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError)

	// ---- 元信息 ----
	GetModelList() []string
	GetChannelName() string
}
```

### 接口的五个阶段

| 阶段 | 方法 | 只做这件事 |
|---|---|---|
| 装配 | `Init` | 把渠道信息、供应商特有的 billing hook 注册进去 |
| 转换 | `Convert*` | 下游 DTO → 上游 DTO（**纯函数，最容易单测**） |
| 定位 | `GetRequestURL` + `SetupRequestHeader` | 算出 URL、拼出认证头 |
| 传输 | `DoRequest` | 发 HTTP（绝大多数直接复用共享实现） |
| 解析 | `DoResponse` | 读上游响应 → 转成下游协议 → **返回 usage 供计费** |

**`DoResponse` 返回 `usage any` 是关键设计。** 它把"这次调用花了多少"从适配器里带出来，交给统一的计费层处理。适配器只负责"如实报告用量"，不负责"算出多少钱"—— 计费逻辑与供应商解耦。

### 为什么 `Convert*` 是一组正交方法而不是一个

因为 `Adaptor` 实现者可以**只实现自己需要的那些**。`claude.Adaptor` 的 `ConvertClaudeRequest` 是真实现，而 `ConvertRerankRequest` 可以直接返回错误（该供应商不支持 rerank）。这比"一个 `Convert(format, req)` 方法 + 类型断言"更啰嗦，但换来两个好处：

1. **编译期就知道哪些组合没实现**（返回错误是显式的）；
2. 每个方法的入参类型是具体的 DTO，不用做 `any` 断言。

## 3. 分发机制：switch 还是 registry

`relay/relay_adaptor.go` 用的是一个朴素的 `switch`：

```go
func GetAdaptor(apiType int) channel.Adaptor {
	switch apiType {
	case constant.APITypeAli:        return &ali.Adaptor{}
	case constant.APITypeAnthropic:  return &claude.Adaptor{}
	case constant.APITypeGemini:     return &gemini.Adaptor{}
	case constant.APITypeOpenAI:     return &openai.Adaptor{}
	// ... ~38 个 case
	case constant.APITypeAdvancedCustom: return &advancedcustom.Adaptor{}
	case constant.APITypeNewAPI:     return &newapi.Adaptor{}
	}
	return nil
}
```

**这是一个刻意选择的"坏味道"。** 它的代价是：新增供应商要改这个文件（违反开闭原则）。收益是：

- 编译期检查完整性（少写一个 case 编译器不会提醒，但 `nil` 会被立刻发现）；
- 无反射、无 init 顺序问题、可读性极高；
- 依赖关系显式：`relay` 包 import 了所有 channel 子包，谁也跑不掉。

有意思的是，**同一个项目在"任务适配器"上用了完全相反的方案**——registry 模式：

```go
func GetTaskAdaptor(platform constant.TaskPlatform) service.TaskPollingAdaptor {
	plugin := pluginruntime.DefaultRegistry.Generation()...   // 从 JS 插件注册表解析
	return jspluginadaptor.New(plugin)
}
```

**为什么两种方案并存？** 因为变更频率不同：

| | 供应商适配器（switch） | 任务适配器（plugin registry） |
|---|---|---|
| 数量增长 | 慢（几十家，稳定） | 快（每家视频厂商一个，持续新增） |
| 实现形态 | Go 代码，需重新编译 | JS 脚本，运行时可热更新 |
| 分发方式 | 编译期 switch | 运行时注册表 + 版本化 |
| 变更影响 | 改代码 + 发版 | 从控制台装插件 |

**结论（自建时的决策依据）：** 如果扩展点的实现者是你自己（发版可控），用 switch，简单可靠；如果扩展点要交给外部/运营人员热更新，才上 registry/插件。**别为了"优雅"把稳定扩展点也做成插件系统。**

## 4. RelayInfo：贯穿全链路的请求上下文

`relay/common/relay_info.go` 里的 `RelayInfo` 是请求级的"万能上下文"。它很大，但字段可以分成六组，理解分组就理解了它：

```go
type RelayInfo struct {
	// ① 身份与配额：谁在花钱
	TokenId, UserId             int
	TokenKey, TokenGroup        string
	UsingGroup, UserGroup       string   // 分组：auto 跨组重试时会变动
	TokenUnlimited              bool
	UserQuota                   int
	SubscriptionId              int

	// ② 模型身份：三份模型名，别混用
	OriginModelName   string   // 客户端请求的名字
	UpstreamModelName string   // 映射后发给上游的名字（在 ChannelMeta 内）
	BillingModelName  string   // 定价用的名字
	ResponseModel     *ResponseModel

	// ③ 定价与结算
	PriceData             hosttypes.PriceData
	TieredBillingSnapshot *billingexpr.BillingSnapshot
	Billing               BillingSettler   // ★ 计费会话（预扣/结算/退款）
	BillingSource         string
	FinalPreConsumedQuota int
	ForcePreConsume       bool

	// ④ 重试状态
	RetryIndex int
	LastError  *types.NewAPIError

	// ⑤ 转换账本与流式状态
	RequestConversionChain  []types.RelayFormat
	FinalRequestRelayFormat types.RelayFormat
	StreamStatus            *StreamStatus
	SendResponseCount       int
	ReceivedResponseCount   int

	// ⑥ 渠道绑定（嵌入式，每次尝试重写）
	*ChannelMeta
	// 以及各种按模态挂载的子结构（ThinkingContentInfo / TokenCountMeta / RerankerInfo …）
}
```

### 三个模型名必须分清（这是最容易出错的地方）

| 字段 | 含义 | 谁设置 |
|---|---|---|
| `OriginModelName` | 客户端**请求**的名字，用于日志与计费身份 | 请求解析时 |
| `UpstreamModelName` | **真正发给上游**的名字（渠道映射后） | `ModelMappedHelper` |
| `BillingModelName` | **定价查表**用的名字 | `resolveBillingModelName` |

**为什么要三个？** 因为存在"虚拟定价别名"—— 你可能给某个模型加了一层渠道专属定价名，或者用同一个上游模型提供两个不同价位档。如果定价跟着 `UpstreamModelName` 走，这些场景就没法表达。所以定价身份单独抽出来（`relay/helper/price.go:resolveBillingModelName`）。

### ChannelMeta 与它的重置语义（★ 最容易被忽略的设计）

`RelayInfo` 内嵌 `*ChannelMeta`，承载"这一次尝试分配给哪个渠道"：

```go
type ChannelMeta struct {
	ChannelType, ChannelId      int
	ChannelIsMultiKey           bool
	ChannelMultiKeyIndex        int
	ChannelBaseUrl, ApiKey      string
	ApiType, ApiVersion         int
	Organization                string
	UpstreamModelName           string
	IsModelMapped               bool
	ParamOverride, HeadersOverride, ChannelSetting any
	SupportStreamOptions        bool
}
```

重试时会调用 `InitChannelMeta(c)` 重新物化它。**关键在于它重置哪些、保留哪些：**

| 重置（per-attempt） | 保留（per-request） |
|---|---|
| `ResponseModel`、模型映射结果 | `StreamStatus` |
| 转换链、流式转换器缓存 | 计费累加器、`PriceData` |
| `VendorToolUsage`、`convOptions` | 计费诊断（`Diagnostics`） |
| 上游模型名、API key、base URL | 用户/令牌身份、重试计数 |

**这条边界就是"跨渠道重试安全"的全部秘密。** 如果重试时把计费累加器也清了，用户就被少扣一次；如果把上一次的流状态留着，就会给客户端发出重复或错乱的事件。

**自建建议：** 把你的请求上下文对象显式拆成 `RequestState`（跨尝试共享）和 `AttemptState`（每次重置）两个结构，让"重试要重置什么"变成类型系统的约束，而不是注释里的约定。

## 5. 共享传输层

`relay/channel/api_request.go` 提供所有适配器共用的出站能力：

```go
DoApiRequest(a Adaptor, c *gin.Context, info *RelayInfo, body io.Reader) (resp *http.Response, err error)
DoFormRequest(...)          // multipart，用于音频/图片上传
DoWssRequest(...)           // WebSocket 场景
DoTaskApiRequest(...)       // 异步任务提交
SetupApiRequestHeader(...)  // URL + header + 覆盖项的统一入口
ResolveHeaderOverride(...)  // 渠道级 header 覆盖
```

适配器的 `DoRequest` 通常只有一行：`return channel.DoApiRequest(a, c, info, requestBody)`。

**挑出来的三个设计点：**

1. **出站 HTTP 客户端统一在 `service.InitHttpClient()` 建好**（连接池、超时、`RELAY_IDLE_CONN_TIMEOUT`），而不是每个适配器 `http.Get`。网关是高并发长连接场景，连接复用是性能命门。
2. **header/param 覆盖是渠道级配置**（`HeaderOverride`/`ParamOverride`），由共享层统一应用，而不是散落在各适配器里。这样运营人员可以在控制台上给某条渠道强制加一个 header，无需改代码。
3. **错误在共享层归一化成 `types.NewAPIError`**，适配器里不做错误分类。

## 6. 流式转发：三 goroutine 模型

`relay/helper/stream_scanner.go` 的 `StreamScannerHandler` 是 SSE 转发的骨架：

```go
func StreamScannerHandler(c *gin.Context, resp *http.Response,
	dataHandler func(data string, sr *StreamResult)) {

	info.StreamStatus = relaycommon.NewStreamStatus()
	SetEventStreamHeaders(c)                       // Content-Type: text/event-stream 等
	scanner := NewStreamScanner(resp.Body)

	// 三个 goroutine 协作，用 stopChan / ctx.Done() 终止
	//  ① ping ticker：定期下发空注释保活，防止中间设备掐断连接
	//  ② data 处理：从 channel 取一条 SSE data，交给 dataHandler
	//  ③ scanner：逐行读上游流，滤出 "data: " 与 "[DONE]"
	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < 6 { continue }
		if data[:5] != "data:" && data[:6] != "[DONE]" { continue }
		data = strings.TrimSpace(data[5:])
		if !strings.HasPrefix(data, "[DONE]") {
			info.SetFirstResponseTime()
			info.ReceivedResponseCount++
			select {
			case dataChan <- data:
			case <-ctx.Done():   return
			case <-stopChan:     return
			}
		} else {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			return
		}
	}
}
```

**为什么必须是三个 goroutine？** 因为流式转发同时受三个独立时钟约束：

- **上游出数据的节奏**（可能 30 秒才来一个 token）；
- **下游客户端的节奏**（写了写不进去 → 说明客户端断了，要立刻停止读上游，**否则上游的 token 白计费**）；
- **保活时钟**（长时间无数据，中间的 Nginx/负载均衡会掐连接）。

单线程做不到"读上游的同时还能感知客户端断开"，所以拆成"读 / 写 / 保活"三个角色，用 `select` + `ctx.Done()` 统一收口。

配套的机制还有：

- `StreamingTimeout`（默认 120s）：超过这么久没数据就放弃，避免上游挂死后连接永久占用；
- 写超时 30s（`ExtendWriteDeadline`）：客户端读取太慢时主动断开；
- `StreamStatus` 记录结束原因（正常 `[DONE]` / 超时 / 客户端断开 / 上游错误）—— 这个状态**会进入计费与日志**，因为"客户端断开"和"完成后断开"的收费策略可能不同。

## 7. 故障转移循环

重试决策被抽成了一个独立函数：`service/DecideRelayRetry`（`service/relay_error.go`），返回 `PolicyDecision{Action, Reason, Source}`。

**会重试：** 渠道类错误（`IsChannelError`）、配置的可重试状态码（`AutomaticRetryStatusCodes`）。
**不会重试：** 标记了 `SkipRetry` 的错误、渠道被 pin 死、严格会话亲和、重试预算耗尽、不可重试的状态码。

重试时做两件事：

1. `getChannel` 重新选渠道（下一优先级 / 下一个分组）；
2. `processChannelError` → `service.ShouldDisableChannel` → 必要时自动禁用该渠道。

**两个关键约束：**

- **只有"尚未向客户端写出任何字节"的失败才能重试。** 一旦开始 SSE 推流，就无法切换到另一个渠道 —— 客户端已经收到半截内容了。这就是 `SendResponseCount` 存在的原因：用它判断"现在还能不能重试"。
- **重试不能重复计费。** 因为预扣是请求级的（在循环外），而每次尝试只更新 `LastError`，最终只结算一次。

## 8. 新增一个供应商：可照抄的骨架

以"一家 OpenAI 兼容的供应商"为例，最小实现只需两个文件：

```go
// relay/channel/myprovider/adaptor.go
package myprovider

type Adaptor struct{}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {}   // 无状态时留空

// 直接透传（已经是 OpenAI 形状）
func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo,
	request *dto.GeneralOpenAIRequest) (any, error) {
	return request, nil                                    // 需要改字段时用 gjson/sjson 改
}

// 其余协议：复用已有适配器或用 relaykit 转换
func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo,
	request *dto.ClaudeRequest) (any, error) {
	return service.ConvertRequest(c, info, types.RelayFormatOpenAI, request)
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return info.ChannelBaseUrl + "/v1/chat/completions", nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header,
	info *relaycommon.RelayInfo) error {
	req.Set("Authorization", "Bearer "+info.ApiKey)
	req.Set("Content-Type", "application/json")
	return nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo,
	body io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, body)     // 复用共享传输
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response,
	info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	return openai.Adaptor{}.DoResponse(c, resp, info) // 复用已有响应解析
}

// 不支持的模态：显式返回错误
func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int,
	request dto.RerankRequest) (any, error) {
	return nil, types.NewError(errors.New("rerank not supported"), types.ErrorCodeInvalidRequest)
}

func (a *Adaptor) GetModelList() []string { return ModelList }
func (a *Adaptor) GetChannelName() string { return "MyProvider" }
```

然后在两个地方注册：

```go
// 1. relay/relay_adaptor.go —— 加入 switch
case constant.APITypeMyProvider: return &myprovider.Adaptor{}

// 2. constant/api_type.go —— 加枚举值（数值由渠道 Type 字段持久化，不要改已有值）
```

**注意：** 已有 `deepseek`、`mistral`、`xai`、`openrouter`、`xinference`、`siliconflow` 等一批供应商就是按这个模板做的（只有 `adaptor.go` + `constants.go`，`DoResponse` 直接转发给 `openai.Adaptor`）。**先看它们，再写自己的。**

## 9. 自建时的坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| 适配器里直接算钱 | 定价改动要动 N 个适配器 | 只返回 `usage`，计费交给统一层 |
| 适配器里直接读 DB / 全局配置 | 无法单测，并发问题 | 需要什么从 `RelayInfo`/参数传入 |
| 重试时重建整个上下文 | 重复计费 / 事件错乱 | 显式区分 per-request 与 per-attempt 状态 |
| 无 `SendResponseCount` 判断就重试 | 向客户端发重复的流内容 | 写过字节就不再重试 |
| 只有一个"模型名"字段 | 别名/虚拟定价没法表达 | 从一开始就分 origin/upstream/billing |
| 流式转发没有保活 | 中间代理静默断连 | ping ticker + 写超时 |
| 无脑把扩展点做成插件 | 复杂度暴涨、调试困难 | 稳定扩展点用 switch，动态扩展点才上 registry |

## 10. 本篇索引

| 关注点 | 文件 |
|---|---|
| 适配器接口 | `relay/channel/adapter.go` |
| 供应商分发 | `relay/relay_adaptor.go` |
| 请求上下文 | `relay/common/relay_info.go`（`RelayInfo`、`ChannelMeta`、`InitChannelMeta`） |
| 共享传输 | `relay/channel/api_request.go` |
| SSE 转发 | `relay/helper/stream_scanner.go`、`relay/helper/common.go` |
| 模态 handler | `relay/compatible_handler.go`（`TextHelper`）、`claude_handler.go`、`gemini_handler.go` |
| 重试决策 | `service/relay_error.go`（`DecideRelayRetry`、`ProcessChannelError`） |
| 简单供应商样例 | `relay/channel/deepseek/`、`relay/channel/mistral/` |
| 复杂供应商样例 | `relay/channel/claude/`、`relay/channel/aws/`（SDK 模式）、`relay/channel/gemini/` |

→ 继续阅读：[04 · 协议转换](04-protocol-conversion.md)
