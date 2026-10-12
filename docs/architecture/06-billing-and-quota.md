# 06 · 计费与配额：预扣、结算、退款，以及并发安全

> **这是整个项目最难的 300 行代码所在。** 协议转换写错了用户重试一次就好；计费写错了要么白送钱，要么多扣用户钱被投诉，而且往往是并发压力下才复现。本篇按"生命周期 → 定价体系 → 并发不变量 → 特殊路径"的顺序讲。

## 1. 为什么计费这么难

四个本质困难：

1. **金额在调用前未知**。你必须先冻结一笔钱（否则用户余额不足时上游已经烧了 token），调用后再按真实用量调整。
2. **上游的用量数据可能缺失或不可信**。流式响应常常不带 usage；有些兼容网关把累积 usage 塞在倒数第二帧里。
3. **并发**。同一个用户/令牌可能同时有几十个请求在飞，扣款必须原子。
4. **不可逆性**。上游调用一旦发生就是既成事实，扣款失败不能当作没发生。

这个项目的对策可以总结成一句话：

> **预扣保证不超支，结算保证准确，退款保证不出错，幂等标志保证不重复。**

## 2. 记账单位

内部货币是整数 **quota**，不用 float 记账（避免累加误差）。

| 常量 | 值 | 含义 |
|---|---|---|
| `QuotaPerUnit` | `500 * 1000` | **50 万 quota = 1 美元** |
| `MaxQuota` | `math.MaxInt32` | 单次请求上限（防溢出） |
| `MaxWalletQuota` | `1<<53 - 1` | 钱包上限（JS 安全整数，前端不丢精度） |
| `USD2RMB` | 7.3 | 展示汇率 |

所有浮点 → 整数的转换都必须走 `common/quota_math.go` 的函数族：

```
QuotaFromFloat / QuotaFromFloatChecked / QuotaFromFloatStrict
QuotaRound    / QuotaRoundChecked    / QuotaRoundStrict
QuotaFromDecimal(Checked|Strict)
```

三档语义值得记住：

| 变体 | 溢出/异常时 | 用在哪 |
|---|---|---|
| `Strict` | **返回错误**（fail-fast） | 预扣 —— 宁可拒绝请求，也不能按错误金额冻结 |
| `Checked` | 饱和到边界 + 返回标记供记录 | 结算、日志 |
| 基础版 | 静默饱和 | 展示类计算 |

饱和的类型记在 `QuotaClamp`（`overflow` / `underflow` / `nan`），并通过 `AdminMap()` 写进日志的 `admin_info.quota_saturation` 字段，供管理员排查。**"数值异常要留痕"是账务系统的硬要求。**

另外注意一个细节：分层计费（tiered）**必须**用 `QuotaRound`（在 `pkg/billingexpr/round.go` 里有别名），否则预扣与结算会差 ±1 quota，日志里天天有噪声差异。

## 3. 生命周期：四阶段

```
① 预估 + 预扣 (freeze)
     PrepareRequestBilling
       → EstimateRequestToken        估算 prompt tokens
       → ModelPriceHelper            算 PriceData.QuotaToPreConsume
       → PreConsumeBilling           创建 BillingSession 并冻结

② 上游调用（可能重试多次；每次可能 Reserve 追加冻结）

③ 结算 (settle)
     PostTextConsumeQuota / PostAudioConsumeQuota / PostWssConsumeQuota
       → SettleBilling(actualQuota)
       → BillingSession.Settle → delta = actual - preConsumed

④ 失败退款 (refund)
     defer RefundFailedRequestBilling → BillingSession.Refund（异步 + 幂等）
```

### ① 预扣：入口与资金源

```go
// relay/request_billing.go:25
func PrepareRequestBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.SetEstimatePromptTokens(...)              // 估算输入 token
	priceData := helper.ModelPriceHelper(...)      // 算价 → PriceData
	if priceData.FreeModel { return nil }          // 免费模型不预扣
	return service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info)
}
```

资金源的选择由用户的计费偏好决定（`service/billing_session.go:381` `NewBillingSession`）：

```
subscription_only    只用订阅额度
wallet_only          只用钱包
wallet_first         先钱包，不够走订阅
subscription_first   先订阅，不够走钱包
```

**这个抽象（`FundingSource`）是必需的**，因为一旦有"订阅包 + 钱包"两种钱，扣款顺序就成了业务规则，散落在各处的 `if` 会立刻失控。

### ②③ 结算：只处理差值

