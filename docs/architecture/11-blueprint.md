# 11 · 从零自建：路线图、骨架与验收清单

> 前面十篇讲"它是什么"，本篇讲"你怎么做"。目标是把这套架构里**可复用的决策**提炼成一份施工图，并明确标出哪些东西**现在不要做**。

## 1. 开工前必须回答的五个问题

这五个答案会决定后面 80% 的技术选型。**不要边写代码边想。**

| 问题 | 影响 |
|---|---|
| **① 定位**：给团队自用 / 对外卖 / 只做协议转换层？ | 决定是否需要订阅、支付、多租户、审计、法务合规 |
| **② 协议**：下游只支持 OpenAI，还是要支持 Claude/Gemini？ | 决定要不要引入协议互转（复杂度分水岭） |
| **③ 计费模型**：免费 / 纯配额 / 配额+订阅 / 按次+按量混合？ | 决定计费层复杂度：最简单到最难差 10 倍 |
| **④ 部署形态**：单机 SQLite，还是要多实例 + 独立日志库？ | 决定要不要做分布式锁、缓存同步、策略传播 |
| **⑤ 扩展策略**：上游会持续增加吗？谁来加？ | 决定用 switch（自己发版）还是插件（运营热更新） |

**本项目的答案**（供参照）：AI 网关 + 多协议 + 配额/订阅/按量混合 + 单机到多实例全支持 + 上游持续增加（因此三层扩展方式并存）。

**如果你的答案是"团队自用 + 只 OpenAI 协议 + 纯配额 + 单机"**，那么你需要的东西大约是本项目的 15%：一个反向代理 + 一张 key 表 + 一个计数账本。**别照抄复杂度。** 本篇第 3 节的阶段划分就是为这种情况设计的。

## 2. MVP 的边界（明确不做什么）

**第一版要做的（M1-M2）：**

```
✅ 一张 user 表 + 一张 channel 表 + 一张 token 表
✅ POST /v1/chat/completions 透传到 OpenAI 兼容上游（含 SSE 流式）
✅ token 鉴权（查库即可）+ 模型白名单
✅ 一个按 token 计费的账本（预扣 → 结算）
✅ 一个最简单的管理界面（或先不做界面，用 SQL/脚本管理）
✅ 消费日志（能查"谁花了多少"）
```

**第一版明确不做的（做了就是浪费）：**

```
❌ 协议互转（RelayKit 那种）        ← 等真的需要第二种协议
❌ 表达式/分层定价                   ← 等定价策略真的装不下配置表
❌ JS 插件体系                       ← 等接供应商的速度真的成为瓶颈
❌ Casbin 细粒度权限                 ← 等真的有"子管理员"角色
❌ ClickHouse 日志库                 ← 等日志表真的拖慢了业务库
❌ 多 master + 分布式调度            ← 等真的要多实例
❌ WebAuthn / OAuth / 多租户         ← 等真的有企业客户
❌ 渠道亲和性、多 key 轮询            ← 等真的遇到缓存命中率/风控问题
```

**这张"不做清单"和"做清单"一样重要。** 本项目的复杂度是**业务长出来的**，不是一开始就设计成这样的。你直接照抄终态，会得到一个难以理解和维护的系统。

## 3. 分阶段路线图

每个阶段给：**目标 / 交付 / 验收标准 / 这一步最容易踩的坑**。

### M0 · 技术骨架（1 周）

**目标**：能跑起来一个空壳，但目录结构和依赖方向定下来。

**交付**
```
main.go              初始化顺序：env → config → db → cache → router → server → 优雅停机
router/              路由分组骨架（/v1 转发面 vs /api 管理面）
middleware/          recovery / request-id / logger / cors
model/               InitDB + AutoMigrate + 迁移只在 master 执行
setting/             配置注册表（struct + tag + Register）+ Option 表 + SyncOptions
common/              JSON 封装、数值安全（QuotaFromFloat 系列）、Redis 封装
controller/          一个 /api/status 探活接口
```

