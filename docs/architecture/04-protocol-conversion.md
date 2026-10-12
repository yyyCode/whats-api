# 04 · 协议转换：RelayKit 的设计哲学

> 项目里最值钱、最容易单独复用的部分。它解决的是"四种 LLM 协议两两互转"，但真正值得学的是它的**有损转换处理哲学**：转换永远成功，丢掉了什么如实报告。

## 1. 为什么它被拆成一个独立 module

`relaykit/` 是独立 Go module（`go 1.25.1`），运行时依赖只有 `uuid`、`lo`、`gjson`、`sjson` 四个包。它做协议数据的**建模与转换**；HTTP 收发、SSE 读写、渠道选择、鉴权、计费全部由宿主完成。

这个边界的价值：

| 收益 | 具体表现 |
|---|---|
| 可单测 | 转换是纯函数式变换，输入 DTO → 输出 DTO，不需要 DB/网络 |
| 可复用 | 别的 Go 网关可以直接 `go get` 嵌进去用 |
| 可替换 | 宿主换实现（换 JSON 库、换日志系统）不影响转换逻辑 |
| 可独立验证 | `GOWORK=off go build ./...` 单独构建，CI 可强制 |
| 有独立版本节奏 | tag 形如 `relaykit/vX.Y.Z`，v0.x 期间 API 可变 |

**判据（可复用到你自己的项目）：** 一段代码如果**不读 DB、不发网络、不读全局配置、不依赖时间/随机数**，它就可以独立成 module。协议转换满足；计费不满足（读定价表、写账本），所以计费老老实实留在宿主。

## 2. 转换矩阵：直接路径 vs 多跳路径

四种协议两两互转，但不是每一对都值得手写。项目用**质量等级**显式标注了转换代价：

| 源 \ 目标 | OpenAI Chat | OpenAI Responses | Claude Messages | Gemini |
|---|---:|---:|---:|---:|
| OpenAI Chat | — | `good` | `fair` | `fair` |
| OpenAI Responses | `good` | — | `fair` | `fair` |
| Claude Messages | `fair` | `fair` | — | `discouraged` |
| Gemini | `fair` | `fair` | `discouraged` | — |

- **`good`**：两种协议结构接近，字段基本一一对应。
- **`fair`**：主要内容能转，协议独有字段以诊断形式报告。
- **`discouraged`**：Claude ↔ Gemini 请求和响应**都要经 OpenAI Chat 中转两跳**，丢字段概率高。

**这是本设计里最值得抄的一个 idea：不假装转换是无损的。**

### 每一次转换都返回一份"账"

```go
type RequestResult struct {
	Value       any                      // 转换后的 DTO
	From, To    types.RelayFormat        // 源 / 目标格式
	Converter   string                   // 使用的转换器 ID
	Quality     string                   // good / fair / discouraged
	Steps       []types.RelayFormat      // ★ 实际经过的每一步
	Diagnostics []types.ConversionDiagnostic // ★ 丢了什么
}
```

`Steps` 让调用方能知道"这个转换实际上是几跳"，`Diagnostics` 让调用方能知道"具体丢了什么"。诊断条目带代码、字段路径、说明、级别和源/目标格式，例如：

- `hosted_web_search_omitted`
- `trailing_assistant_omitted`
- `unsupported_media_type`
- `claude_max_tokens_raised`（Claude 的 max_tokens 超出模型范围被抬高）
- `claude_budget_adjusted`（thinking budget 被调整）

**为什么这比"尽力而为然后静默"好得多？** 因为网关运营者需要知道"用户抱怨工具调用没生效"到底是上游不支持还是我们的转换丢了。有了诊断列表，这个问题变成一条可检索的日志。

## 3. 选路：调用方不用关心路径

```go
// 自动选路：按传入 DTO 的具体类型推断源协议，查表选直接转换器或多跳路径
func ConvertRequest(ctx context.Context, meta convmeta.Meta,
	target types.RelayFormat, request any) (*RequestResult, error)

// 固定路径（你知道要走哪几跳）
func ConvertRequestVia(ctx, meta, target, request, path ...RelayFormat) (*RequestResult, error)

// 按转换器 ID 执行
func ConvertRequestByID(ctx, meta, target, request, converterID string) (*RequestResult, error)
```

三级 API 的用意很清楚：

