# 07 · 认证与访问控制

> 一个网关有**三类完全不同的身份**，用同一套机制处理是灾难的开始。本篇讲清楚本项目怎么把它们分开，以及授权（谁能做什么）与认证（你是谁）如何解耦。

## 1. 三类身份

| 身份 | 谁在用 | 凭证形态 | 存在哪 |
|---|---|---|---|
| **控制台用户** | 人（浏览器） | 短期 JWT + 服务端 Session | `user_sessions` |
| **转发 API Key** | 程序（SDK/客户端） | `sk-...` 长期密钥 | `tokens` |
| **个人访问令牌（PAT）** | 程序/脚本（访问控制台 API） | `nap_...` 前缀，只存哈希 | `user_access_tokens` |

**为什么必须分开？**

- 控制台会话的威胁模型是**浏览器**（XSS、CSRF、会话劫持）→ 需要短有效期、HttpOnly cookie、Origin 校验、轮换；
- 转发 API Key 的威胁模型是**长期凭证泄露**（被写进代码仓库、被日志打印）→ 需要可吊销、可限额、可限模型、可限 IP，且必须支持超高频校验（每个请求一次）；
- PAT 的威胁模型是**自动化脚本**（CI、定时任务）→ 需要细粒度 scope 和过期时间，但不需要浏览器的那些防护。

**最常见的设计错误是用一个 token 类型覆盖全部三种场景**，结果要么性能不够（每次请求都查 session 表），要么安全性不够（长期 key 带上了会话权限）。

## 2. 控制台认证：JWT 套在服务端 Session 之外

这是本项目一个很值得学的组合设计。

### 双令牌结构

```
access token  = HS256 JWT，15 分钟有效（AccessTokenTTL）
  claims: sid（会话 ID）、uv（user auth version）、sv（session version）
  ↑ 签名密钥由 common.SessionSecret 派生

refresh token = 不透明的随机串，只把 HMAC 后存进 DB
  ↑ 明文只在客户端，DB 里永远没有明文
```

为什么是这种组合，而不是二选一：

| 方案 | 问题 |
|---|---|
| 纯 JWT | **无法吊销**。用户改密码、管理员封号、登出，JWT 依然有效直到过期 |
| 纯服务端 Session | 每个请求一次 DB/Redis 查询，控制台请求虽不多但有大量并发页面加载 |
| **JWT + Session 表（本项目）** | JWT 承担高频校验（无状态、免查库），Session 表承担"控制平面"（可吊销、可轮换、可审计） |

**关键在于 claims 里放的是 `sid` 而不是用户权限。** 代码注释明确写了：`AuthIdentity = model.AuthSessionIdentity` —— **角色、状态、分组永远从用户缓存读取，绝不信任 JWT claims。** 这是为了避免"签发后权限变更不生效"的经典漏洞。

### Session 表的字段设计（`model/user_session.go:42`）

```go
type UserSession struct {
	SID       string    // 主键：会话 ID（JWT 里也带这个）
	Version   int       // 会话版本 → 参与 sv claim
	UserAuthVersion int // 用户认证版本 → 参与 uv claim（改密码时递增，踢掉所有会话）
	Status    string    // active / revoking / revoked
	RefreshHash         string  // ★ HMAC 后的 refresh token，绝不存明文
	PreviousRefreshHash string  // ★ 上一次的 refresh hash
	PreviousValidUntil  int64   // ★ 上一次的过期时刻（轮换宽限窗口）
	LoginMethod, IP, UserAgent string
	LastActiveAt, ExpiresAt, RevokedAt int64
	RevokedReason string
}
```

三个细节：

1. **`RefreshHash` 用 HMAC 而非明文** —— 数据库泄露不等于会话可被劫持。这和密码哈希是同一个道理，但很多项目会漏掉 refresh token。
2. **`PreviousRefreshHash` + `PreviousValidUntil`** —— 解决"refresh 轮换时的并发重放"。客户端可能因为网络重试把同一个 refresh token 发两次；如果服务端严格一次性，第二次会被误判为攻击。保留一个短窗口的"上一个 hash"可以容忍这种重试，同时真正的重放（超出窗口）仍会被拒。
3. **两个版本号**：`UserAuthVersion`（用户级：改密码 → 全部会话失效）和 `Session.Version`（会话级：单会话轮换）。**失效粒度要分级** —— 用户要能"退出所有设备"，也要能"只退出当前设备"，还需要系统能"因为可疑活动踢掉某一个会话"。

### 相关配置（从 compose 里可见的运维旋钮）

