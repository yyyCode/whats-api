# 10 · 前端架构：一个管理控制台该怎么搭

> 控制台是"表单 + 表格 + 图表 + 权限"的密集组合。本篇关注的是**那些能直接省掉你三天工作的结构决策**：状态怎么分层、表格怎么抽象、权限怎么驱动 UI、i18n 的 key 该怎么定。

## 1. 定位与构建

控制台是 React 19 SPA，**构建产物被 `go:embed` 进 Go 二进制**：

```go
//go:embed web/dist
var buildFS embed.FS
//go:embed web/dist/index.html
var indexPage []byte
```

生产环境由 Go 的 `router/web-router.go` 通过 `NoRoute` 兜底提供静态文件 + SPA fallback（并对 `/v1`、`/api`、`/assets` 主动 404）。开发环境由 Rsbuild dev server 代理 `/api`、`/v1`、`/mj`、`/pg` 到后端。

**"单二进制部署"是自托管项目的强需求**（用户只想 `docker run` 一个镜像），代价是前端构建必须进 Go 的构建流程。值得抄。

### 构建配置要点（`web/rsbuild.config.ts`）

```ts
pluginReact()
pluginTailwindcss({ optimize: false })
tanstackRouter({ target: 'react', autoCodeSplitting: isProd })
  // ★ 按路由代码分割「只在生产开启」：
  //   开发时保持单 chunk，避免改一个路由白屏刷新

splitChunks: vendor-react / vendor-ui-primitives / vendor-tanstack
  // 把更新频率低的依赖单独分包，提升缓存命中

dev server 代理 VITE_REACT_APP_SERVER_URL（默认 http://localhost:3000）
```

**工具链的特殊性（照抄前请注意）：**

| 项 | 本项目 | 常见默认 |
|---|---|---|
| 包管理 | **Bun** | npm/pnpm |
| 类型检查 | `tsgo`（TS native preview） | `tsc` |
| Lint / Format | `oxlint` / `oxfmt` | ESLint / Prettier |
| 构建 | Rsbuild | Vite |

**这些不是随便换的**（Rsbuild 更快、oxlint 快一个数量级），但如果你是团队协作或有既成规范，**先沿用 Vite + tsc + ESLint 更稳妥**，之后再按需替换。本篇后面的架构决策与这些工具无关。

## 2. 路由：文件式 + 三个路由组

用 TanStack Router 的**文件式路由**（`@tanstack/router-plugin` 生成 `routeTree.gen.ts`，且被签入仓库）。

```
routes/
├── __root.tsx                  根路由：全局 beforeLoad（升级重定向、setup 检查、认证 bootstrap）
│                               渲染 NavigationProgress + Outlet + Toaster + devtools
├── (auth)/                     无路径组：sign-in / sign-up / forgot-password / otp / oauth
├── (errors)/                   401 / 403 / 404 / 500 / 503
├── _authenticated/             ★ 认证守卫布局 + 所有受保护页面
│   ├── route.tsx               beforeLoad → resolveAuthentication()
│   │                           失败则 redirect({ to: '/sign-in', search: { redirect } })
│   └── dashboard/$section.tsx  等
├── index.tsx / about/ / pricing/ / setup/ 等公开页
```

### ★ `createSectionRegistry`：一个路由文件撑起一组子页面

这是最值得抄的一个模式。控制台里大量页面是"左侧一堆子菜单，右侧内容随子菜单切换"（系统设置、仪表盘、模型管理、用量日志…）。如果没有抽象，每个子域都要写十几个路由文件。

项目用**注册表**把它们压成一条路由：

```tsx
// routes/_authenticated/dashboard/$section.tsx
const DASHBOARD_SECTION_IDS = [...]           // 来自 section-registry.tsx
// 校验 $section 合法性，非法则 redirect 到默认 section
```

`createSectionRegistry`（`features/system-settings/utils/section-registry.ts`）从一份配置生成：

- section id 列表（用于路由参数校验）
- 默认 section
- 导航项（用于侧边栏）
- 每个 section 的 `adminOnly` 等元信息（渲染时过滤）