```go
// service/billing_session.go:47
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if s.settled { return nil }                 // ★ 幂等：已结算就跳过
	delta := actualQuota - s.preConsumedQuota
	if delta == 0 { s.settled = true; return nil }

	if !s.fundingSettled {                      // ★ 防止对已提交的资金再次结算
		if err := s.funding.Settle(delta); err != nil { return err }
		s.fundingSettled = true
	}
	// 令牌额度同步调整：delta > 0 扣，delta < 0 退
	if delta > 0 { model.DecreaseTokenQuota(...) } else { model.IncreaseTokenQuota(..., -delta) }
	s.settled = true
	return nil
}
```

**`delta` 语义表：**

| 情况 | 含义 | 处理 |
|---|---|---|
| `delta > 0` | 预扣少了（实际用得更多） | 补扣；钱包**允许扣成负数** |
| `delta < 0` | 预扣多了 | 退还差额 |
| `delta == 0` | 估算准确（每次固定价的场景） | 只标记 settled |

**允许负余额是一个有意的设计决策。** 因为上游已经执行完了，此时拒绝"补扣"等于白送；所以选择"记成负余额，下次充值/下一请求时自然被拦下"。`model.DecreaseUserQuota` 就是无条件的 `quota = quota - ?`。反过来，**加钱**（`IncreaseUserQuota`）则严格校验上限和负数输入——**放宽扣款、收紧加款**，这个不对称是对的。

### ④ 退款：异步 + 幂等

```go
// service/billing_session.go:88
func (s *BillingSession) Refund(c *gin.Context) {
	// 用 gopool.Go 异步执行；refunded/settled/fundingSettled 三重幂等守卫
	// ★ fundingSettled 为真时绝不退款 —— 否则会把已结算的钱又退一次，余额凭空翻倍
}
```

守卫标志的语义：

| 标志 | 含义 | 效果 |
|---|---|---|
| `settled` | 已结算成功 | 不再结算、不再退款 |
| `refunded` | 已退款 | 不再退款 |
| `fundingSettled` | 资金源已提交结算 | **禁止退款**（防重复退款） |
| `trusted` | 走了信任跳过 | 预扣为 0，结算时全额收 |

`WalletFunding.Refund` 被文档明确标注为**非幂等**，所以它只在互斥锁内被调用一次。订阅退款则是事务式的，按 `requestId` 实现幂等重试（`refundWithRetry`）。

**这是一段"用标志位表达状态机"的典型代码。** 如果你自建，建议显式建模成状态机（`init → preConsumed → settled | refunded`），用非法状态转换直接 panic，而不是靠四个 bool 的组合正确性。

## 4. 预扣金额怎么算

`relay/helper/price.go` 里有四条路径：

| 路径 | 公式 | 用在哪 |
|---|---|---|
| **Ratio（默认）** | `promptTokens × InputPreConsumeMultiplier × modelRatio × groupRatio` | 大多数 chat 模型 |
| **Price（按次）** | `modelPrice × QuotaPerUnit × groupRatio` | 固定单价的模型 |
| **Per-call**（MJ/Task） | price 路径同上；ratio 路径预扣 `modelRatio/2 × QuotaPerUnit × groupRatio` | 图片、异步任务 |
| **Tiered（表达式）** | 用 `C=0` 跑表达式 → `rawCost / 1e6 × QuotaPerUnit` → × 分档倍数 → × groupRatio | 分层定价模型 |

**注意 Ratio 路径只预扣 prompt tokens，输出在结算时收。** 这是因为输出长度在调用前完全未知，如果按最大可能预扣会大量误拒请求。

`InputPreConsumeMultiplier` 是个可配置的安全系数（预扣倍数），用来抵消"输入 token 估算偏低"的风险。

## 5. 倍率体系

计费的最终价格由一组倍率相乘得到：

| 倍率 | 来源 | 作用 |
|---|---|---|
| `modelRatio` | `ratio_setting.GetModelRatio` | 基础 token 单价 |
| `completionRatio` | `GetCompletionRatio` | 输出 token 相对输入的倍数 |
| `groupRatio` | `GetGroupRatio` | 用户分组的折扣/加价 |
| `groupGroupRatio` | `GetGroupGroupRatio(userGroup, usingGroup)` | 某分组**使用**另一分组的特殊定价 |
| `cacheRatio` / `cacheCreationRatio` | 缓存读 / 写 | 缓存命中通常更便宜 |
| `imageRatio` / `audioRatio` / `audioCompletionRatio` | 多模态 | 按模态计价 |
| `otherRatios` | `types.PriceData` 自由附加倍率 | 图片张数 `n`、视频秒数/分辨率、任务时长 |

