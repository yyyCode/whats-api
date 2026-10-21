# M1 验收记录：转发闭环

执行时间：2026-10-21 17:57 (+08:00)
执行环境：Windows 11（10.0.22621）、`go version go1.26.5 windows/amd64`
分支：`feat/m1-relay`（自 `main` 的 `4c98b86` 拉出）
范围：一次 OpenAI 兼容 `POST /v1/chat/completions` 端到端跑通，含流式、重试、自动禁用、客户端断开停上游。

> 说明：`go.mod` 仍声明 `go 1.23.0`；本机实际工具链是 1.26.5。M0 验收时是 1.23.2。
> 本次没有改动 `go.mod`，构建与测试用的是新工具链。

## 四条验收标准

| # | 标准（路线图 M1） | 覆盖用例 | 结果 |
|---|---|---|---|
| ① | 流式逐 token 可见，客户端断开时立刻停止读上游 | `TestRelayStreamsIncrementally`、`TestRelayStopsUpstreamWhenClientDisconnects` | **PASS** |
| ② | 上游 5xx 切换渠道重试（只在尚未向客户端写过字节时） | `TestRelayRetriesOnUpstream5xxAndDisablesChannel`、`TestRelayDoesNotRetryAfterStreamStarted` | **PASS** |
| ③ | 长响应有保活 | `TestStreamScannerSendsKeepAlive` | **PASS** |
| ④ | 单渠道故障自动禁用并在后续请求中跳过 | `TestRelayRetriesOnUpstream5xxAndDisablesChannel`（第二段断言） | **PASS** |

附加验证：非流式响应字节级透传（`TestRelayNonStreamPassthrough`）、模型映射改写上游请求体（`TestRelayAppliesModelMapping`）、鉴权失败返回 OpenAI 错误形状（`TestRelayAuthFailureUsesOpenAIShape`）、不跟随上游重定向且不禁用渠道（`TestRelayDoesNotFollowUpstreamRedirect`）、客户端断开不误判渠道故障（`TestDoResponseClientGoneIsNotChannelError`）、`/v1` panic 保持 OpenAI 形状（`TestRecoveryUsesOpenAIShapeOnRelayPath`）。

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
ok      github.com/yyyCode/whats-api/cmd/optctl                       15.157s
ok      github.com/yyyCode/whats-api/internal/app                     43.053s
ok      github.com/yyyCode/whats-api/internal/cachex                  6.343s
ok      github.com/yyyCode/whats-api/internal/common                  6.059s
ok      github.com/yyyCode/whats-api/internal/constant                2.235s
ok      github.com/yyyCode/whats-api/internal/controller              11.139s
ok      github.com/yyyCode/whats-api/internal/logger                  2.430s
ok      github.com/yyyCode/whats-api/internal/middleware              11.229s
ok      github.com/yyyCode/whats-api/internal/model                   7.976s
ok      github.com/yyyCode/whats-api/internal/relay                   5.563s
?       github.com/yyyCode/whats-api/internal/relay/channel           [no test files]
ok      github.com/yyyCode/whats-api/internal/relay/channel/openai    6.348s
ok      github.com/yyyCode/whats-api/internal/relay/common            5.409s
ok      github.com/yyyCode/whats-api/internal/relay/helper            8.030s
ok      github.com/yyyCode/whats-api/internal/router                  10.340s
ok      github.com/yyyCode/whats-api/internal/service                 7.435s
ok      github.com/yyyCode/whats-api/internal/setting/config          2.633s
ok      github.com/yyyCode/whats-api/internal/setting/system_setting  0.351s
ok      github.com/yyyCode/whats-api/internal/types                   3.758s

