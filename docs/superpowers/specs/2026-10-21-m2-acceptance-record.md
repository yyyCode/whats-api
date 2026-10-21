# M2 验收记录：计费闭环

执行时间：2026-10-21 19:10–19:20 (+08:00)
执行环境：Windows 11（10.0.22621）、`go version go1.26.5 windows/amd64`
分支：`feat/m2-billing`（自 `main` 的 `c3bf062` 拉出）
范围：请求级预扣、按 usage 结算、失败退款、并发不超支、可复盘的消费日志。

## 五条验收标准

| # | 标准（路线图 M2） | 覆盖用例 | 结果 |
|---|---|---|---|
| ① | 并发 200 个请求打同一个余额只有 100 次请求额度的令牌，成功数 ≤ 100，余额不为负 | `TestConcurrentReserveUserQuotaNeverOverdraws`、`TestConcurrentReserveTokenQuotaNeverOverdraws`（各 200 goroutine / 100 quota，恰好 100 成功）、`TestConcurrentRelayNeverOverdraws`（HTTP 层 30 并发） | **PASS** |
| ② | 上游失败时预扣全额退还 | `TestRelayRefundsWhenAllAttemptsFail`、`TestRelayRefundLogsNoChannelReason`、`TestBillingRefundIsIdempotent` | **PASS** |
| ③ | 上游不给 usage 时不按 0 计费（退款 + 告警） | `TestRelayRefundsWhenUsageMissing`（余额恢复 + `LogTypeError` + `refunded=true`）、`TestBuildBillingErrorLogRecordsRefundFailure`（退款失败时如实记 `refunded=false`） | **PASS** |
| ④ | 日志能完整复盘「这笔钱怎么算出来的」 | `TestRelayChargesFromUsage`（`other` 含倍率、估算/实际 token、预扣、结算、饱和标记、渠道与请求身份）、`TestRelaySettlesWithFrozenPricing`（请求途中调价，仍按预扣时冻结的倍率结算并记录） | **PASS** |
| ⑤ | 一次请求只结算一次 | `TestBillingPreConsumeAndSettle`（重复 Settle 返回 false）、`TestBillingConcurrentSettleOnlyOnce`、`TestRelaySettlesOnceAcrossRetries` | **PASS** |

附加验证：额度不足在选渠道之前拒绝（`TestRelayRejectsInsufficientQuota`，上游 0 命中）、流式按 usage 计费（`TestRelayStreamsUsageAndCharges`）、流式请求向上游补 `include_usage`（`TestRelayStreamRequestsIncludeUsage`）、免费模型/最小 1 quota/分组倍率/多模态估算（service 与 ratio_setting 单测）、负 usage 与负倍率被拒绝（`TestUsageValid`、`TestCalculateTextQuotaClampsNegativeTokens`、`TestNegativeRatioKeepsPreviousSnapshot`）、无限令牌仍累计 `used_quota`（`TestBillingUnlimitedToken`）。

## 全量门禁（真实输出）

```text
$ go build ./...
build: OK

$ go vet ./...
vet: OK

$ gofmt -l .
gofmt: clean

$ go test -count=1 ./...
?       github.com/yyyCode/whats-api    [no test files]
ok      github.com/yyyCode/whats-api/cmd/optctl                       19.173s
ok      github.com/yyyCode/whats-api/internal/app                     47.595s
ok      github.com/yyyCode/whats-api/internal/cachex                   5.594s
ok      github.com/yyyCode/whats-api/internal/common                   5.948s
ok      github.com/yyyCode/whats-api/internal/constant                 2.430s
ok      github.com/yyyCode/whats-api/internal/controller              17.265s
ok      github.com/yyyCode/whats-api/internal/logger                   3.257s
ok      github.com/yyyCode/whats-api/internal/middleware              15.250s
ok      github.com/yyyCode/whats-api/internal/model                   10.790s
ok      github.com/yyyCode/whats-api/internal/relay                    8.720s
?       github.com/yyyCode/whats-api/internal/relay/channel           [no test files]
ok      github.com/yyyCode/whats-api/internal/relay/channel/openai     7.662s
ok      github.com/yyyCode/whats-api/internal/relay/common             7.625s
ok      github.com/yyyCode/whats-api/internal/relay/helper             9.450s
ok      github.com/yyyCode/whats-api/internal/router                  14.994s
ok      github.com/yyyCode/whats-api/internal/service                 11.651s
ok      github.com/yyyCode/whats-api/internal/setting/config           3.642s
ok      github.com/yyyCode/whats-api/internal/setting/ratio_setting    4.275s
ok      github.com/yyyCode/whats-api/internal/setting/system_setting   3.597s
ok      github.com/yyyCode/whats-api/internal/types                    5.098s

$ go test -race -count=1 ./internal/relay/... ./internal/service/ ./internal/controller/ ./internal/middleware/ ./internal/model/
ok      github.com/yyyCode/whats-api/internal/relay                   29.780s
ok      github.com/yyyCode/whats-api/internal/relay/channel/openai     7.938s
ok      github.com/yyyCode/whats-api/internal/relay/common             6.714s
ok      github.com/yyyCode/whats-api/internal/relay/helper            19.333s
ok      github.com/yyyCode/whats-api/internal/service                 30.290s
ok      github.com/yyyCode/whats-api/internal/controller              29.550s
ok      github.com/yyyCode/whats-api/internal/middleware              29.236s
ok      github.com/yyyCode/whats-api/internal/model                   27.531s
```