最终文本计费公式（`service/text_quota.go`）近似为：

```
quota = (base·1 + cached·cacheRatio + image·imageRatio + cacheCreate·cacheCreationRatio + completion·completionRatio)
        × modelRatio × groupRatio
        + 音频输入专项价格
        + 工具调用附加费
```

`otherRatios` 是一个**通用扩展点**（`PriceData.AddOtherRatio` / `ApplyOtherRatiosToFloat` / `RemoveOtherRatiosFromFloat`）：任何新的计价维度（张数、秒数）都塞进这里，而不需要给 `PriceData` 加字段。**"用 map 承载可扩展倍率"是避免改结构体的实用技巧**，代价是没有编译期字段检查。

**定价是按「模型 + 分组」解析的，没有渠道级定价。** 渠道只通过"模型映射改了上游模型名"间接影响价格。这个边界很干净，值得保持：**渠道是资源，模型是商品，价格挂商品不挂资源。**

## 6. 表达式定价（`pkg/billingexpr/`）★

### 它解决什么问题

倍率体系能表达"输入/输出/缓存不同价"，但表达不了：

- 上下文长度分档计价（0-128K 一个价，128K-1M 另一个价）；
- 时间段差异定价（夜间折扣）；
- 按请求固定收费 + 按量叠加（`fixed(0.01) + p*2.5/1e6`）；
- 按请求头/参数差异定价（某个 beta header 加价 6 倍）。

硬编码这些只会让计费代码变成一团 if。项目的解法是把定价变成**一种可配置的表达式语言**。

### 语法要点

- 版本前缀 `v1:`（默认）；
- **变量**：`p` `c`（prompt/completion tokens）、`len`（完整输入长度）、`cr`（缓存读）、`cc` / `cc1h`（缓存写，按 5 分钟/1 小时）、`img` / `img_cr` / `img_o`、`ai` / `ao`（音频输入/输出）、`image_count`；
- **函数**：`tier(name, value)`（分层，会记录命中的档位名）、`fixed(amount)`（按次固定价）、`param(path)`（从请求体按 gjson 路径取值）、`header(key)`、`has(src, sub)`、时间函数 `hour/minute/weekday/month/day(tz)`、`max/min/abs/ceil/floor`、`u(key)`（任务用量事实）；
- **请求规则**：在主表达式后用 `|||` 追加，例如：

```
p*2.5/1000000 + c*10/1000000 ||| when(header("anthropic-beta") has "fast-mode") * 6
```

- **系数单位是"美元 / 百万 token"**：`p * 2.5` 表示输入 $2.50/M tokens。这让表达式对运营人员可读。
- **智能排除**：只有当表达式引用了某个变量，对应的 token 子类才会从 `p`/`c` 里排除，避免重复计价。

### 评估入口

```go
// pkg/billingexpr/run.go
func RunExpr(exprStr string, params TokenParams) (float64, TraceResult, error)
func RunExprWithRequest(exprStr string, params TokenParams, request RequestInput) (float64, TraceResult, error)
func RunExprByHash(exprStr, hash string, params TokenParams) (float64, TraceResult, error)
func RunExprByHashWithRequest(exprStr, hash string, params TokenParams, request RequestInput) (float64, TraceResult, error)
```

执行环境（`runProgram`）：

```go
env := map[string]any{
	"image_count": float64(imageCount),
	"p": params.P, "c": params.C, "len": params.Len,
	"cr": params.CR, "cc": params.CC, "cc1h": params.CC1h,
	"img": params.Img, "img_cr": params.ImgCR, "img_o": params.ImgO,
	"ai": params.AI, "ao": params.AO,
	"tier":  func(name string, value float64) float64 { trace.MatchedTier = name; return value },
	"fixed": func(amount float64) float64 { trace.FixedPrice = &amount; return amount * 1_000_000 },
	// param / header / hour / u ...
}
out, err := expr.Run(prog, env)
```

### 四个可以借鉴的工程细节