**验收标准**
- [ ] `docker run` 起来能用 SQLite 跑通，且不配任何环境变量
- [ ] 拿掉 Redis 也能启动（退化为内存缓存）
- [ ] 改一个配置项，不重启即生效，且多实例在 `SYNC_FREQUENCY` 后收敛
- [ ] 进程收到 SIGTERM 后能优雅停机

**坑**
- 配置表加载必须在任何读配置的模块之前（本项目是 `InitResources` 第 10 步）；
- 从第一天就把"配置放数据库 + 内存映射 + 定时同步"做好，别写死在 yaml 里（否则多实例时要重做）。

### M1 · 转发闭环（1-2 周）

**目标**：一次 OpenAI 兼容请求能端到端跑通，且**流式可用**。

**交付**
```
middleware/auth.go        token 鉴权（先直查 DB）
middleware/distributor.go 选渠道（先按优先级+权重随机）
relay/channel/adapter.go  Adaptor 接口（先只实现 OpenAI 一个）
relay/channel/openai/     Convert（透传）/ DoRequest / DoResponse（含 SSE）
relay/helper/stream_scanner.go  SSE 三 goroutine 转发
relay/common/relay_info.go      RequestState / AttemptState 分离
controller/relay.go       主循环 + 重试骨架
```

**验收标准**
- [ ] 流式响应逐 token 可见，且客户端断开时能立刻停止读上游
- [ ] 上游 5xx 时能切到另一个渠道重试（且**只在还没向客户端写过字节时**）
- [ ] 长响应有保活（不会被中间代理掐断）
- [ ] 单渠道故障时能自动禁用并在后续请求中跳过

**坑**
- 忘了保活 → 长流被 Nginx 静默断开；
- 重试时没判断 `SendResponseCount` → 客户端收到重复内容；
- 把 `lastError` 之类的状态写在请求对象上而不是每次尝试重置的对象上。

### M2 · 计费闭环（2 周，最需要仔细的一个阶段）

**目标**：钱算对了，且并发下不超支。

**交付**
```
common/quota_math.go      整数记账 + 三档数值转换（Strict / Checked / 基础）
model/quota_reserve.go    原子预扣（条件 UPDATE 起步）
service/billing_session.go  预扣 / 结算 / 退款 + 幂等状态机
service/text_quota.go     按倍率算钱 + 从 usage 结算
relay/request_billing.go  请求级预扣与退款挂载
model/log.go + 消费日志   含完整计费上下文（用了哪些倍率、走了哪条路径）
```

**验收标准**
- [ ] **并发 200 个请求打同一个余额只有 100 次请求额度的 token，成功数 ≤ 100，余额不为负**
- [ ] 上游失败时预扣全额退还
- [ ] 上游不给 usage 时**不按 0 计费**（退款 + 告警）
- [ ] 日志能完整复盘"这笔钱怎么算出来的"
- [ ] 一次请求只结算一次（构造重复结算路径测试）

**坑**
- 用 float 记账 → 长期累积误差；
- `SELECT` 判断再 `UPDATE` → 并发超支（**必须用条件 UPDATE 或 Redis Lua**）；
- 退款无幂等 → 重复退款，余额凭空增加；
- 零 usage 当成免费 → 用户从上游超时中白赚。

### M3 · 管理面（1-2 周）

**目标**：不登服务器就能运营。

**交付**
```
controller/channel.go     渠道 CRUD + 测试
controller/token.go       令牌 CRUD
controller/user.go        用户 CRUD + 登录
controller/log.go         日志查询与统计
web/                      前端骨架（路由 + 认证守卫 + data-table + 表单）
权限                     角色等级（user < admin < root），先不做细粒度矩阵
```

**必做**：渠道的 `ModelMapping` / `ParamOverride` / `HeaderOverride` 三个字段。**有了它们，大量"新供应商"不需要改代码。**