```
USER_SESSION_ACTIVE_LIMIT              同时活跃会话数上限
USER_SESSION_ISSUANCE_LIMIT            发号速率上限
USER_SESSION_ISSUANCE_WINDOW_SECONDS   发号窗口
USER_SESSION_REVOKED_RETENTION_DAYS    已吊销会话的保留天数（用于审计）
USER_SESSION_HOURLY_ALERT_THRESHOLD    小时级告警阈值（★ 只告警，不做全局拒绝）
```

最后一条特别值得注意：**阈值设计成"告警"而不是"硬拒绝"**。因为全局拒绝会造成大范围不可用（可能是攻击，也可能是正常的集中登录），而告警让人来判断。**安全控制里的失败模式选择（fail-open vs fail-closed）要按影响面权衡。**

### `ParseDashboardAccessToken` 的一个安全细节

```go
// 能区分「本系统签发的 JWT」和「任意不透明凭证」，
// 这样格式错误的控制台 JWT 永远不会"顺延"到 PAT / relay key 的校验分支
```

**不要用同一个解析函数依次尝试多种凭证类型** —— 那会让 A 类型的畸形输入意外通过 B 类型的校验。**先分类，再校验。**

## 3. 二次因子与敏感操作

| 机制 | 实现位置 | 要点 |
|---|---|---|
| TOTP | `model/twofa.go`（`Secret`、`IsEnabled`、`FailedAttempts`、`LockedUntil`）、`common/totp.go` | 带失败计数与锁定；有备用码表 `TwoFABackupCode` |
| WebAuthn / Passkey | `model/passkey.go`（credential id、公钥、sign count、AAGUID、transports）、`service/passkey/` | **sign count 必须校验并递增**（防凭证克隆） |
| 安全验证（step-up） | `service/auth_token.go:IssueSecurityProof` | 1 分钟有效的"操作证明"，用于删账号/改密码等高危操作 |
| 登录挑战流程 | `model/auth_flow.go:45` `AuthFlow` | 一次性 ceremony state（防 CSRF / 防重放） |

**`AuthFlow` / 一次性 state 的设计是 OAuth 与 WebAuthn 的共同需求**：流程跨越多次 HTTP 往返，必须有一个服务端记录的一次性状态把它串起来，并在完成后立刻作废。

**"敏感操作要重新认证"（re-authentication / step-up）是 OWASP ASVS 的明确要求**，项目用 1 分钟的 `SecurityProof` 实现。自建时至少要覆盖：改密码、改邮箱、绑/解绑 OAuth、删除账号、生成 PAT。

## 4. OAuth / OIDC

结构（`oauth/`）：

```
oauth/provider.go      统一 Provider 接口
oauth/registry.go      注册表
oauth/github.go  linuxdo.go  telegram.go  discord.go  oidc.go  generic.go
model/custom_oauth_provider.go   管理员可在控制台添加自定义 OIDC 提供方
model/user_oauth_binding.go      账号 ↔ 外部身份绑定
model/external_identity_claim.go 外部身份声明的记录
oauth.LoadCustomProviders()      启动时从 DB 装载（非致命）
```

**值得学的两点：**

1. **内置 provider 与"通用 OIDC"并存，且支持运行时可添加自定义 provider。** 内置的用于常见平台（GitHub、Discord 等，它们各自的 API 差异大），通用的 `oidc.go` 覆盖标准 OIDC（企业 SSO），自定义的覆盖长尾。**这是"少数硬编码 + 通用协议 + 运行时可扩展"的三层策略**，比"全部做成插件"务实得多。
2. **账号绑定单独建模**（`UserOAuthBinding`），而不是在 user 表上堆 `GitHubId`/`OidcId`/`WeChatId` 一堆列。虽然本项目 user 表里确实也有这些历史列，但新方向是独立绑定表 —— **一个用户可以绑定多个外部身份，这在一对多关系上是必需的。**

**安全要求（项目 AGENTS.md 明确写入的 OWASP 约束，值得直接抄进你的规范）：**

- 服务端强制所有安全控制，前端校验不能替代；
- 防账号枚举（登录/注册/找回密码的响应不能泄露账号是否存在）；
- 防暴力破解（限流 + 失败计数 + 锁定）；
- CSRF 与重放保护；
- token / challenge 的过期与一次性；
- 协议级校验（OIDC 要验 state、nonce、签名、aud/iss）；
- 会话轮换与失效；
- **审计事件不得记录密码、验证码、恢复码、私钥、可用的会话或认证 token**，但要记录足够的非敏感上下文。

最后一条在实现时极易违反（把整个请求对象 dump 进审计日志是最省事的做法）。

## 5. 转发 API Key：`Token` 模型