- `ConvertRequest`：**99% 的场景**，调用方只想说"把这个转成 Claude 格式"；
- `ConvertRequestVia`：调试/测试时想锁定路径，保证可复现；
- `ConvertRequestByID`：需要精确控制实现（比如绕开某个有 bug 的转换器）。

多跳路径**在注册时校验连续性**（`StepConverters`），所以不会出现"A→B→D"这种中间断链的配置。

## 4. 转换上下文：怎么让转换器拿到它需要的东西

转换有时需要外部信息：源模型名、上游模型名、推理设置、工具编码记录、媒体下载函数。这些通过 `convmeta.Meta` 接口注入：

```go
type Meta interface {
	GetOriginModelName() string
	GetUpstreamModelName() string
	HasChannelMeta() bool
	ReasoningState() *dto.ReasoningConversionState
	EnsureClaudeConvertInfo() *dto.ClaudeConvertInfo
	ResponsesToolState() *convmeta.ResponsesToolState
	SetResponsesToolState(*convmeta.ResponsesToolState)
	ConvOptions() *convmeta.Options
	AppendRequestConversion(...)
	// ...
}
```

**关键点：宿主的 `*relaycommon.RelayInfo` 直接实现这个接口**（`relay_info.go:841-990`）。所以 relaykit 不需要知道宿主的请求对象长什么样，宿主也不需要为转换单独造一个对象。

**接口的一个细节要求值得学**：接口注释明确要求"指针类型的每个方法都要能在 nil receiver 上返回零值"。这样大部分基础转换可以直接传 `nil` 作为 meta，只有需要上下文时才传具体实现。**用 nil-safe 接口换取调用侧的简洁**，是个很实用的技巧。

### 宿主钩子（依赖注入的三种形态）

```go
relayconvert.SetMediaResolver(relayconvert.MediaResolver{
	GetBase64Data:        getBase64Data,          // 跨协议图片转换要下载 URL 时调用
	DecodeBase64FileData: decodeBase64FileData,
})
kitutil.SetCodec(myCodec)                         // 换 JSON 编解码实现
kitutil.SetLogging(common.SysLog, errorLogger)    // 转换器诊断日志交给宿主
```

| 钩子形态 | 用途 | 何时用 |
|---|---|---|
| 函数注入（`SetMediaResolver`） | 转换器需要宿主提供**能力** | 无状态、一次性注册 |
| 接口注入（`Meta`） | 转换器需要**本次请求的上下文** | 每请求一份 |
| 选项结构（`Options`） | 转换器需要**行为开关** | 按请求/按渠道配置 |

这三种形态覆盖了几乎所有的"库需要宿主配合"的场景，可以直接搬到你自己的模块设计里。

### Options：把供应商差异做成配置而非分支

```go
meta := &convmeta.Values{
	Options: &convmeta.Options{
		Claude: convmeta.ClaudeOptions{
			DefaultMaxTokens: func(model string) int { return 4096 },
		},
		Gemini: convmeta.GeminiOptions{ThinkingAdapterEnabled: true},
		ToolLossPolicy:  convmeta.ToolLossPolicySafe,
		OpenRouterDialect: true,   // 输出 OpenRouter 额外接受的字段
		WebSearch:       myWebSearchEncoding,
	},
}
```

值得注意的几个：

- `Claude.DefaultMaxTokens`：**用函数而不是常量**，因为默认值依模型而定。把"策略"作为回调注入，比在转换器里写 `if model == "..."` 干净得多。
- `OpenRouterDialect`：同一个协议的不同方言（OpenRouter 兼容 OpenAI 但多接受一些字段）。**把方言差异做成选项，而不是为新方言写一个新协议枚举。**
- `ToolLossPolicy`：见下节。

## 5. 有损转换的处理策略（★ 最有价值的设计）

跨协议转换必然丢东西：Responses 的 custom tool、托管 web search、某些多模态类型在目标协议里根本没有对应物。项目的处理是：

**第一步：永远完成转换，把损耗写进 `Diagnostics`。**
**第二步：让调用方用 `ToolLossPolicy` 决定要不要因此失败。**

```go
ToolLossPolicy:
  allow   // 默认：转换完成，所有损耗以诊断返回
  safe    // 请求阶段遇到 error 级诊断 → 返回 *types.ConversionLossError
  strict  // 请求阶段遇到任何诊断 → 返回错误
```