**收益：新增一个"设置页"= 加一条注册表配置 + 一个组件文件，不碰路由。** 代价是 URL 与组件不再是静态一一对应，调试时需要在注册表里多看一眼。

**适用判断：** 子页面数量多、结构同质（都是"一页表单/表格"）、需要统一导航 —— 满足就可以用。反之（每个页面结构差异大）就不要强上。

## 3. 状态分层：服务端状态与客户端状态

**这是前端架构里最容易做错的地方。** 项目分得很清楚：

| 类型 | 工具 | 放什么 |
|---|---|---|
| **服务端状态** | TanStack Query | 所有来自 API 的数据（列表、详情、状态） |
| **客户端状态** | Zustand | 认证会话、UI 偏好、通知已读状态、定价显示偏好 |

### Query Client 的全局策略（`web/src/lib/query-client.ts`）

```ts
queries: {
  staleTime: 10_000,             // 10 秒内不重新请求
  refetchOnWindowFocus: false,   // 控制台不需要焦点刷新
  retry: DEV ? 0 : 3,            // 开发时立即失败便于调试
  //      排除 401/403（认证错误不该重试）
}
MutationCache.onError → handleServerError(...)   // 统一错误提示（可 meta.errorToast:false 关闭）
QueryCache.onError    → handleServerError(...) + 500 → 跳 /500
```

**三处值得抄的细节：**

1. **`meta.errorToast?: boolean`**：通过模块增强给 query/mutation 加上"要不要弹错"的元数据。**默认全部弹错，个别需要自己处理的显式关闭。** 比在每个调用点写 `onError` 干净得多。
2. **错误去重**：`handleServerError` 用 `WeakSet` 按错误对象去重，避免同一个错误触发一串 toast（表格里 20 行同时失败会炸出 20 个提示）。
3. **401 重试策略**：拦截器里做一次 refresh + 重放，失败才登出。**注意排除了 401/403 的自动重试**，否则会 refresh 风暴。

### Query Key 的层级约定

```ts
// features/channels/lib/channel-actions.ts
export const channelsQueryKeys = {
  all: [...] as const,
  lists: () => [...channelsQueryKeys.all, 'list'] as const,
  list: (params) => [...channelsQueryKeys.lists(), params] as const,
  details: () => [...channelsQueryKeys.all, 'detail'] as const,
  detail: (id: number) => [...channelsQueryKeys.details(), id] as const,
}
```

**`all / lists / list(params) / details / detail(id)` 五层是通用范式。** 它的价值在于失效粒度：改一条渠道时 invalidate `lists()`（列表要刷新）但不 invalidate `details()`（详情不变）。**如果 key 是散落的字符串常量，你只能全量失效。**

### 会话变更要清空整个查询缓存

`auth-store.ts` 保存 `user / accessToken / accessExpiresAt / session / bootstrapState`，而 `__root.tsx` 订阅 auth store：**当 session id 变化时清空整个 query cache**。

**为什么必须做：** 切换账号（或会话被换掉）后，缓存里还留着上一个用户的渠道列表、用户列表。这是最难发现也最危险的脏数据泄漏。

## 4. API 客户端层

`web/src/lib/http-client.ts` 基于 axios，有几个非标准但很有用的设计：

```ts
export const api = axios.create({
  baseURL: '',                 // 同源；开发由 dev server 代理
  withCredentials: true,       // 带上会话 cookie
  headers: { 'Cache-Control': 'no-cache, no-store' },
})

// ★ GET 请求去重：同一 session + 同一 URL + 同一参数，只发一次
const inFlightGet = new Map<string, Promise<unknown>>()
const key = `${sessionSID}:${url}?${JSON.stringify(params)}`
if (inFlightGet.has(key)) return inFlightGet.get(key)
```

**GET 去重的实际价值：** 控制台首页/仪表盘常常多个组件同时请求同一个接口（状态、用户、配置）。没有去重就是 5 个重复请求。带 `sessionSID` 前缀是为了避免跨账号污染。

### 自定义 config 标志（模块增强）