> `-race` 只压在转发、计费、选路、鉴权、数据层这些并发敏感的包上，**没有对全仓跑**。

## 验收用例的逐条输出

```text
$ go test ./internal/controller/ -run 'TestRelay|TestConcurrent' -v -count=1
--- PASS: TestRelayChargesFromUsage (0.05s)
--- PASS: TestRelaySettlesWithFrozenPricing (0.07s)
--- PASS: TestRelayRefundLogsNoChannelReason (0.07s)
--- PASS: TestRelaySettlesOnceAcrossRetries (0.05s)
--- PASS: TestRelayRefundsWhenAllAttemptsFail (0.04s)
--- PASS: TestRelayRefundsWhenUsageMissing (0.04s)
--- PASS: TestRelayRejectsInsufficientQuota (0.05s)
--- PASS: TestRelayStreamsUsageAndCharges (0.04s)
--- PASS: TestRelayStreamRequestsIncludeUsage (0.04s)
--- PASS: TestConcurrentRelayNeverOverdraws (0.22s)
--- PASS: TestRelayNonStreamPassthrough (0.05s)
--- PASS: TestRelayAppliesModelMapping (0.05s)
--- PASS: TestRelayRetriesOnUpstream5xxAndDisablesChannel (0.09s)
--- PASS: TestRelayDoesNotRetryAfterStreamStarted (0.05s)
--- PASS: TestRelayStreamsIncrementally (0.60s)
--- PASS: TestRelayStopsUpstreamWhenClientDisconnects (0.09s)
--- PASS: TestRelayDoesNotFollowUpstreamRedirect (0.05s)
--- PASS: TestRelayAuthFailureUsesOpenAIShape (0.05s)
ok      github.com/yyyCode/whats-api/internal/controller               8.224s

$ go test ./internal/model/ -run 'ConcurrentReserve' -v -count=1
--- PASS: TestConcurrentReserveUserQuotaNeverOverdraws (0.24s)
--- PASS: TestConcurrentReserveTokenQuotaNeverOverdraws (0.15s)

$ go test ./internal/service/ -run 'Billing|Settle|Refund|PreConsume|Frozen|Negative|ErrorLog' -v -count=1
--- PASS: TestBillingPreConsumeAndSettle (0.06s)
--- PASS: TestBillingPreConsumeFreeQuota (0.04s)
--- PASS: TestBillingRefundIsIdempotent (0.03s)
--- PASS: TestBillingPreConsumeInsufficientQuota (0.04s)
--- PASS: TestBillingSettleNegativeDelta (0.05s)
--- PASS: TestBillingUnlimitedToken (0.05s)
--- PASS: TestBillingConcurrentSettleOnlyOnce (0.04s)
--- PASS: TestBuildBillingErrorLog (0.00s)
--- PASS: TestBuildBillingErrorLogRecordsRefundFailure (0.00s)
--- PASS: TestCalculateTextQuotaUsesFrozenPricing (0.00s)
--- PASS: TestCalculateTextQuotaClampsNegativeTokens (0.00s)
--- PASS: TestEstimatePreConsumeQuotaFormula (0.00s)
```

这些用例不是 mock：真实 gin 路由、真实 SQLite 临时库、`httptest` 上游（真实 TCP），走的是生产代码路径。model 层的并发用例用 200 个 goroutine 对同一条余额各扣 1，断言恰好 100 个成功、最终余额为 0。

## 明确未验证事项（不谎称通过）