```go
// model/token.go:14
type Token struct {
	Id          int
	UserId      int       `gorm:"index"`            // ← 映射到用户
	Key         string    `gorm:"varchar(128);uniqueIndex"`
	Status      int       `gorm:"default:1"`        // 启用/禁用
	Name        string
	CreatedTime, AccessedTime int64
	ExpiredTime int64     `gorm:"default:-1"`       // -1 = 永不过期
	RemainQuota int
	UnlimitedQuota     bool
	ModelLimitsEnabled bool
	ModelLimits        string   // 逗号分隔的允许模型白名单
	AllowIps           *string  // 换行分隔的 IP / CIDR 列表
	UsedQuota          int
	Group              string   // 允许使用的分组
	CrossGroupRetry    bool     // 仅 auto 分组
	AutoGroups         string   // auto 分组的候选列表（JSON）
	DeletedAt          gorm.DeletedAt
}
```

**这是一张"权限+限额"表**：有效期限、余额上限、模型白名单、IP 白名单、可用分组。

### 认证流程（`middleware/auth.go:514` `TokenAuth`）

```
1. 归一化凭证来源
   Authorization: Bearer sk-xxx
   x-api-key（Anthropic 风格）
   x-goog-api-key / ?key=（Gemini 风格）
   mj-api-secret
   WebSocket 子协议（浏览器无法自定义 header）
2. model.ValidateUserToken(key)
   → 缓存查（Redis，键是 key 的 HMAC）
   → 校验 状态 / 过期时间 / 剩余额度
3. 校验 IP：token.GetIpLimits() + IsIpInCIDRList
4. model.GetUserCache(token.UserId)
   → 用户被封禁？分组是否允许？
5. SetupContextForToken：写入 gin.Context
   → 用户 id、token id/key/name、额度、模型限制、分组、
     跨组重试、auto groups、以及（管理员限定）渠道 pin
```

### ★ Token 缓存的"栅栏"设计（很有价值的一个细节）

Token 校验在每个转发请求上都要做，必须走缓存。但缓存和"原子预扣"会打架：

- 预扣直接改 Redis 里的余额（比 DB 更快）；
- 如果此时缓存被某个管理操作（改 token）重建成 DB 里的旧值，**预扣就被回滚了 → 用户白嫖**。

项目的解法是 `model/token_cache.go` 里的**栅栏（fence）**：

```
token:<HMAC(key)>        值缓存
token:fence:<HMAC(key)>  ★ 栅栏标记
```

- `cacheInitToken` 用 **Lua 脚本**做"仅当缓存不存在时才初始化"，避免覆盖掉正在进行中的预扣；
- 管理变更（改 token）时**先立栅栏**，再写 DB，**且不主动删除栅栏**（靠 10 秒自然过期）—— 这样在栅栏有效期内，旧的缓存不会被重新发布；
- 缓存里的值带有版本（`CacheSchema`），解码旧版本时自动失效。

**通用教训：缓存 + 原子计数器同时存在时，"过期失效"是不够的，你需要一个"禁止回填"的窗口。** 任何有热路径写入的缓存都会遇到这个问题。

## 6. PAT（个人访问令牌）

`model/user_access_token.go:41`：

- 前缀 `nap_`，便于识别与泄密扫描；
- **只存 SHA-256 哈希**（DB 泄露不可用）；
- 带 scope（不是全权限）与 `ExpiresAt` / `LastUsedAt`；
- 路由级限制：`middleware/access_token_routes.go`（`enforceAccessTokenRoute`）限制 PAT 能访问哪些接口。

**PAT 与 relay key 的核心区别不是格式，而是"用途域"**：PAT 访问的是管理面（`/api`），relay key 访问的是转发面（`/v1`）。两者的限流、审计、scope 语义完全不同，所以路由层必须能区分它们（见 §2 的"先分类再校验"）。

## 7. 授权：Casbin 资源-动作模型

### 模型

```
[request_definition]   r = sub, obj, act
[policy_definition]    p = sub, obj, act, eft
[matchers]             r.sub == p.sub && r.obj == p.obj && r.act == p.act && p.eft == "allow"
```

**注意 matcher 是精确匹配**（不做通配/正则）。这意味着权限矩阵是显式枚举的 —— 对管理后台这种权限点有限、需要可审计的场景，显式枚举比模式匹配更安全（不会因为一条通配规则意外放权）。

### 结构