**验收标准**
- [ ] 在界面上加一个渠道、跑一次测试、建一个 key、看到消耗日志
- [ ] 渠道变更后**不需要重启**即生效（缓存增量更新）
- [ ] 界面上的权限与后端一致（前端隐藏 ≠ 后端放行）

### M4 · 路由精细化（1-2 周）

**目标**：多渠道多分组下"选得对、挂得起"。

**交付**
```
model/ability.go          反范式 (group, model, channel) 索引表 + 一键重建
model/channel_cache.go    内存索引 + 定时同步 + 失效
service/channel_select.go 优先级分层 + 层内加权随机 + 平滑因子
service/channel.go        自动禁用/恢复（含"只恢复自动禁用的"）
service/relay_error.go    重试决策（可重试错误 vs SkipRetry）
多 key                    轮询/随机 + 单 key 级健康状态
```

**验收标准**
- [ ] 同优先级多渠道按权重分布（统计 1000 次请求的分布）
- [ ] 权重全为 0 时不 panic，仍能正常分发
- [ ] 重试自动降级到下一优先级；档位用完停在最差档而不是报错
- [ ] 手动禁用的渠道不会被自动恢复
- [ ] 模型映射配成环时不死循环

### M5 · 运维与可观测（1 周）

**目标**：出问题能看见。

**交付**
```
系统任务框架（DB 租约 + 心跳 + 手动触发 + 运行历史）
  → 渠道定时测试、上游模型列表同步
Prometheus 指标 + pprof
审计日志（★ 脱敏：不记录密码/验证码/token）
健康检查接口（供容器编排）
```

**验收标准**
- [ ] 两实例同时运行，定时任务只在一个实例上执行
- [ ] 杀掉持有租约的实例，另一实例能在租约过期后接管
- [ ] 审计日志里**搜不到**任何密钥、密码、验证码

### M6 · 按需扩展（不做时间估计，按真实需求触发）

每一项都是**独立可选项**，做之前先确认触发条件：

| 扩展 | 触发条件 | 对应文档 |
|---|---|---|
| 协议互转（RelayKit 模式） | 下游开始要求非 OpenAI 协议 | 04 篇 |
| 表达式 / 分层定价 | 定价策略装不进配置表（分档、时段、按 header 差异） | 06 篇 §6 |
| 订阅 + 钱包双资金源 | 有预付费套餐业务 | 06 篇 §3 |
| JS 插件体系 | 新增上游的速度成为瓶颈且需要运营自助 | 09 篇 §4 |
| 细粒度权限矩阵 | 出现"能管渠道不能管用户"的角色 | 07 篇 §7 |
| 多实例 + ClickHouse | 单库扛不住日志量 / 需要水平扩展 | 08 篇 §2 |
| WebAuthn / OAuth | 企业客户要求 SSO 或更强制 MFA | 07 篇 §3-4 |
| 渠道亲和性 | 上游缓存命中率或风控成为问题 | 05 篇 §4 |

## 4. 核心数据模型（最小 DDL 骨架）

```sql
-- 用户
users(id, username UNIQUE, password_hash, role, status, quota, used_quota,
      group_name, email, auth_version, created_at)

-- 上游渠道（运营单位）
channels(id, type, key, base_url, models, group_name, priority, weight, tag,
         status, model_mapping, param_override, header_override, setting, ...)

-- ★ 选路索引（反范式）：一行 = (分组, 模型, 渠道)
abilities(group_name, model_name, channel_id, enabled, priority, weight, tag,
          PRIMARY KEY(group_name, model_name, channel_id))

-- 转发 API Key（权限 + 限额）
tokens(id, user_id, key UNIQUE, name, status, expired_time, remain_quota,
       unlimited_quota, model_limits_enabled, model_limits, allow_ips,
       used_quota, group_name, created_at, accessed_at)

-- 账本（审计凭证，只追加）
logs(id, user_id, type, content, model_name, quota, prompt_tokens,
     completion_tokens, use_time, is_stream, channel_id, group_name,
     token_name, ip, request_id, other, created_at)

-- 运行时配置
options(key PRIMARY KEY, value)

-- 后台任务与租约（多实例）
system_tasks(id, task_id, type, status, active_key UNIQUE, payload, state, result,
             locked_by, created_at, updated_at)
system_task_locks(type PRIMARY KEY, locked_by, locked_until)
```