1. **`TraceResult`：执行轨迹要留痕。** 表达式返回 `float64`，但额外记录了 `MatchedTier`、`BillingUnit`、`FixedPrice`。这些会写进消费日志（`InjectTieredBillingInfo` 往 `Other` 里塞 `billing_mode`、`expr_b64`、`matched_tier`、`billing_tokens`…）。**用户问"为什么这次贵了"时，日志里能给出完整推导。**
2. **编译缓存 + 版本哈希**：`CompileFromCache` / `CompileFromCacheByHash`，SHA-256 为键、上限 256 条，`InvalidateCache` 手动失效。表达式是热路径代码，**不缓存编译结果会直接把 CPU 打满。**
3. **定金/快照（`BillingSnapshot`）**：预扣时把表达式和当时的计价参数存进快照，结算时**按快照重新计算**而不是按当前配置。这样运营在请求飞行途中改了价格，也不会影响在途请求。
4. **`UsedVars` / `UsesFixedPricing`**：静态分析表达式用到了哪些变量，用于（a）决定从 `p`/`c` 中排除哪些子类，（b）判断是否按次计价。**"先分析、再执行"让动态表达式有了静态的可预测性。**

### 兜底顺序

`service/tiered_settle.go:TryTieredSettle` 失败时会退回倍率计价。**任何可配置的计价引擎都必须有兜底路径**，否则一条写错的表达式会让整条链路无法计费。

## 7. 并发安全：真正的核心（★）

计费出线上事故，99% 在并发。项目的防线有三层：

### 第一层：原子的"检查并扣减"

`model/quota_reserve.go`：

```go
func TryReserveUserQuota(id int, quota int) (bool, error)
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool) (bool, error)
```

**Redis 路径**：Lua 脚本做 `HGET` 比较 + `HINCRBY`，保证 check-and-deduct 原子（`userQuotaReserveScript` / `tokenQuotaReserveScript`）。缓存未命中时先 hydrate；Redis 出错则回退 DB。

**DB 路径**：条件 UPDATE

```sql
UPDATE users SET quota = quota - ? WHERE id = ? AND quota >= ?
-- 检查 RowsAffected == 1
```

**关键点：`WHERE quota >= ?` 是并发安全的全部。** 两个并发请求不可能同时通过这个条件并同时扣减——数据库的行锁保证了这一点。相比之下，`SELECT ... 判断 ... UPDATE` 的写法在并发下必然超支。

**补偿机制**：如果缓存层扣减成功但 DB 持久化失败，要通过 `cacheApplyUserQuotaDelta` 把缓存**补偿回去**，否则会凭空多出额度。

### 第二层：请求级的状态机幂等

`BillingSession` 的 `mu sync.Mutex` + `settled` / `refunded` / `fundingSettled` / `trusted` 标志（见 §3③④）。

### 第三层：任务类结算的"恰好一次"

异步任务完成时可能被多个节点/多次轮询同时看到。项目用 **CAS（compare-and-swap）状态更新**决定赢家：

```go
task.UpdateWithStatus(...)   // 状态转换成功的那一个才做结算
```

只有 CAS 成功的执行者结算，其他执行者跳过。**这是分布式"恰好一次"的标准做法：用一次原子状态转换来选举执行者。**

### 信任跳过（性能优化）

```go
func (s *BillingSession) shouldTrust(c *gin.Context) bool {
	// 仅钱包（不含订阅）、非 ForcePreConsume、非图片请求，
	// 且用户余额与令牌额度都 > TrustQuotaUSD × QuotaPerUnit 时，
	// 跳过预扣，在结算时一次性全额收
}
```

**条件限制得很保守**（不适用订阅、不适用强制预扣、不适用图片），因为图片/任务类请求的金额大且不可回收。**优化一定要有明确的安全边界，并把边界写在代码里。**

### 必须写进测试的关键区段

```
BillingSession.{preConsume, Settle, Refund, Reserve}     互斥 + 幂等
model.TryReserveUserQuota / TryReserveTokenQuota         Lua / 条件 UPDATE
FundingSource.{Settle, Refund}                           钱包 / 订阅各自语义
model.mutateModelPricingOptions                          事务 + lockForUpdate + 版本冲突
model/pricing.go updatePricing()                         双检锁
```

注意 `lockForUpdate` 在本项目里是**共享 helper**（`model/main.go`），因为它要为 SQLite 自动跳过（SQLite 不支持 `FOR UPDATE`）。**多数据库项目里，"写一次、三库都能跑"的锁必须封装。**

## 8. 流式 usage 的核算

上游流式响应常常不给 usage。处理链条：