```ts
declare module 'axios' {
  interface AxiosRequestConfig {
    skipBusinessError?: boolean   // 不把 success:false 当异常
    skipErrorHandler?: boolean    // 不弹全局错误
    disableDuplicate?: boolean    // 关闭本次去重
    skipAuthRefresh?: boolean     // 不做刷新重放
    authRetry?: boolean           // 内部标记：已重试过
    acceptAuthRotation?: boolean
    singleUseAuthorization?: boolean  // 一次性凭证，刷新后不重放
  }
}
```

**用 axios 的配置类型承载调用侧的语义开关**，比在拦截器里靠 URL 判断优雅得多。这是把"横切逻辑的开关"下沉到请求配置的标准做法。

### ★ i18n 的"消息 key"协议（前后端协作的关键设计）

后端返回的错误长这样：

```json
{
  "success": false,
  "message": "Insufficient quota",          // 英文兜底文本
  "message_key": "Insufficient quota",      // ★ 翻译 key
  "message_params": { "need": 500 }         // 插值参数
}
```

前端：

```ts
export function getServerMessage(value: unknown): string | undefined {
  if (typeof value.message_key === 'string' && value.message_key) {
    return i18next.t(value.message_key, value.message_params)
  }
  return messageText(value.message)   // 无 key 时直接显示英文
}
```

**为什么这个协议值得抄：**

- 后端**不需要**知道前端支持哪些语言（它只管产出稳定 key）；
- 新增语言只需前端加一份 locale 文件；
- 后端日志/API 响应始终是英文，便于检索与对接；
- `message_key` 缺省时自动降级为英文文本，**协议是渐进可用的**。

**注意项目规范里的约定：** Web 控制台的 API 消息用英文源文本作为 key，**每个 key 必须加到全部 7 个前端 locale 文件**；而"没有 Web 控制台介入"的文本（返回给 AI 客户端的错误、邮件、通知）走后端自己的 `i18n/`（en / zh-CN / zh-TW）。

**这条边界很重要：面向人的文本用前端翻译，面向程序的文本用后端翻译。** 混用会导致"改了一句后端文案，要改 7 个前端文件"。

还有 `translateServerText(t, text)`：处理**存储在数据库里的英文文本**（任务失败原因、分组描述），用 `{ skipInterpolation: true }` 避免上游文本里的 `{{var}}` 被误当模板插值。**用户/上游写入的文本永远要按纯文本处理，不能当模板。**

## 5. 组件组织

```
web/src/
├── components/
│   ├── ui/                  shadcn 基础原语（Base UI + Hugeicons，style: base-nova）
│   │                        button / dialog / select / table / sidebar / form / chart / markdown
│   ├── layout/              应用外壳：AppHeader / AppSidebar / AuthenticatedLayout / PublicLayout
│   ├── data-table/          ★ 共享表格抽象（有独立 README 与冻结的公共 API）
│   └── 根级共享业务组件      confirm-dialog / copy-button / empty-state / json-editor /
│                            model-group-selector / status-badge / permission-matrix …
├── features/<feature>/      业务模块
│   ├── components/          该功能专属组件（columns / dialogs / drawers）
│   ├── lib/                 schemas / transforms / query keys / form 定义
│   ├── hooks/  api.ts  types.ts  constants.ts
│   └── index.tsx            页面入口（组合）
├── stores/                  Zustand
├── lib/                     http-client / query-client / format / currency / admin-permissions
└── routes/                  TanStack Router
```

### 三层组件的边界

| 层 | 例 | 规则 |
|---|---|---|
| UI 原语 | `components/ui/button` | 只做样式与无障碍，不含业务 |
| 共享业务组件 | `confirm-dialog`、`copy-button`、`status-badge` | 跨功能复用，业务语义稳定 |
| 功能组件 | `features/channels/components/...` | 只服务一个功能，可组合上面两层 |

**项目规范里有一条规定值得直接搬：** 实现前必须先在 `components/` 里搜索，**"能复用/组合/扩展"就必须复用**，只有当"确实存在能力缺口"时才允许新建，并且要在 PR 里说明为什么现有组件不适用。"文案不同、尺寸不同、颜色不同、位置不同"都不算理由。

**为什么这条规则需要写进规范：** 控制台项目里"再写一个确认弹窗"是最常见的技术债来源。等到有 8 个确认弹窗时，改一次交互要改 8 个地方。

