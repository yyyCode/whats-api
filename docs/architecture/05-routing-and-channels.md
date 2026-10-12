# 05 · 渠道、路由与负载均衡

> "给一个模型名，挑一个能用的上游账号"——听起来简单，但它是网关里**读最频繁、最容易成为瓶颈、也最容易出线上事故**的一步。本篇讲清楚渠道模型、选择算法、自动健康管理。

## 1. 什么是"渠道"

`model.Channel`（`model/channel.go:23`）= **一个上游账号 / 端点**：一组 API Key、一个 Base URL、它能提供的模型列表、它服务哪些用户分组、以及它在负载均衡里的优先级和权重。

```go
type Channel struct {
	Id      int
	Type    int      // ★ 决定用哪个 Adaptor（见 03 篇的 switch）
	Key     string   // 密钥；多 key 用换行分隔或 JSON 数组
	BaseURL string
	Models  string   // 逗号分隔的模型列表
	Group   string   // 逗号分隔的分组列表

	// 路由属性
	Priority *int64
	Weight   *uint
	Tag      *string

	// 状态
	Status   int  // 1=启用  2=手动禁用  3=自动禁用
	AutoBan  *int // 是否允许自动封禁

	// 元信息
	Name, TestModel      string
	ResponseTime, Balance *float64
	BalanceUpdatedTime    int64
	UsedQuota             int64

	// 行为覆盖（运营杠杆）
	ModelMapping      *string          // 模型名映射（见 §5）
	StatusCodeMapping *string          // 上游状态码 → 本地状态码
	ParamOverride     *string          // 强制改写请求参数
	HeaderOverride    *string          // 强制改写/追加请求头
	Setting           json.RawMessage  // 渠道额外设置

	// 多 Key 管理
	ChannelInfo ChannelInfo // IsMultiKey / MultiKeyStatusList / MultiKeyMode …
	Keys        []string    `gorm:"-"`  // 内存中解析后的 key 列表
}
```

**三个设计决策值得注意：**

1. **`Type` 是适配器选择器，且是持久化的整数。** 这意味着**枚举值不能改**（改了会破坏所有存量数据）。这类"持久化在 DB 里的枚举"必须从一开始就当作稳定 ABI 对待。
2. **优先级/权重/标签都是可选字段（指针类型）**。"未配置"和"配置为 0"是语义不同的：前者用默认值，后者是真的 0 权重。用指针区分"缺省"和"零值"是本项目贯穿始终的约定（在请求 DTO 里也一样）。
3. **运营杠杆被建模成渠道字段**（`ModelMapping`/`ParamOverride`/`HeaderOverride`）。这很关键：**能让运营在控制台上解决的问题，就不要让它变成一次代码发版。**

## 2. Ability 表：用空间换时间的索引设计

这是本项目最值得抄的一个数据建模技巧。

如果每次选渠道都去 `channels` 表里做 `WHERE models LIKE '%gpt-4o%' AND group LIKE '%default%'`，性能会立刻崩掉。所以项目维护了一张**反范式索引表** `model.Ability`（`model/ability.go:17`）：

```go
type Ability struct {
	Group     string `gorm:"primaryKey"`     // ← 用户分组
	Model     string `gorm:"primaryKey"`     // ← 模型名
	ChannelId int    `gorm:"primaryKey;index"`
	Enabled   bool
	Priority  *int64  `gorm:"index"`
	Weight    uint    `gorm:"index"`
	Tag       *string `gorm:"index"`
}
```

**一行 = 一个 (分组, 模型, 渠道) 三元组。**

- 写入侧：渠道的 `Models × Group` 展开成多行，由 `Channel.AddAbilities` / `UpdateAbilities` / `DeleteAbilities` 维护；`FixAbility()` 可以整表重建（`TRUNCATE` + 重建）。
- 读取侧：一次索引查询就能拿到"某分组下某模型的所有可用渠道"，并已按 `priority DESC, weight DESC` 排好序。

**这就是"查询模式驱动的数据建模"：** 不要去优化查询（加缓存、加索引），而是**把查询的结果预先存下来**。这个模式的适用条件是：

- 读极多、写极少（渠道变更频率远低于请求频率）；
- 组合维度有限（分组 × 模型）；
- 数据量可控（渠道数千 × 模型数百 → 几十万行，仍在单表舒适区）。

**代价与对策：** 反范式必然带来一致性风险——渠道改了但 Ability 忘了更新。项目的对策是**把一致性逻辑集中在渠道的写入路径上**（`UpdateChannelStatus` 同时改 memory cache + DB + Ability），并保留 `FixAbility()` 作为兜底修复手段。**自建时：如果你也做这种表，一定要同时做（a）集中的写入路径，（b）一键重建工具。**

## 3. 选择算法：优先级分层 + 层内加权随机