```
1. 优先取上游返回的 usage
   - OpenAI：缓冲最后两帧，先看最后一帧，再回退看倒数第二帧
     （某些兼容网关把累积 usage 放在倒数第二帧）
   - 只在 service.ValidUsage(...) 为真时才接受
2. 上游完全没给 → 用响应文本估算
   service.ResponseText2Usage(c, responseText, modelName, promptTokens)
   → tokenkit.Estimate(...)（按模型家族的启发式权重）
3. PostTextConsumeQuota 收到 usage == nil → 回退到预扣时估算的 prompt tokens
4. ★ 零 token 请求 → 判定为「上游可能超时」
   → 配额强制置 0，并把预扣退还
```

第 4 条尤其值得注意：**"零用量"在流式场景里是异常信号而不是"免费"**。如果直接按 0 计费，用户就从上游超时里白赚了。项目选择退款 —— **宁可少收，不可错收**。

另外 `dto.MergeUsageNonZero` 的合并规则（**后到的非零覆盖，后到的零值不覆盖**）在流式计费里是必须的，理由见 04 篇 §7。

Realtime（WebSocket）走的是**增量计费**：`PreWssConsumeQuota` 在每个 chunk 上按实际 usage 扣款并记账，而不是等连接结束 —— 因为长连接可能持续几十分钟，等结束再算的敞口太大。

## 9. 特殊计费路径

| 路径 | 关键点 | 文件 |
|---|---|---|
| **图片生成** | 按**出站张数**分次预留；`UsePrice` 时 `× n`；结算用 `RequestedImageCount` / `BillingImageCount` 对账（用户请求 n 张，上游可能实际返回 m 张，**按实际计费**） | `service/image_billing.go`、`relay/image_handler.go` |
| **异步任务（视频/音乐）** | 提交时 `ForcePreConsume` 全额预扣；落库后结算；完成后按优先级重算：① 分层快照 ② 保持按次 ③ 适配器 `AdjustBillingOnComplete` ④ 按 token 重算；失败退款 | `relay/relay_task.go`、`service/task_billing.go`、`service/task_polling.go` |
| **超时任务** | `sweepTimedOutTasks` 用 CAS 把超时任务置失败并退款 | `service/task_polling.go:70` |
| **音频 / TTS / Whisper** | 独立的 `audioRatio` / `audioCompletionRatio` 维度；时长 → token 换算 | `service/quota.go:292` |
| **Realtime / WebSocket** | 增量预扣 + 增量结算 | `service/quota.go` `PreWssConsumeQuota` / `PostWssConsumeQuota` |
| **工具调用附加费** | `/v1/alpha/search` 这类零 token 但可计费的调用 | `service/text_quota.go` `calculateTextToolCallSurcharge` |
| **违规费** | 在正常退款**之后**对特定违规内容收费 | `service/violation_fee.go:103` `ChargeViolationFeeIfNeeded` |

**任务类计费的跨请求上下文**：异步任务在提交和完成之间隔了很久，价格配置可能变了。所以任务把计价要素存进 `task.PrivateData.BillingContext`（`TaskBillingContext`：ModelPrice、GroupRatio、ModelRatio、OtherRatios、OriginModelName、PerCallBilling、TieredSnapshot），完成时按这份快照结算。**凡是"跨时间"的计费，都必须存计价快照。**

## 10. 日志与账本

消费记录 `model.Log` 的字段：`UserId, Type, Content, Username, TokenName, ModelName, Quota, PromptTokens, CompletionTokens, UseTime, IsStream, ChannelId, ChannelName, TokenId, Group, Ip, RequestId, UpstreamRequestId, Other`。

**日志类型枚举刻意不用 iota**（`LogTypeUnknown/Topup/Consume/Manage/System/Error/Refund/Login` 用固定数字），因为要保证历史数据可读 —— **持久化枚举的又一条铁律**（和 05 篇的渠道 `Type` 同理）。

`Other` 字段承载计费上下文，由 `service/log_info_generate.go` 生成：

```
GenerateTextOtherInfo / GenerateClaudeOtherInfo / GenerateAudioOtherInfo / GenerateWssOtherInfo
InjectTieredBillingInfo   → billing_mode, expr_b64, matched_tier, billing_unit,
                            fixed_price, request_rules, billing_tokens, image_count
appendBillingInfo         → billing_source, 订阅 id 与余量
appendUsageBillingPathForLog → 计费走的是哪条路径
attachQuotaSaturation     → admin_info.quota_saturation（仅管理员可见）
```

**"一次消费日志能完整复盘这笔钱怎么算出来的"是账务系统的验收标准。** 你的日志如果只有一个金额，出了争议就无法自证。