**三条约束（会救你很多次）：**

1. **金额只用整数**（`quota`），永不 float；
2. **持久化枚举只增不改**（`channels.type`、`logs.type` 都是 ABI）；
3. **`abilities` 表必须配一个"整表重建"函数**（索引漂移的兜底）。

## 5. 目录结构建议

照抄这个形状（依赖方向自上而下，同层之间不互相依赖）：

```
main.go                 入口：初始化 → 后台任务 → HTTP → 优雅停机
router/                 路由注册（按面拆文件）
middleware/             认证 / 限流 / 选渠道 / 过载保护 / 请求体清理
controller/             HTTP handler（转发面 + 管理面）
service/                业务逻辑（选渠道、计费、任务、订阅）★ 无 HTTP 依赖
relay/                  转发内核
  ├── channel/<provider>/   供应商适配器（每个一个目录）
  ├── channel/adapter.go    ★ Adaptor 接口
  ├── common/               ★ 请求上下文（RequestState / AttemptState）
  └── helper/               SSE 扫描、模型映射、价格计算
model/                  GORM 模型 + 缓存 + 迁移
setting/                配置定义与注册（按变更人分域）
common/                 基础设施（JSON、数值安全、Redis、加密）
constant/ types/ dto/   常量、共享类型、传输对象
pkg/<lib>/              可独立的内置库（计费表达式等）
web/                    前端（构建产物 embed 进二进制）
```

**关于"再拆一个独立 module"的判断标准**（如 `relaykit` / `tokenkit`）：
**不读 DB、不发网络、不读全局配置、不依赖时间/随机数** → 可以独立成 module。

## 6. 十条核心设计思想（提炼）

如果在整个项目里只能带走十句话：

1. **协议转换是纯函数，应该独立成模块；计费有副作用，必须留在宿主。**
2. **转换永远成功，丢掉了什么如实报告（Diagnostics）；只在可逆的地方（请求前）允许失败。**
3. **请求上下文必须区分"跨尝试共享"与"每次尝试重置"——这是重试安全的全部秘密。**
4. **不要缓存"选中结果"，只缓存"候选集"；"命中缓存"和"负载均衡"是两件事。**
5. **把查询的结果预先存下来（反范式索引表），而不是优化查询本身。**
6. **金额只用整数；数值溢出的三档语义（Strict/Checked/基础）；异常必须留痕。**
7. **并发安全的本质是"原子的检查并扣减"（条件 UPDATE / Lua），不是加锁的先后顺序。**
8. **跨时间的业务必须存计价快照；跨时间的计费不能按当前配置算。**
9. **配置系统用"结构体 + tag + 注册"生成 schema/默认值/key/文档，避免五份定义漂移；关联配置用不可变快照原子替换。**
10. **分布式锁不只要能加，还要能在失去锁时停止工作（心跳 + 失锁取消 ctx）。**

另有三条关于"什么时候不做"：

- **稳定扩展点用 switch（编译期检查、可读），只有需要热更新的扩展点才上插件系统。**
- **先穷尽配置（映射/覆盖/选项），再考虑写代码。**
- **本项目的复杂度是业务长出来的，照抄终态会得到一个你无法维护的系统。**

## 7. 反模式清单（明确不要做）