选择逻辑在内存缓存里执行（`model/channel_cache.go:117` `GetRandomSatisfiedChannel`），核心是两级结构：

```
候选集 = group2model2channels[usingGroup][model]      ← 内存索引，O(1) 取候选
   ↓ 按优先级分档（priority 降序）
   ↓ retry 索引决定用哪一档             ← ★ 重试 = 降级到下一优先级
   ↓ 档内按 weight 加权随机
选中的渠道 = channelsIDM[channelId]
```

关键代码（简化）：

```go
func GetRandomSatisfiedChannel(group string, model string, retry int,
	filters []Filter) (*Channel, error) {

	channels, _ := filterCandidateIDs(group2model2channels[group][model], model, filters)
	if len(channels) == 0 {
		// 归一化模型名再试一次（处理别名/后缀）
		normalizedModel := ratio_setting.RoutingMatchModelName(model)
		channels, _ = filterCandidateIDs(group2model2channels[group][normalizedModel], ...)
	}
	if len(channels) == 0 { return nil, nil }
	if len(channels) == 1 { return channelsIDM[channels[0]], nil }

	// ① 取出所有出现的优先级，降序排序
	sort.Sort(sort.Reverse(sort.IntSlice(uniquePriorities)))
	// ② ★ 重试次数映射到优先级档位（重试 = 用更差的渠道）
	if retry >= len(uniquePriorities) { retry = len(uniquePriorities) - 1 }
	targetPriority := int64(uniquePriorities[retry])

	// ③ 档内加权随机
	var sumWeight int
	for _, id := range channels {
		if ch := channelsIDM[id]; ch.GetPriority() == targetPriority {
			sumWeight += ch.GetWeight()
			targetChannels = append(targetChannels, ch)
		}
	}
	// ④ 平滑：权重太小或全为 0 时仍能随机分布
	smoothingFactor, smoothingAdjustment := 1, 0
	if sumWeight == 0 {
		sumWeight = len(targetChannels) * 100
		smoothingAdjustment = 100
	} else if sumWeight/len(targetChannels) < 10 {
		smoothingFactor = 100
	}

	randomWeight := rand.Intn(sumWeight * smoothingFactor)
	for _, ch := range targetChannels {
		randomWeight -= ch.GetWeight()*smoothingFactor + smoothingAdjustment
		if randomWeight < 0 { return ch, nil }
	}
	return nil, nil
}
```

### 三个值得学的地方

**① 重试 = 优先级降级。**
`retry` 计数直接用作"取第几档优先级"的索引。这样"重试"语义天然变成了**质量降级**：第一次用最好的渠道，失败后依次用次优渠道。不需要专门的重试策略表。

注意最后那句 `if retry >= len(...) { retry = len(...) - 1 }` —— **重试超出档位时停在最差档继续试**，而不是报错。这是有意的：宁可继续用差渠道，也不要因为档位用完直接失败。

**② 权重平滑因子。**
如果所有权重都是 0，直接 `rand.Intn(0)` 会 panic；如果权重都很小（平均值 < 10），随机精度会很不均匀。所以引入了 `smoothingFactor` / `smoothingAdjustment` 做缩放和平移。**这是典型的"防御性算术"：处理 0 权重、微小权重、单个渠道这些退化输入。** 你的负载均衡代码也一定需要这段。

**③ 选路结果不缓存。**
每次请求重新随机。缓存的是**候选集**（内存索引），不是**选中结果**——因为选中结果必须每次重新随机才能实现真正的负载均衡。会话粘性是通过独立的"亲和性"机制做的（见 §4），而不是靠缓存选路结果。**别把这两件事混在一起。**

### DB 回退路径

`MemoryCacheEnabled` 关闭时（未配 Redis 的单机小内存模式），同样的逻辑有个 DB 版本：`model/ability.go:107` 的 `GetChannel` 直接查 `abilities` 表，按 `priority DESC, weight DESC` 排序后做同样的分档 + 加权随机（DB 版本用加法下限 `weight + 10` 代替平滑因子）。

**架构启示：** 同一套算法有两份实现（内存 / DB）。这在工程上是重复，但换来了"小部署零依赖"。如果你的项目也要支持"轻量单机"和"集群"两种形态，这种双实现是常见代价——**至少要把两份实现写成同样的语义，并用同一组测试用例覆盖。**

## 4. 渠道亲和性：让同一会话尽量落在同一渠道

纯随机负载均衡有个副作用：多轮对话的每次请求可能落在不同渠道上，导致上下文缓存命中率暴跌（同一渠道内的 prompt cache 才有意义），对某些上游还会触发风控。

解法是**亲和性（affinity）**（`service/channel_affinity.go`）：