**日志库物理隔离**：`LOG_DB` 可以独立配置，主库不支持 ClickHouse 但日志库支持（`chooseDB` 会拒绝主库用 ClickHouse）。ClickHouse 上有三处特殊处理：无自增 ID（`assignDisplayLogIds` 手动分配展示 ID）、固定排序（`created_at desc, request_id desc`）、LIKE 转义（`sanitizeClickHouseLikePattern`）、保留期用 `ALTER TABLE ... DELETE ... SETTINGS mutations_sync = 1`。

注意：**日志是立即写的，但余额更新可以批量**（`BATCH_UPDATE_ENABLED`）。两者语义不同，不要混为一谈——日志是审计凭证，不能丢。**"账本必须同步落盘，余额可以最终一致"这个区分很重要。**（余额的最终一致靠 Redis 里的原子扣减保证不超支，DB 只是持久化。）

## 11. 自建骨架

### 不变量清单（先写测试，再写实现）

```
I1  任何时刻 sum(冻结) + sum(已扣) <= 用户可用额度      → 原子预扣
I2  一次请求只结算一次、只退款一次                      → 状态机 + 标志
I3  结算金额与日志金额完全一致                          → 单一结算入口
I4  没有 usage 时不按 0 计费                            → 退款 + 告警
I5  跨时间的计费按快照结算，不按当前配置                → 存快照
I6  数值溢出/NaN 必须留痕                               → QuotaClamp + 日志
I7  撤销（禁用/删除）不改变历史账本                      → 只追加，不回写
```

### 最小可用实现

```go
type BillingSession struct {
	mu             sync.Mutex
	relayInfo      *RelayInfo
	funding        FundingSource     // 钱包 or 订阅
	preConsumed    int
	settled        bool
	refunded       bool
	fundingSettled bool
	trusted        bool
}

// 预扣：先冻结，再调用上游
func (s *BillingSession) PreConsume(quota int) error {
	if s.shouldTrust() { s.trusted = true; return nil }
	if _, err := TryReserve(s.relayInfo.UserId, quota); err != nil { return err }
	s.preConsumed = quota
	return nil
}

// 结算：只处理 delta，幂等
func (s *BillingSession) Settle(actual int) error { /* 见 §3② 骨架 */ }

// 退款：只在未结算时生效
func (s *BillingSession) Refund() error { /* 幂等守卫 */ }
```

### 落地顺序（这是从零做计费的正确顺序）

```
1. 整数账本 + QuotaFromFloat 系列（先把数值安全做好）
2. 原子预扣（条件 UPDATE 起步，Redis 可以后加）
3. 固定价的模型（UsePrice 路径）—— 最简单，先跑通全链路
4. 倍率计费（Ratio 路径）+ 结算 delta
5. 失败退款 + 幂等状态机
6. 消费日志（含完整计费上下文）
7. 分组倍率、缓存倍率、多模态倍率
8. 表达式/分档定价（有真实需求再上）
9. 订阅 + 钱包双资金源
10. ClickHouse 日志库、批量写、配额对账
```

**第 4 步之前不要碰表达式定价。** 表达式引擎是"定价策略已经复杂到配置表装不下"时才需要的工具，提前引入只会让简单问题变复杂。

## 12. 本篇索引

| 关注点 | 文件 |
|---|---|
| 生命周期编排 | `relay/request_billing.go`、`service/billing.go`、`service/billing_session.go`、`service/funding_source.go` |
| 定价计算 | `relay/helper/price.go`、`types/price_data.go`、`setting/ratio_setting/*` |
| 表达式引擎 | `pkg/billingexpr/{run,compile,settle,fixed,round,types}.go`、`pkg/billingexpr/expr.md` |
| 数值安全 | `common/quota_math.go` |
| 并发预扣 | `model/quota_reserve.go`、`model/user.go`、`model/token.go` |
| 文本/音频结算 | `service/text_quota.go`、`service/quota.go`、`service/billing_usage.go` |
| 特殊路径 | `service/image_billing.go`、`service/task_billing.go`、`service/task_polling.go`、`service/violation_fee.go` |
| Token 计数 | `tokenkit/`、`service/token_counter.go`、`service/usage_helpr.go` |
| 日志 | `model/log.go`、`service/log_info_generate.go`、`common/database.go` |
| 定价配置读写 | `model/pricing*.go`、`model/model_pricing_config.go` |

→ 继续阅读：[07 · 认证与访问控制](07-auth-and-access-control.md)