| 反模式 | 后果 |
|---|---|
| 适配器里直接算钱 | 改定价要动 N 个供应商实现 |
| 适配器里读 DB / 全局配置 | 无法单测，并发隐患 |
| 用一个 token 类型覆盖会话/API Key/PAT | 要么性能不够，要么安全不够 |
| JWT claims 里放角色权限 | 权限变更不生效 |
| 审计日志 dump 整个请求 | 密码/密钥进日志 |
| 计费预扣放在响应阶段 | 上游已经花钱了，扣款失败白送 |
| 日志与业务同库，且不支持独立配置 | 日志量压垮业务库 |
| 前端权限当安全边界 | 直接调 API 就绕过 |
| 把"全部 400+ 状态码"都设成可重试 | 重试放大故障 |
| 微服务化（把 relay/model/billing 拆成独立服务） | 这个规模下只会增加延迟和运维成本 |

最后一条特别说明：**本项目是一个模块化单体（modular monolith）**，不是微服务。所有模块在一个进程内，通过接口和包依赖方向解耦。对这个量级它足够快，而且省掉了服务间调用的延迟与一致性成本。**别在还不需要的时候拆服务。**

## 8. 测试与验证策略

按"会出什么事故"排序：

```
① 账务不变量（最高优先级）
   并发预扣不超支 / 只结算一次 / 只退款一次 / 日志金额==结算金额

② 协议转换
   golden 快照测试（整 DTO 比对，不逐字段断言）
   覆盖：请求 / 非流式响应 / 流式事件 / 工具调用 / 多模态 / 推理参数

③ 路由选择
   分布统计（权重比例）、退化输入（全 0 权重、单渠道）、映射环、档位耗尽

④ 多数据库（如果支持多种）
   真实 SQLite + MySQL + PostgreSQL 跑同一条路径
   新库初始化 + 从上一版本升级 + 连续启动三次（幂等）

⑤ 安全
   越权访问、凭证重放、CSRF、账号枚举、审计日志脱敏
```

**关于测试的一条纪律（本项目写进了规范）：** 不要为一个小改动在每一层都加测试文件，不要写只提升覆盖率数字的测试，不要写随机输入/大循环/计时的"伪压力测试"。**测试要保护真实契约，不是保护行数。**

## 9. 上线前验收清单

```
□ 并发压力下余额从未为负
□ 上游全部挂掉时，请求快速失败并明确报错（不挂起）
□ 上游返回 5xx 能自动切换渠道
□ 客户端中途断开时，上游请求被及时取消（不继续烧 token）
□ 流式响应有保活，长响应不被中间代理掐断
□ 长连接（WebSocket，如果支持）期间断网能正确结算
□ 重启后：配置不丢、缓存能重建、在途任务能续跑
□ 优雅停机时在途 SSE 有时间跑完
□ 审计日志中不含任何密钥/密码/验证码
□ 消费日志能完整复盘任意一笔费用的计算过程
□ 多实例下定时任务只执行一次
□ 数据库迁移幂等（连续启动三次无副作用）
□ 任何"手动操作"（禁用渠道、改价格）都不会被自动化系统撤销
```

## 10. 阅读索引

| 主题 | 文档 |
|---|---|
| 全景、模块边界、启动时序 | [01-overview.md](01-overview.md) |
| 请求链路、中间件顺序、路由分层 | [02-request-lifecycle.md](02-request-lifecycle.md) |
| 适配器、请求上下文、重试、SSE | [03-relay-core.md](03-relay-core.md) |
| 协议互转、有损转换策略 | [04-protocol-conversion.md](04-protocol-conversion.md) |
| 渠道、反范式索引、负载均衡、健康管理 | [05-routing-and-channels.md](05-routing-and-channels.md) |
| 预扣/结算/退款、表达式定价、并发安全 | [06-billing-and-quota.md](06-billing-and-quota.md) |
| 三类身份、Casbin、会话设计 | [07-auth-and-access-control.md](07-auth-and-access-control.md) |
| 数据模型、三库兼容、配置系统、缓存 | [08-data-model-and-settings.md](08-data-model-and-settings.md) |
| 系统任务、异步任务、JS 插件 | [09-async-tasks-and-plugins.md](09-async-tasks-and-plugins.md) |
| 控制台架构、表格抽象、i18n | [10-frontend-architecture.md](10-frontend-architecture.md) |