- 用 `cachex.HybridCache[int]`（Redis 或内存 LRU）维护 `会话标识 → 渠道 ID` 的映射，namespace `new-api:channel_affinity:v1`；
- **只在第一次尝试时生效**（`GetPreferredChannelByAffinity`），重试时不再粘住 —— 否则一个坏渠道会把会话永久钉死；
- 成功的渠道通过 `RecordChannelAffinity` 记录；
- 配置在 `setting/operation_setting/channel_affinity_setting.go`。

**这是"负载均衡"与"缓存友好"之间的经典权衡**：完全随机的负载最均匀，但会破坏上游的缓存命中；完全粘性则失去负载均衡与故障隔离。项目选择了"首次粘性 + 重试打破"的折中。

**还有个反例值得注意**：重试决策里有"严格会话亲和"会阻止切换渠道（见 `DecideRelayRetry`）。也就是说亲和性既影响选路，也影响重试策略 —— 这类"策略互相纠缠"的地方是 bug 高发区，自建时最好把亲和性策略集中在一个地方定义。

## 5. 模型名映射：别名链与环检测

渠道级配置 `ModelMapping` 是一张 `{"客户端模型名": "上游模型名"}` 的 JSON 表。解析在 `relay/helper/model_mapped.go` 的 `ModelMappedHelper`：

```
1. 读 gin.Context 里的 model_mapping（由 Distribute 阶段写入）
2. 沿映射链逐跳解析，记录 visitedModels 做环检测
3. 找不到精确匹配时，退化为「去掉推理后缀的基名」再查（reasoning.BaseModelName）
4. 命中则置 info.IsModelMapped = true，写 info.UpstreamModelName
5. request.SetModelName(info.UpstreamModelName)
```

**三个必要细节：**

- **环检测**：运营在控制台上很容易配出 `a→b, b→a`。没有 `visitedModels` 就是死循环。
- **后缀退化查找**：让 `gpt-4o-thinking` 能命中为 `gpt-4o` 配置的映射，避免运营为每个后缀都配一条。
- **`IsModelMapped` 标志**：后续逻辑（计价、上报名）需要知道"名字是不是被改过"，因为改写后会改变 `UpstreamModelName`，进而影响上游实际调用的模型。

另外，选路阶段还有一层归一化：`ratio_setting.RoutingMatchModelName(model)`（用于把带后缀/别名的模型名归一到选路用的规范名，见 §3 代码里的 fallback）。

**放的位置很重要：** 项目把"选路用的名字归一化"（`RoutingMatchModelName`）和"上游用的名字映射"（`ModelMapping`）分开了。前者用于**找渠道**，后者用于**改请求**。混在一起会导致"映射后的名字又去匹配了另一条映射"的诡异行为。

## 6. 自动健康管理

渠道会挂。运营不想半夜起床点按钮，所以项目做了自动化的禁用/恢复：

**配置项**（默认在 `common/constants.go` / `setting/operation_setting`）：

| 配置 | 含义 |
|---|---|
| `ChannelDisableThreshold = 5.0` | 响应时间超过 5 秒就算不健康 |
| `AutomaticDisableChannelEnabled` | 自动禁用总开关 |
| `AutomaticEnableChannelEnabled` | 自动恢复总开关 |
| `AutomaticDisableStatusCodes` | 哪些状态码触发禁用 |
| `AutomaticDisableKeywords` | 响应里出现哪些关键词触发禁用（用 AC 自动机匹配） |
| `AutomaticRetryStatusCodes` | 哪些状态码触发**重试**（与禁用是两回事） |

**判定逻辑**（`service/channel.go`）：

```go
func ShouldDisableChannel(err *types.NewAPIError) bool {
	// 自动禁用开启 && （错误是渠道类错误 || 状态码在禁用列表 || 消息命中禁用关键词）
	// 注意：标记了 SkipRetry 的错误被豁免
}

func ShouldEnableChannel(err, status) bool {
	// 仅当自动恢复开启 && 无错误 && 当前状态是「自动禁用」
	// ★ 手动禁用的渠道不会被自动恢复
}
```

**`ShouldEnableChannel` 的条件设计很有讲究**：只恢复"被自动禁用"的渠道（`ChannelStatusAutoDisabled`）。手动禁用的渠道（比如运营在续费过程中临时关掉的）绝不会被自动打开 —— **自动化不能覆盖人工意图**。

**多 Key 的独立健康状况**：`ChannelInfo.MultiKeyStatusList map[int]int` 记录每个 key 的状态，`GetNextEnabledKey()` 在随机/轮询模式下挑一个可用 key；全部不可用时返回 `ErrorCodeChannelNoAvailableKey`。也就是说**故障隔离粒度可以细到单个 key**，而不是整个渠道。

## 7. 后台健康检查：复用系统任务框架

渠道测试不是自己起一堆 goroutine，而是注册成**系统任务**（见 09 篇）：