### `data-table`：控制台最值得投入的抽象

控制台 70% 的页面是"带筛选的表格"。项目把它抽成了独立子系统（`components/data-table/`，有 README 和冻结的 `index.ts` 公共 API）：

```
core/      TanStack Table 渲染、表头、单元格、分页、列固定
layout/    DataTablePage、桌面表格 + 移动端卡片列表、批量操作
toolbar/   筛选 / 搜索 / 列显示 / 视图模式切换
static/    内存数组的轻量表格
hooks/
```

**最值得抄的一点：URL 是表格状态的单一事实源。**

```ts
useTableUrlState()   // 把分页、全局筛选、列筛选绑定到 URL；页大小存 localStorage
```

带来的收益：

- 表格状态可以分享链接（"你看这个筛选条件下的日志"）；
- 刷新/前进后退不丢状态；
- 多个标签页各自独立。

**代价：** URL 变脏、需要序列化/反序列化、复杂筛选条件的编码方式要设计。**但对"可分享、可回退"有要求的控制台，这个代价是值得的。**

## 6. 表单：RHF + Zod，schema 与 payload 分离

```ts
// features/<f>/lib/<f>-form.ts
export const userSchema = z.object({ ... })         // 校验
export type UserForm = z.infer<typeof userSchema>   // 类型
export const userFormDefaults = (u?: User): UserForm => ({ ... })   // 回填默认值
export const transformUserToPayload = (v: UserForm) => ({ ... })    // 表单 → API payload
export const transformUserToFormDefaults = (u: User) => ({ ... })   // API → 表单
```

**四个东西放在同一个文件**：schema、推断类型、默认值转换、双向 payload 转换。服务端字段错误用 `form.setError` 回填。

**为什么要显式区分"表单模型"和"API payload"？** 因为表单里常有 UI 专用字段（比如"是否自定义超时"开关 + 超时值），而 API 期望的是最终值。**没有这层转换，你会在提交函数里写满 if。**

## 7. 权限驱动 UI

```ts
// web/src/lib/admin-permissions.ts
hasPermission(user, resource, action)   // 超管短路
normalizeAdminPermissions(...)          // scope ⇄ 矩阵 双向转换
```

- **权限目录只有一个来源**：`GET /api/authz/catalog`（后端注册表），前端不重复定义（见 07 篇 §7）；
- `permission-matrix.tsx` 是共享的权限矩阵编辑器；
- section 注册表支持 `adminOnly` 标记，**在渲染时过滤**。

**铁律：前端权限只影响"可发现性"，永远不是安全边界。** 每个接口都必须服务端校验。项目规范里把这条写得很硬（"Frontend checks MUST NOT substitute for server-side enforcement"）。

## 8. i18n 工具链

```ts
// web/src/i18n/config.ts
fallbackLng: 'en'
supportedLngs: ['en','zhCN','fr','ru','ja','vi','zhTW']   // ★ 接口语言码不是标准 BCP-47
nsSeparator: false     // 让 key 里可以出现冒号
detection: localStorage → navigator（缓存检测结果）
```

### 三条值得抄的设计

**① key 就是英文源文本。**

```ts
t('Insufficient quota')   // 而不是 t('errors.quota.insufficient')
```

收益：源码可读、不用为每个新文案想 key 名、缺失翻译时自动降级为英文。
代价：文案改动会让所有语言的翻译失效（需要 sync 脚本识别）。

**② 同步脚本兜住可行性。**

```bash
bun run i18n:sync
```

`scripts/sync-i18n.mjs`：以最完整的 locale 为基准重排所有文件，**检出缺失 / 多余 / 未翻译**，输出 `_extras/*.extras.json`、`_reports/*.untranslated.json`、`_sync-report.json`。

**③ 静态 key 注册表兜住动态 key。**

```ts
// web/src/i18n/static-keys.ts
export const STATIC_I18N_KEYS = [ ... ]   // 无法被 t('...') 正则扫描到的 key
```

因为脚本是用正则扫 `t('...')` 找 key 的，动态拼接的 key 扫不到（比如从后端拿到的 key）。**用一份显式清单兜住，是"扫描型工具"的标准补丁。**

