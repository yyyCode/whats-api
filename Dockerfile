# 多阶段构建：编译阶段有完整工具链，运行阶段只有一个静态二进制。
FROM golang:1.23-alpine AS builder

WORKDIR /src

# 先只拷依赖清单再下载：源码改动时这一层还能命中缓存，
# 不必每次重新拉一遍依赖。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 能成立，是因为 SQLite 驱动选的是纯 Go 实现
# （glebarez/sqlite → modernc.org/sqlite）。换回 CGO 版驱动就必须装 gcc 并放弃静态链接。
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/whats-api .

FROM alpine:3.20

# ca-certificates：M1 起要访问上游 HTTPS 接口；
# tzdata：日志与账本按本地时区展示（存储仍是 unix 秒）。
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app

WORKDIR /app
COPY --from=builder /out/whats-api /app/whats-api

# 数据落在卷里：容器重建不丢配置与库。
# SQL_DSN 由镜像给出默认值，用户仍可用环境变量覆盖（比如指向外部 PostgreSQL）。
ENV SQL_DSN=/data/whats-api.db
RUN mkdir -p /data && chown -R app:app /data /app
VOLUME /data

# 非 root 运行：容器逃逸后的影响面小得多。
USER app

EXPOSE 3000

# 探活用 /api/status：它同时反映"进程活着"与"库连得上"。
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:3000/api/status >/dev/null || exit 1

# 容器里本进程就是 PID 1。PID 1 默认会把未显式处理的信号忽略掉，
# 所以必须自己处理 SIGTERM（app.Run 收到 ctx 取消后做优雅停机）——
# 否则 docker stop 只能干等到超时再 SIGKILL，在途请求全部被砍。
ENTRYPOINT ["/app/whats-api"]