$ go test -race -count=1 ./internal/relay/... ./internal/service/... ./internal/controller/... ./internal/middleware/...
ok      github.com/yyyCode/whats-api/internal/relay                   7.848s
?       github.com/yyyCode/whats-api/internal/relay/channel           [no test files]
ok      github.com/yyyCode/whats-api/internal/relay/channel/openai    7.757s
ok      github.com/yyyCode/whats-api/internal/relay/common            6.844s
ok      github.com/yyyCode/whats-api/internal/relay/helper            9.128s
ok      github.com/yyyCode/whats-api/internal/service                 8.731s
ok      github.com/yyyCode/whats-api/internal/controller              8.079s
ok      github.com/yyyCode/whats-api/internal/middleware              5.616s
```

> `-race` 只压在转发、选路、鉴权这些并发敏感的包上，**没有对全仓跑**。
> 这是本记录里唯一一处「做了但没做全」的取舍，写在这里以免被读成「全仓 race 干净」。

## 验收用例的逐条输出

```text
$ go test ./internal/controller/ -run 'TestRelay' -v -count=1
=== RUN   TestRelayNonStreamPassthrough
--- PASS: TestRelayNonStreamPassthrough (0.05s)
=== RUN   TestRelayAppliesModelMapping
--- PASS: TestRelayAppliesModelMapping (0.03s)
=== RUN   TestRelayRetriesOnUpstream5xxAndDisablesChannel
--- PASS: TestRelayRetriesOnUpstream5xxAndDisablesChannel (0.05s)
=== RUN   TestRelayDoesNotRetryAfterStreamStarted
--- PASS: TestRelayDoesNotRetryAfterStreamStarted (0.05s)
=== RUN   TestRelayStreamsIncrementally
--- PASS: TestRelayStreamsIncrementally (0.56s)
=== RUN   TestRelayStopsUpstreamWhenClientDisconnects
--- PASS: TestRelayStopsUpstreamWhenClientDisconnects (0.06s)
=== RUN   TestRelayAuthFailureUsesOpenAIShape
--- PASS: TestRelayAuthFailureUsesOpenAIShape (0.06s)
PASS
ok      github.com/yyyCode/whats-api/internal/controller       5.200s

$ go test ./internal/relay/helper/ -run 'TestStreamScannerSendsKeepAlive' -v -count=1
=== RUN   TestStreamScannerSendsKeepAlive
--- PASS: TestStreamScannerSendsKeepAlive (0.31s)
PASS
ok      github.com/yyyCode/whats-api/internal/relay/helper     4.279s
```

这些用例不是 mock 出来的「单元级」验证：它们启动真实的 `gin` 路由、真实 SQLite 临时库、`httptest` 上游（真实 TCP 监听），再用 Go 的标准 HTTP 客户端发请求。也就是说，路由、鉴权、选路、适配器、SSE 转发、重试与自动禁用都走了生产代码路径。

### 终审修复后的复跑（同日）

终审发现三处问题并修复：非流式 body 读失败时客户端断开被误判渠道故障、出站客户端默认跟随重定向、`/v1` panic 返回管理面信封。修复后重跑：

```text
$ go test ./internal/controller/ -run 'TestRelay' -v -count=1
--- PASS: TestRelayNonStreamPassthrough (0.06s)
--- PASS: TestRelayAppliesModelMapping (0.03s)
--- PASS: TestRelayRetriesOnUpstream5xxAndDisablesChannel (0.04s)
--- PASS: TestRelayDoesNotRetryAfterStreamStarted (0.04s)
--- PASS: TestRelayStreamsIncrementally (0.60s)
--- PASS: TestRelayStopsUpstreamWhenClientDisconnects (0.09s)
--- PASS: TestRelayDoesNotFollowUpstreamRedirect (0.08s)
--- PASS: TestRelayAuthFailureUsesOpenAIShape (0.04s)
ok  	github.com/yyyCode/whats-api/internal/controller	7.618s

$ go test ./internal/middleware/ -run TestRecoveryUsesOpenAIShapeOnRelayPath -v -count=1
--- PASS: TestRecoveryUsesOpenAIShapeOnRelayPath (0.01s)