### Intl locale 的强约束

```ts
// web/src/i18n/languages.ts
toIntlLocale()   // ★ 任何传给 Intl.* / toLocaleString 的语言码都要先过它
```

因为 `zhCN` / `zhTW` 是**非标准的接口语言码**，直接传给 `Intl` 会抛异常或行为异常。项目用 `oxlint` 自定义规则 `project/intl-locale` **设为 error 级别**来强制这条约束，并规定数字格式化走 `@/lib/format`、货币走 `@/lib/currency`。

**这条经验的通用价值：界面语言码与国际化库的 locale 码不是一回事，需要一个转换层 + lint 规则强制。** 否则会零散地炸出 `RangeError: Incorrect locale information provided`。

## 9. 自建路线

```
阶段 1：能跑
  文件式路由 + 认证守卫布局 + axios 实例 + TanStack Query + 一个共享 data-table

阶段 2：能规模化
  Query key 层级约定 + 统一错误处理（去重 + 可关闭）
  组件三层划分（ui / shared / feature）+ 复用纪律
  表单 schema/payload 转换约定

阶段 3：能运营
  i18n（key = 英文源文本 + sync 脚本 + 静态 key 注册表）
  权限驱动 UI（后端权限目录 + hasPermission）
  section registry（如果子页面真的很多）

阶段 4：体验
  URL 承载表格状态、会话变更清缓存、单二进制部署（go:embed）
```

## 10. 坑清单

| 坑 | 后果 | 对策 |
|---|---|---|
| 服务端状态塞进 Zustand | 手写缓存/失效/竞态，必然出 bug | 服务端状态一律走 TanStack Query |
| Query key 用散落字符串 | 只能全量失效，刷新过多 | `all/lists/list/details/detail` 层级约定 |
| 切换账号不清缓存 | 上一个用户的列表数据泄漏 | session 变化 → 清空 query cache |
| 同一接口并发重复请求 | 仪表盘打 5 倍请求 | GET 去重（带 session 前缀） |
| 错误提示不去重 | 表格 20 行失败弹 20 个 toast | 按错误对象去重 |
| 前端权限当成安全边界 | 直接调 API 就绕过 | 服务端是唯一边界，前端只为体验 |
| 界面语言码直接传 Intl | RangeError | 转换层 + lint 规则强制 |
| 把上游文本当 i18n 模板插值 | 用户文本里的 `{{}}` 被替换 | `skipInterpolation` |
| 开发环境按路由分 chunk | 改一行代码就白屏 | 只在生产开启 code splitting |

## 11. 本篇索引

| 关注点 | 文件 |
|---|---|
| 构建配置 | `web/rsbuild.config.ts`、`web/package.json`、`web/components.json` |
| 应用入口 | `web/src/main.tsx` |
| 路由 | `web/src/routes/**`、`web/src/routeTree.gen.ts`、`web/src/components/layout/` |
| section 注册表 | `web/src/features/system-settings/utils/section-registry.ts` |
| Query 配置 | `web/src/lib/query-client.ts`、`web/src/lib/status-query.ts` |
| HTTP 客户端 | `web/src/lib/http-client.ts`、`web/src/lib/api.ts` |
| 错误处理 | `web/src/lib/handle-server-error.ts`、`web/src/lib/server-error-message.ts` |
| 状态 | `web/src/stores/*`（auth / system-config / notification / pricing-preferences） |
| 表格 | `web/src/components/data-table/`、`web/src/hooks/use-table-url-state.ts` |
| 表单 | `web/src/features/*/lib/*-form.ts` |
| 权限 | `web/src/lib/admin-permissions.ts`、`web/src/components/permission-matrix.tsx` |
| i18n | `web/src/i18n/config.ts`、`languages.ts`、`static-keys.ts`、`web/scripts/sync-i18n.mjs` |
| 主题 | `web/src/context/`、`web/src/styles/`、`web/src/lib/use-chart-theme.ts` |
| 前端规范 | `web/AGENTS.md` |

→ 继续阅读：[11 · 从零自建路线图](11-blueprint.md)
