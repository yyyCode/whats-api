# M0 验收记录

执行时间：2026-10-12 18:07 (+08:00)
执行环境：`MINGW64_NT-10.0-22621`（Windows 11，MSYS2 / Git Bash 3.4.10）、`go version go1.23.2 windows/amd64`
执行命令：`./scripts/m0-acceptance.sh`（退出码 0）
被验二进制：本次运行由本机 `go build -o whats-api .` 现场产出

## 四条验收标准

| # | 标准 | 本机结果 | 备注 |
|---|---|---|---|
| ① | 零环境变量启动（SQLite） | **PASS** | `db.driver=sqlite`、`dsn_kind=file`，库文件落在启动目录；Docker 形态 `NEEDS-EXTERNAL` |
| ② | 拿掉 Redis 也能启动 | **PASS**（3/3） | 配了不可达的 Redis 仍启动，`cache.backend=memory` 且 `redis_configured=true`，日志有降级告警 |
| ③ | 配置热更新 + 多实例收敛 | **PASS**（4/4） | 两个实例共享同一 SQLite，`optctl` 从**进程外**写库，实例 B 在同步周期内由 `M0 技术骨架运行中` 收敛到脚本写入的新值，实例 A 同样收敛 |
| ④ | SIGTERM 优雅停机 | **NEEDS-EXTERNAL** | Windows 无法向原生进程投递 SIGTERM；等价路径由 `internal/app` 的 in-process 测试覆盖（见下） |

合计 `PASS=9 FAIL=0 NEEDS-EXTERNAL=2`。

## 全量门禁

| 命令 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `gofmt -l .` | 无输出 |
| `go test -count=1 ./...` | 12 个包全 `ok`（根目录无测试文件） |
| `go test -race -count=1 ./internal/cachex/... ./internal/setting/... ./internal/common/...` | 通过 |
| `go test -race -shuffle=on -count=2 ./internal/model/...` | 通过 |
| `go test -race -count=1 ./internal/app/...` | 通过（32.7s） |

> `-race` 只压在并发敏感的包上，没有对全仓跑。这是本记录里唯一一处
> 「做了但没做全」的取舍，写在这里以免被读成「全仓 race 干净」。

## 优雅停机的等价覆盖

本机无法投递真实信号，可验证的路径是把信号处理留在 `main.go` 一层薄壳、
让 `internal/app` 的 `Run(ctx)` 能被直接取消：

```
go test -race -count=1 ./internal/app/... -run TestRunDrainsInFlightRequest
```

该测试驱动一个在途请求（handler 内 800ms），取消 ctx 后断言请求跑完、
`CloseDB` 已执行（句柄被置空）。**它不等价于 `kill -TERM` 的端到端验收**——
信号的接线本身只做了编译期核对。

## 未验证事项（明确列出，不谎称通过）

| 事项 | 原因 | 复核方式 |
|---|---|---|
| Docker 构建与容器内运行 | 本机无 docker | 在有 docker 的环境执行 `docker compose up --build`，再跑一次本脚本 |
| 真实 SIGTERM 投递 | Windows 无法向原生进程投递 SIGTERM | Linux/macOS 上 `kill -TERM <pid>`，或 `docker stop`（容器内本进程是 PID 1） |
| MySQL / PostgreSQL 真实连接 | 本机无这两个服务 | 起容器后把 `SQL_DSN` 指过去，检查 `/api/status` 的 `db.ok` |
| Redis 真实连接成功路径 | 本机无 Redis 服务，只覆盖了「配了但连不上」的降级 | 起一个 Redis，确认 `cache.backend=redis` |
| ClickHouse 日志库 | M0 明确不引入驱动 | 不需要（后续里程碑） |
| 手动 Ctrl+C 交互路径 | 非交互环境无法完成 | 在终端里人工按一次 Ctrl+C |

## 脚本原始输出

原样粘贴，仅去掉终端颜色转义符（`\x1b[...m`），未做任何其他改动：

```text
M0 验收开始（BIN=/f/go/whats-api/whats-api）

① 零环境变量启动（SQLite）
  PASS            ① 零环境变量启动成功，db.driver=sqlite，dsn_kind=file
  PASS            ① SQLite 文件按默认名建在启动目录（whats-api.db）
  NEEDS-EXTERNAL  ① Docker 形态：本机无可用 docker，请在 Linux/docker 环境跑 docker compose up 复核

② 拿掉 Redis 也能启动（降级为内存缓存）
  PASS            ② Redis 不可达时仍启动，cache.backend=memory（redis_configured=true）
  PASS            ② 状态接口如实反映「配置了 Redis 但降级了」（redis_configured=true）
  PASS            ② 启动日志里有降级告警（不是静默忽略）

③ 改配置：单实例立即生效 + 多实例 SYNC_FREQUENCY 内收敛
  PASS            ③ 两个实例共享同一个 SQLite 库启动成功
  PASS            ③ optctl 从进程外写入配置成功
  PASS            ③ 实例 B 在同步周期内收敛到新值（改前 M0 技术骨架运行中 → 改后 脚本改的-330）
  PASS            ③ 实例 A 同样收敛

④ SIGTERM 优雅停机
  NEEDS-EXTERNAL  ④ 优雅停机：Windows 无法向原生进程投递 SIGTERM，请在 Linux/docker 环境复核
     本机可验证的等价路径已由单元测试覆盖：
       go test -race ./internal/app/ -run TestRunDrainsInFlightRequest

----------------------------------------
PASS=9  FAIL=0  NEEDS-EXTERNAL=2
说明：NEEDS-EXTERNAL 表示本机环境无法验证，需在 Linux/docker 环境复核（不是通过）。
结论：本机可验证部分全部通过
```

## 脚本改过三处（都是首轮实跑逼出来的）

首轮运行 `PASS=2 FAIL=2`，两条 FAIL 与 ③ 的「两个实例都没起来」同源：

1. **从信封之外读字段**。管理面响应是 `{success, message, data}`，而脚本原先从根取
   `db.driver` / `cache.backend`，全是空值——四条标准会假 FAIL。现在 `json_get` 先剥信封。
   另有两处细节：jq 分支不能用 `//` 解包（jq 把 `false` 当空，而 `redis_configured`
   与 `db.ok` 的合法值正是 `false`）；无 jq 时的兜底必须用 `grep -o` 取第一处，
   原先的 `sed` 里 `.*` 贪婪会命中最后一个同名键（`driver` 在 `db` 与 `log_db` 里各一个）。
   本机实测无 jq，走的正是这条兜底。
2. **MSYS 路径喂给了原生二进制**。`mktemp -d` 给出 `/tmp/...`，Windows 版 Go 程序不认，
   SQLite 报 `unable to open database file`（错误文案 `out of memory (14)` 是
   `SQLITE_CANTOPEN` 的误导性标签）。改为相对文件名——`start_server` 本来就会
   `cd` 进各自目录，于是对所有平台都成立。
3. **`optctl` 的工作目录**。改用相对 DSN 后，`optctl` 必须与两个实例在同一目录里跑，
   否则它会连到另一个（或新建一个）库，③ 就成了「自己写自己读」的假通过。