$ go test ./internal/relay/channel/openai/ -run TestDoResponseClientGone -v -count=1
--- PASS: TestDoResponseClientGoneIsNotChannelError (0.00s)
```

修复后同样重跑了全量门禁（`go build` / `go vet` / `gofmt -l` 全清，`go test -count=1 ./...` 全包 ok）与 `-race`（同一组并发敏感包，全 ok），耗时数字略有变化，结论与上表一致。

## 明确未验证事项（不谎称通过）

| 事项 | 原因 | 复核方式 |
|---|---|---|
| 真实 OpenAI 或第三方兼容上游 | 需要真实密钥与网络 | 用 `optctl` 建一个真实渠道，`curl /v1/chat/completions`（流式/非流式各一次） |
| 真实 SDK 客户端（openai-python / openai-node） | 本机未安装这些 SDK | 用 SDK 跑一次流式对话，确认 `stream=True` 下逐块可读 |
| gzip 压缩的请求体 | M1 明确不支持（M3 前补 Decompress 中间件） | 不在本阶段范围 |
| 多实例下选路一致性 | M1 每次直查数据库，没有缓存；多实例行为在 M4 才验证 | 起两个实例共享同一库，交替请求观察渠道分布 |
| 计费与 usage 统计 | M2 主题；M1 解析后丢弃 usage | 不在本阶段范围 |
| MySQL / PostgreSQL 实连 | 本机无这两个服务 | 起容器后把 `SQL_DSN` 指过去，重跑转发用例 |
| TCP 代理（`ProxyFromEnvironment`）行为 | 本机未配置代理 | 在需要代理的环境复核 |
| 上游 3xx 重定向 | 已用返回 302 的 `httptest` 上游验证：网关回 502、不跟随、不禁用渠道 | 接一个真实会重定向的 `base_url` 复核 |
| 超过 60 秒的长流 | e2e 只跑了 0.5 秒的流；保活间隔在单测里用 50ms 观察 | 用慢速真实上游跑一次 > 60s 的流 |
| 32MB 请求体上限 | 未做压力验证 | 用接近上限的多模态请求复核 |
| `optctl` 三个夹具命令的进程外真实调用 | 只有包内测试；未在一次真实进程启动后手工执行 | 按文末步骤做一次手工冒烟 |

## 手工冒烟步骤（未在本次执行，留给有真实上游的人）

```bash
# 1. 起网关（零外部依赖，SQLite 落在当前目录）
go run . &

# 2. 从进程外建夹具（与网关共用同一个 SQL_DSN）
go run ./cmd/optctl user add alice default 1000000
go run ./cmd/optctl token add sk-local 1
go run ./cmd/optctl channel add 1 sk-upstream https://api.openai.com gpt-4o-mini

# 3. 非流式
curl -s http://127.0.0.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer sk-local' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

# 4. 流式（应逐块到达）
curl -N http://127.0.0.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer sk-local' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## 与设计文档的偏离（实现时拍板）

| 项 | 实际做法 | 原因 |
|---|---|---|
| `middleware/distributor.go` | 未创建；选渠道在 `controller.Relay` 的重试循环里 | 重试必须重选；放中间件会把 body 解析与选路各做两遍。等 M3 有更细的中间件需求再搬 |
| 429 的归因 | 可重试，但**不**自动禁用渠道 | 429 是上游限流，不是渠道故障；当故障禁用会在流量高峰批量打掉渠道 |
| 自动禁用 | 条件 UPDATE（`status=enabled → auto_disabled`） | 手动禁用的渠道绝不能被自动化改写 |
| 令牌名称 | `AuthInfo.TokenName` 留空 | `Token` 模型没有 `name` 列；把明文 key 放进上下文只会给日志埋泄漏点 |
| 非流式响应 | 原样透传上游状态码与 Content-Type | M1 是透传；改写响应形状会让客户端难以解释 |
| 3xx 上游响应 | 归一化为 502，但不重试、不自动禁用 | `base_url` 配错时常见 3xx；同一请求里换渠道不会让配置变对，禁用只会把唯一渠道也打掉 |
| 出站重定向 | `sharedClient` 设 `CheckRedirect = ErrUseLastResponse`，3xx 原样交给 `DoResponse` | Go 默认跟随重定向：POST 常被改成 GET、丢掉 body 与跨主机 Authorization，最后把登录页当成功响应透传 |
| `/v1` 的 panic 响应 | 返回 OpenAI 错误形状（`error.type=server_error`），`/api` 仍是管理面信封 | 两个面的错误契约从 M1 起分开；SDK 不解析管理面信封 |
| 慢客户端写阻塞 | 未设写超时：客户端保持连接但不读时，SSE 写协程可能被阻塞较久 | M1 不引入连接级超时；需要时在 M3 用 `http.Server.WriteTimeout`/单事件写超时解决 |