- `controller/system_task_handlers.go:19` 的 `RegisterScheduledSystemTasks` 注册 `channelTestHandler`，周期来自 `monitor_setting`（`AutoTestChannelEnabled`、`AutoTestChannelMinutes`、`ChannelTestConcurrency`）；
- 执行在 `controller/channel-test.go`：`testChannelForHealthCheck` 在**失败**或**响应时间 > threshold** 时禁用渠道，并在 `ShouldEnableChannel` 通过时恢复；
- 有 `ChannelTestMode`，其中 `passive_recovery` 模式**只恢复**之前被自动禁用的渠道，不做全量测试（省上游配额）；
- 手动触发走 `TestAllChannels`，同样入队成 `channel_test` 系统任务。

**这个"业务动作复用统一调度框架"的做法值得抄：** 渠道测试、模型列表更新、异步任务轮询需要同一种能力（周期执行 + 多节点去重 + 有运行历史 + 可手动触发 + 可观测），那就只做一套框架。

## 8. 自建骨架

### 表设计

```sql
-- 渠道：运营单位
channels(id, type, key, base_url, models, group, priority, weight, tag,
         status, model_mapping, param_override, header_override, setting, ...)

-- ★ 选路索引（反范式）：一行 = (group, model, channel)
abilities(group, model, channel_id, enabled, priority, weight, tag,
          PRIMARY KEY(group, model, channel_id))
```

### 选路函数骨架

```go
func SelectChannel(group, model string, retry int) (*Channel, error) {
	// 1. 候选集：内存索引 O(1)
	ids := index[group][model]
	if len(ids) == 0 { ids = index[group][normalize(model)] }
	ids = applyFilters(ids, filters)          // 渠道约束（IP/任务插件/请求策略）
	if len(ids) == 0 { return nil, ErrNoChannel }

	// 2. 优先级分档
	tiers := distinctPrioritiesDesc(ids)
	if retry >= len(tiers) { retry = len(tiers)-1 }   // 档位用完就停在最差档
	tier := tiers[retry]

	// 3. 档内加权随机（含平滑）
	return weightedPick(ids, tier), nil
}
```

### 必做的三件事

1. **一个集中的写入路径**：任何渠道变更（改配置、改状态、增删）都走同一个函数，在里面同时更新 DB、内存索引、Ability 表。**不要允许旁路写。**
2. **一个重建工具**：`FixAbility()` 这种"整表重建"能力必须存在，用于索引损坏后的恢复。
3. **一个缓存失效约定**：本项目还记录了一个真实的坑——`channel_cache.go` 里注明 `channelSyncLock` 必须在**失效定价缓存之前**释放，否则会出现 AB-BA 死锁。**缓存之间的加锁顺序要写进代码注释。**

## 9. 坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| 用 `LIKE` 查渠道支持的模型 | 请求量上来后 DB 打满 | 反范式 Ability 索引表 |
| 持久化的 `Type` 枚举被改动 | 存量渠道全部失效 | 枚举值只增不改，视作 ABI |
| 缓存"选中的渠道" | 负载失衡，重试永远打同一个 | 只缓存候选集，选中结果每次重算 |
| 权重为 0 / 极小值 | panic 或分布不均 | 平滑因子 + 加法下限 |
| 映射配成环 | 死循环 | visited 集合 |
| 自动恢复覆盖手动禁用 | 运营操作被系统撤销 | 只恢复"自动禁用"状态的渠道 |
| 亲和性在所有尝试上生效 | 坏渠道被永久钉死 | 只在首次尝试生效 |
| 禁用渠道后不清 WebSocket | 长连接仍打在已禁渠道上 | 禁用时主动关闭该渠道的连接 |

## 10. 本篇索引

| 关注点 | 文件 |
|---|---|
| 渠道模型 | `model/channel.go` |
| 能力索引 | `model/ability.go`（`Ability`、`AddAbilities`、`FixAbility`）、`model/channel_constraint.go` |
| 缓存选路 | `model/channel_cache.go`（`InitChannelCache`、`SyncChannelCache`、`GetRandomSatisfiedChannel`） |
| 业务层选择 | `service/channel_select.go`（`SelectChannelForRequest`、`CacheGetRandomSatisfiedChannel`） |
| 亲和性 | `service/channel_affinity.go`、`setting/operation_setting/channel_affinity_setting.go` |
| 模型映射 | `relay/helper/model_mapped.go`、`relay/helper/reasoning_suffix.go`、`relay/helper/model_modifier.go` |
| 健康管理 | `service/channel.go`（`ShouldDisableChannel`、`DisableChannel`）、`controller/channel-test.go`、`setting/operation_setting/monitor_setting.go` |
| 渠道 CRUD | `controller/channel.go`、`router/channel-router.go` |
| 分发中间件 | `middleware/distributor.go` |

→ 继续阅读：[06 · 计费与配额](06-billing-and-quota.md)