**为什么是"请求阶段"才可能失败，响应阶段永远成功？**

因为请求阶段的失败是**可恢复的**（返回错误给客户端，让它换个协议或换个模型），而响应阶段的失败是**不可恢复的**（上游已经执行、已经计费了，此时拒绝响应等于既花了钱又没结果）。这个不对称是有意的。

**这是本设计最值得学的一点：** 对不可逆的操作（已经花钱的上游调用）永远选择"完成 + 报告"，对可逆的操作（请求前的校验）才允许"拒绝"。

## 6. 跨步骤状态传递：三个 state 对象

多跳转换（A → Chat → B）里，有些信息必须在每一步之间传递，否则第二步就还原不出来。项目用了三个显式状态对象：

| 状态对象 | 承载什么 | 为什么需要 |
|---|---|---|
| `convmeta.ResponsesToolState` | Responses 的 custom tool / namespace 工具被改写成普通 function tool 的**编码记录** | 响应转换时要按这份记录**还原**成 Responses 的形状 |
| `dto.ReasoningConversionState` | reasoning effort、thinking budget、是否返回思考内容 | 多跳路径上每一步都要能读到**精确**的 budget 和 effort，而不是靠猜 |
| `ResponseStreamState` | 跨事件的工具调用拼接、usage 累加、结束状态 | 流式响应里一个工具调用分散在多个事件中 |

**这三者揭示了一个通用规律：转换不是无状态的函数，而是"带账本的过程"。** 你转换时做的任何改写（把工具换编码、把预算换算、把参数改名），都必须在账本里留一笔，否则响应转换时无法逆向。

**自建建议：** 如果你的协议转换只有一跳（A → B），先不需要账本；但**当你决定支持多跳或支持 Responses 这类有强状态的协议时，账本必须和转换器同期设计**，不能事后补。

## 7. Usage 的归一化：既要统一，又要保留原始

```go
type Usage struct {
	// 统一字段
	InputTokens, OutputTokens, CacheHitTokens ...
	CacheCreation5m, CacheCreation1h ...   // Claude 缓存写入按 5 分钟 / 1 小时拆分
	// ★ 原始痕迹
	BillingUsage BillingUsage   // 上游原始 usage 结构 + 它来自哪个协议
}
```

`BillingUsage` 是设计亮点：**统一字段用于通用计价，原始结构用于按上游自己的计量规则计费**。这是因为不同供应商对"什么算钱"的定义不同（缓存命中怎么算、思考 token 怎么算），归一化必然丢失信息。

合并两份 usage 时用了明确的规则（`dto.MergeUsageNonZero`）：

> **后到的非零字段覆盖已有值；后到的零值保留已有的计数。**

为什么？因为流式响应的中间事件可能带 `usage` 全零（占位），如果按"后到覆盖"就会把真实计数抹成 0。**在流式场景里，"零值"往往意味着"这条消息没带这个字段"，而不是"这个字段是 0"。** 这个坑在计费里非常致命。

## 8. 流式转换：每条流一个状态对象

```go
state, err := relayconvert.NewResponseStreamState(
	types.RelayFormatOpenAI,            // 源
	types.RelayFormatOpenAIResponses,   // 目标
	relayconvert.ResponseStreamOptions{
		ID: "resp_123", Model: "gpt-4.1",
		IncludeUsage: true, EmitSequenceNumber: true,
	})

for _, chunk := range upstreamChunks {
	results, err := relayconvert.ConvertStreamResponseChunk(ctx, meta, state, chunk)
	for _, r := range results { emit(r.Value) }     // 一个 chunk 可能产出多个下游事件
}

finalResults, err := relayconvert.FinalizeStreamResponse(ctx, meta, state)  // ★ 必须调用
for _, r := range finalResults { emit(r.Value) }

usage := state.Usage()
diagnostics := state.Diagnostics()
```

三个必须记住的点：

1. **一个上游 chunk 可能产出多个下游事件**（返回的是切片，不是单个值）。比如 Claude 的一个 `content_block_delta` 可能既要产出一个文本增量，又要更新工具调用参数。
2. **`FinalizeStreamResponse` 每条流都必须调用**。有些转换器在这一步才补发终止事件和最终 usage。忘了调 → 客户端永远等不到 `[DONE]`，且计费拿不到 usage。
3. **`Diagnostics()` 是累计的**，包括那些"没有产生任何下游事件"的损耗。