| 组件 | 文件 | 说明 |
|---|---|---|
| Enforcer | `service/authz/enforcer.go` | `casbin.SyncedEnforcer`，策略持久化在 `casbin_rule` 表 |
| 策略同步 | `authz.StartPolicySync(frequency)` | 每 `SyncFrequency` 秒从 DB 重载 → **多节点权限变更自动收敛** |
| 角色 | `service/authz/role.go` | 内置 `BuiltInRoleRoot` / `BuiltInRoleAdmin`；`Superuser` 短路放行 |
| 资源/动作注册表 | `service/authz/registry.go` + `resources_channel.go` / `resources_task_plugin.go` / `resources_audit.go` | 代码里声明资源与动作，避免前后端各写一份 |
| 主体 | `service/authz/permission.go` | `user:<id>` / `role:<key>` |
| 判定 | `service/authz/resolver.go` | `Can(userID, systemRole, permission)`：超管短路 → 用户级覆盖（**deny 优先**）→ 角色基线并集 |
| 用户级覆盖 | `service/authz/override.go` | 单个用户的额外授予/拒绝 |

### 中间件挂载点

```go
middleware.UserAuth / AdminAuth / RootAuth          // 粗粒度：角色等级
middleware.RequirePermission(authz.Permission)      // 细粒度：资源+动作
middleware.TokenOrUserAuth                          // 两者皆可
```

**两级授权是必要的：** 角色等级（user < admin < root）处理 90% 的场景，成本极低；资源-动作矩阵处理剩余 10%（比如"能管渠道但不能管用户"的子管理员）。只用 Casbin 会让简单场景变重，只用角色会让权限无法细分。

### 前端一致性

前端从 `GET /api/authz/catalog` 拉取权限目录（**不重复定义**），用 `hasPermission(user, resource, action)` 决定 UI 显隐，用 `permission-matrix.tsx` 编辑。**权限目录只有一个来源（Go 侧的注册表），前端只消费** —— 否则前后端权限定义必然漂移，出现"界面能点但接口拒绝"或更糟的"界面隐藏但接口放行"。

## 8. 自建路线

```
阶段 1：能用
  - user 表 + bcrypt/argon2 密码 + 服务端 session（先不做 JWT 优化）
  - relay API Key 表（含 限额/模型白名单/过期时间）+ 每请求校验（先直查 DB）
  - 角色枚举 user < admin < root

阶段 2：能扛
  - 会话换 JWT + 版本号（uv/sv），支持"踢所有会话"
  - API Key 加缓存（Redis）+ 栅栏防回填
  - IP 白名单、模型白名单

阶段 3：能运营
  - 资源-动作权限矩阵（Casbin 或自研）+ 前端目录 API
  - 审计日志（★ 脱敏）
  - 多节点策略同步

阶段 4：企业特性
  - TOTP → Passkey → OAuth/OIDC
  - PAT + scope
  - 敏感操作 step-up 重新认证
```

## 9. 坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| JWT claims 里带角色/权限 | 权限变更后仍生效到过期 | claims 只放 ID，权限每次查缓存 |
| refresh token 明文入库 | 库泄露 = 会话被劫持 | HMAC 存储 |
| 用同一个函数解析所有凭证类型 | 畸形输入意外通过别的校验分支 | 先分类，再分别校验 |
| 审计日志 dump 整个请求 | 密码/密钥/验证码进日志 | 白名单字段 + 显式脱敏 |
| token 缓存没有防回填窗口 | 预扣被旧缓存覆盖 → 白嫖 | 栅栏/版本号 |
| 权限目录前后端各写一份 | 界面与接口权限不一致 | 后端单一来源 + 目录 API |
| 服务端不校验只有前端校验 | 直接调 API 就绕过 | 前端仅为体验，服务端是唯一防线 |
| 登录/注册响应区分"账号不存在" | 账号枚举 | 统一响应文案与耗时 |
| 限流阈值直接硬拒绝 | 误伤正常流量导致大范围不可用 | 高危阈值先告警，观察后再收紧 |

## 10. 本篇索引

| 关注点 | 文件 |
|---|---|
| 令牌签发/校验 | `service/auth_token.go`、`service/auth_session.go` |
| 会话模型 | `model/user_session.go`、`controller/auth_session.go` |
| 登录 | `controller/user.go`（`Login`、`setupLogin`、`writeLoginResponse`） |
| Token（API Key） | `model/token.go`、`model/token_cache.go`、`middleware/auth.go`（`TokenAuth`、`SetupContextForToken`） |
| PAT | `model/user_access_token.go`、`middleware/access_token_routes.go` |
| 2FA / Passkey | `model/twofa.go`、`common/totp.go`、`model/passkey.go`、`service/passkey/` |
| OAuth/OIDC | `oauth/*`、`model/custom_oauth_provider.go`、`model/user_oauth_binding.go`、`model/auth_flow.go` |
| 授权 | `service/authz/*`（`enforcer.go`、`registry.go`、`resolver.go`、`role.go`、`override.go`）、`model/casbin_rule.go` |
| 前端权限 | `web/src/lib/admin-permissions.ts`、`web/src/components/permission-matrix.tsx` |

→ 继续阅读：[08 · 数据模型与配置系统](08-data-model-and-settings.md)
