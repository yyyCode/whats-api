# whats-api

AI 网关与资产管理平台。**当前里程碑：M0（技术骨架）——只有骨架，没有转发功能。**

## 这个仓库里有什么

- `docs/architecture/` —— 对 New API 源码的架构分析（12 篇）。它不是本项目的设计，
  而是**参考材料与决策来源**；本文档多处引用它（如 `11-blueprint.md` 的分阶段路线图）。
- `docs/superpowers/specs/` —— 本项目的设计文档。当前是 M0 的设计。
- `docs/superpowers/plans/` —— 实施计划（含每步的完整代码）。
- `internal/` —— 本项目的代码。

## M0 做了什么、没做什么

**做了**：目录结构与依赖方向、三库兼容的数据层（SQLite / MySQL / PostgreSQL）、
七张核心表与幂等迁移、反射驱动的配置系统（可热更新 + 多实例定时收敛）、
结构化日志、Redis 可降级缓存、请求级中间件、`/api/status` 探活、优雅停机。

**明确没做**（都在后续里程碑）：任何转发端点与协议转换（M1）、计费与预扣（M2）、
管理面 CRUD 与前端（M3）、渠道选路与负载均衡（M4）、
Prometheus 指标与系统任务框架（M5）。
没有管理面写接口，所以改配置要用 `cmd/optctl`（见下）。

## 快速开始

### 本地运行（不需要任何配置）

```bash
go run .
# 或者
go build -o whats-api . && ./whats-api
```

不配任何环境变量就能启动：数据库落在 `./whats-api.db`，缓存是进程内的。
打开另一个终端验证：

```bash
curl -s http://127.0.0.1:3000/api/status
```

### Docker

```bash
docker compose up --build
```

数据卷 `whats-api-data` 挂在 `/data`，容器重建不丢库。

## 目录结构

```
main.go                     入口：信号 → ctx → app.Run(ctx)
internal/app/               启动时序、HTTP 生命周期、优雅停机
internal/router/            路由三层注册（/api 管理面、/v1 转发面、NoRoute 兜底）
internal/middleware/        recovery / request-id / version / route-tag / 访问日志 / CORS
internal/controller/        HTTP handler 与响应信封
internal/model/             GORM 模型、三库兼容连接、迁移、配置持久化
internal/setting/           配置注册表与配置域（反射驱动 + 原子快照）
internal/cachex/            Redis 与进程内自动切换的缓存原语
internal/common/            基础设施：环境变量、JSON、数值安全、Redis 封装
internal/logger/            log/slog 封装（级别可热更新）
internal/constant/          持久化枚举（ABI，只增不改）
cmd/optctl/                 配置读写工具
scripts/m0-acceptance.sh    M0 验收脚本
```

依赖方向自上而下，同层不互相依赖：

```
app → router → controller → { middleware, model } → { setting, cachex } → { common, constant }

logger 是横切工具：只依赖标准库，任何层可用（含 common）
```

## 环境变量

全部变量都有默认值——不配任何变量也能起来。完整清单见 [`.env.example`](.env.example)。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `3000` | HTTP 监听端口（1-65535；越界或非法会告警并回退 3000） |
| `SQL_DSN` | 空 → `whats-api.db`（SQLite） | 主库 DSN，按前缀选驱动：`postgres://`→PG、`local`/`local:<path>`→SQLite、其他→MySQL |
| `LOG_SQL_DSN` | 空 → 复用主库 | 日志库 DSN（`logs` 表落在这里） |
| `REDIS_CONN_STRING` | 空 → 进程内缓存 | Redis 连接串；配置了但连不上会**告警并降级**，不影响启动 |
| `NODE_TYPE` | `master` | `master` 执行数据库迁移，`slave` 跳过。大小写不敏感；认不出的值按 `master` 处理并告警 |
| `NODE_NAME` | 空 | 实例标识，用于日志区分来源 |
| `SYNC_FREQUENCY` | `60` | 配置定时同步间隔（秒），最小 5 |
| `SHUTDOWN_TIMEOUT_SECONDS` | `120` | 优雅停机超时，最小 5（低于下限会告警并抬到 5）。流式响应可能跑几分钟，别调小 |
| `LOG_LEVEL` | `info` | 启动期日志级别，可被库里的配置覆盖 |
| `GIN_MODE` | `release` | gin 运行模式（`debug` 会打印 SQL 与路由表） |
| `CORS_ALLOW_ORIGINS` | `*` | 允许的跨域来源，逗号分隔。配了但没有有效条目（如 `,`）则谁都不允许——**不会**回退成 `*` |

## 改配置（不重启）

M0 没有管理面写接口，用 `cmd/optctl` 直接改库：

```bash
go build -o optctl ./cmd/optctl

./optctl list
./optctl get system_setting.status_message
./optctl set system_setting.status_message '"来自 optctl"'
./optctl set system_setting.log_level '"debug"'
```

值必须是 **JSON 编码**：字符串要带引号（shell 里用单引号包住双引号写法最省事）。

改动立即生效（改 `log_level` 会马上换日志级别），并会在 `SYNC_FREQUENCY` 秒内
同步到共享同一个库的其他实例。`/api/status` 里能看到 `status_message`、`log_level` 等字段的变化。

## 验收

```bash
./scripts/m0-acceptance.sh
```

脚本驱动 M0 的四条验收标准（零环境变量启动、无 Redis 可启动、配置热更新与多实例收敛、
SIGTERM 优雅停机），输出 `PASS` / `FAIL` / `NEEDS-EXTERNAL`：
前两条的 Docker 形态、第四条的真实信号投递，需要 Linux 或 docker 环境才能完整验证，
在 Windows 上会标记为 `NEEDS-EXTERNAL` 而不是假装通过。

单元测试：

```bash
go test -race ./...
```

## 下一步

M1（转发闭环）：`/v1/chat/completions` 透传 OpenAI 兼容上游，含 SSE 流式、
上游 5xx 重试与渠道自动禁用。设计文档见 `docs/superpowers/specs/`。