另外，目标是 Responses 时，上游中途出错可以调 `FailResponsesStream` 生成符合 Responses 协议的错误事件——**错误也要转换**，这是常被忽略的部分。

**SSE 的读写由宿主完成**：宿主把每个上游事件解析成对应 DTO 传进来，再把转换结果编码后发出去。这是模块边界的具体体现——relaykit 不知道 SSE 长什么样。

## 9. 模型名尾缀：把"每个模型的特性"从代码里赶出去

一类常见需求：`claude-sonnet-4-5-thinking`、`gemini-2.5-pro-thinking-8000`、`gpt-5-high`。这些尾缀表达的是**推理设置**，不是模型真名。

处理方式：

```go
// 1. 解析尾缀 → 得到意图
intent := reasoning.ParseClaudeModelSuffix(modelName, allowSuffixAlias)
// 2. 意图写入转换上下文
values.ReasoningConversion = reasoning.StateFromIntent(intent)
// 3. 裁掉尾缀的干净模型名放进请求
```

**这个模式的通用价值：** 让"用户能用模型名表达参数"变成一个**在网关边界解析的语法**，而下游永远只看到干净模型名和显式参数。这样：

- 客户端不用改代码就能切换推理强度；
- 上游供应商不需要知道这层语法；
- 计价可以基于干净模型名，不受尾缀影响。

## 10. 测试策略：golden snapshot

转换矩阵由 **golden test** 覆盖：把转换结果序列化后与快照文件比对，协议输出变化经确认是预期行为后更新快照：

```bash
GOWORK=off go test ./relayconvert -run TestGolden -update
```

**为什么协议转换特别适合快照测试？** 因为它的输出是"大而结构化、字段多、人工写断言成本极高"的 JSON。快照测试让"改动是否影响输出"变成一次 diff 审查——这在多协议互转这种高耦合场景里几乎是唯一可行的回归手段。

**自建建议：** 协议转换器写单元测试时，不要逐字段断言，直接 `assert.Equal` 整个 DTO，或者用 golden 文件。逐字段断言会让你在协议演进中疲于奔命。

## 11. 你自建的路线图

```
阶段 1：单协议透传
  只支持一种协议（OpenAI Chat），上游也是 OpenAI 兼容 —— 不做任何转换。

阶段 2：点对点双向
  加 Claude Messages。手写 Chat ↔ Claude 两个方向的转换器（用户最多的组合）。

阶段 3：引入中转枢纽
  再要加 Gemini/Responses 时，不要写 4×3 个转换器。
  选定 OpenAI Chat 作为枢纽协议，只写 每协议 ↔ Chat 的双向转换，
  其他组合自动走两跳。此时必须引入 Steps + Diagnostics 机制。

阶段 4：流式与状态
  加流式转换（ResponseStreamState）、工具调用账本（ResponsesToolState）、
  推理状态传递（ReasoningConversionState）。

阶段 5：损耗策略
  加 ToolLossPolicy，让不同用户（内部 vs 公开）用不同的容忍度。
```

**关键：阶段 3 是分水岭。** 在只有两种协议时（4 个方向的转换器）点对点更简单；一旦到三种以上，枢纽协议 + 多跳是唯一不爆炸的方案，但代价是必须把"损耗"显式化。

## 12. 本篇索引

| 关注点 | 文件 |
|---|---|
| 模块说明与支持矩阵 | `relaykit/README.md` |
| 协议 DTO | `relaykit/dto/` |
| 格式枚举与诊断类型 | `relaykit/types/`（`relay_format.go`） |
| 转换入口与注册表 | `relaykit/relayconvert/request_registry.go`、`response_registry.go` |
| 转换上下文 | `relaykit/relayconvert/convmeta/`（`meta.go`、`Options`） |
| 推理尾缀解析 | `relaykit/relayconvert/reasoning/` |
| 编解码/日志钩子 | `relaykit/relayconvert/kitutil/` |
| 宿主侧包装 | `service/request_converter.go`、`service/response_converter.go` |
| 宿主实现 Meta | `relay/common/relay_info.go`（`GetOriginModelName` 等方法） |
| 快照测试 | `relaykit/relayconvert/*golden*` |

→ 继续阅读：[05 · 渠道、路由与负载均衡](05-routing-and-channels.md)