| 事项 | 原因 | 复核方式 |
|---|---|---|
| MySQL / PostgreSQL 上的并发预扣 | 本机无这两个服务；条件 UPDATE 是标准 SQL，但行锁行为未实测 | 起容器后把 `SQL_DSN` 指过去，重跑 `TestConcurrentReserve*` |
| Redis/Lua 预扣路径 | M2 明确不实现（只用条件 UPDATE） | 高并发场景再引入 |
| 真实 OpenAI 流式 usage 帧 | 需要真实密钥与网络；e2e 用假上游模拟 | 用真实 key 跑一次 `stream:true`，确认最后一帧带 usage 且被计费 |
| 真实 SDK 收到额外 usage 帧的行为 | 本机未安装 openai-python/node | 用 SDK 跑流式，确认不报错、内容完整 |
| 订阅额度 / 双资金源 | 本阶段只做钱包 + 令牌 | 后续阶段 |
| 缓存倍率、多模态倍率、表达式/分档定价 | 按设计文档的落地顺序后置 | 有真实需求再上 |
| 多实例并发与配置同步 | 单实例测试；预扣不依赖内存缓存，理论上天然多实例安全，未实测 | 两实例共享同一库，交替压测 |
| 结算写库失败后的对账 | 只验证了代码路径会记 Error 日志，未做故障注入 | 注入 DB 错误，确认日志中的 request_id 可人工补账 |
| 超过 60 秒的流式请求计费 | e2e 流很短 | 慢速真实上游跑一次长流 |
| 日志库独立部署（ClickHouse） | 计划在 M6 | 不在本阶段范围 |

## 与设计文档的偏离（实现时拍板）

| 项 | 实际做法 | 原因 |
|---|---|---|
| 流式请求体 | 自动补 `stream_options.include_usage=true`（合并客户端已有键） | 不补的话 OpenAI 兼容上游的流式响应不带 usage，所有流式请求都会退款，等于免费；代价是客户端多收到一帧 `choices: []` 的 usage 帧 |
| 无 usage 的处理 | 退款 + `LogTypeError`，**不估算**（设计文档 §8 有「按响应文本估算」的兜底） | M2 验收标准明确要求「退款 + 告警」；估算会引入另一套可能失真的金额 |
| 最小预扣 | 计算值 > 0 但取整为 0 时抬到 1 quota | 否则便宜模型可无限并发白嫖 |
| 资金源 | 只有钱包与令牌额度，无订阅、无信任跳过 | 控制 M2 范围；信任跳过是性能优化，不是正确性需求 |
| 结算差额 | 允许余额变负（`delta > 0` 无条件补扣） | 上游已执行，拒绝补扣等于白送 |
| 退款时机 | 同步执行，不另起 goroutine | 停机时不会写到已关闭的连接 |
| 倍率配置 | `ratio_setting` 用 JSON 字符串存整张表，`OnRebuild` 解析成原子快照 | 配置注册表只支持基础类型；热路径无锁读 |
| 非法倍率 JSON | 保留上一份快照并记 Error，不 panic | 一次写错的配置不该让服务起不来 |
| 模型倍率语义 | 美元 / 100 万输入 token；completion_ratio 是输出倍数 | 与运营价目表同单位，公式可直接写进日志 |
| 日志写入 | 同步写 `LOG_DB`；写失败只告警，不回滚账本 | 账本已按上游事实记下，日志丢失应告警补账，而不是把已完成的请求判失败 |
| 结算用的倍率 | 预扣时把四组倍率冻结进请求上下文，结算直接复用；不再结束前重读配置 | 请求途中调价时，金额与日志必须还能用同一版倍率复盘（`TestRelaySettlesWithFrozenPricing`） |
| 负用量/负倍率 | usage 任一字段为负即视为无效（退款 + 错误日志）；负倍率与非法 JSON 一样整体拒绝，保留旧快照 | 负金额会被取整成 0 等于免费；负倍率还会让预扣永久失败，等于把一个模型/分组下线 |
| 无限令牌的 used_quota | 结算时照常累加，只是不动 `remain_quota` | 否则最常见的无限令牌在报表里永远是 0，无法按 key 观察消费 |
| 退款失败的审计 | 只有确实退款成功才写 `refunded:true`；失败时写 `refunded:false` 并说明预扣仍冻结 | 虚假的「已退款」会让对账人员误以为余额已恢复 |

## 已知局限

- **流式中途客户端断开**：上游可能已产生费用，但拿不到 usage，按验收标准退款。这是「宁可少收，不可错收」的取舍。
- **结算写库失败**：响应已发出，只能在日志里留 `request_id`，需要人工对账；没有实现自动补偿任务。
- **令牌额度允许为负**：`delta > 0` 时补扣是无条件的，与钱包语义一致；下一次请求会被预扣拒绝。
