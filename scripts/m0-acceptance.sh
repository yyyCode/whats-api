#!/usr/bin/env bash
# M0 验收脚本：驱动设计文档 §10.1 的四条验收标准。
#
# 判定三态（这是这个脚本最重要的一点）：
#   PASS            本机完整验证通过
#   NEEDS-EXTERNAL  本机环境不具备验证条件（无 docker / 无法投递 SIGTERM），
#                   需要你在 Linux 或 docker 环境复核 —— 而不是假装通过
#   FAIL            验证失败；只要有一个 FAIL，脚本退出码为 1
#
# 用法：
#   ./scripts/m0-acceptance.sh
#   BIN=/path/to/whats-api ./scripts/m0-acceptance.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BIN:-$ROOT/whats-api}"
WORK="$(mktemp -d)"
PIDS=()
PASS=0
FAIL=0
NEED=0

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  \033[32mPASS\033[0m            %s\n' "$1"; PASS=$((PASS + 1)); }
fail() { printf '  \033[31mFAIL\033[0m            %s\n' "$1"; FAIL=$((FAIL + 1)); }
need() { printf '  \033[33mNEEDS-EXTERNAL\033[0m  %s\n' "$1"; NEED=$((NEED + 1)); }

is_windows() {
  case "$(uname -s)" in
    MINGW* | MSYS* | CYGWIN*) return 0 ;;
    *) return 1 ;;
  esac
}

# json_get <json> <key>，key 支持 "db.driver" 这种一层嵌套
json_get() {
  local json="$1" key="$2"
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$json" | jq -r --arg k "$key" '
      ($k | split(".")) as $p
      | if ($p | length) == 1 then .[$p[0]] else .[$p[0]][$p[1]] end
      | tostring' 2>/dev/null
    return
  fi
  # 没有 jq 时退化为取第一个同名键——本脚本只用它读顶层与 db/cache 一层嵌套，
  # 而 JSON 里这些键名在各自段内唯一。失败时脚本会打印完整 JSON 供人工核对。
  printf '%s' "$json" | tr -d '\n' \
    | sed -n "s/.*\"${key##*.}\"[[:space:]]*:[[:space:]]*\([^,}]*\).*/\1/p" \
    | tr -d '"' | head -1
}

# wait_status <port> <timeout_seconds>，成功则把 body 打到 stdout
wait_status() {
  local port="$1" timeout="${2:-20}" deadline body
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if body="$(curl -fsS "http://127.0.0.1:$port/api/status" 2>/dev/null)"; then
      printf '%s' "$body"
      return 0
    fi
    sleep 0.3
  done
  return 1
}

# start_server <dir> [ENV=V ...] → 输出 pid
# 用 exec 让子 shell 被 env/二进制替换，于是 $! 就是二进制的 pid，kill 才有效。
start_server() {
  local dir="$1"
  shift
  ( cd "$dir" && exec env "$@" "$BIN" ) >"$dir/server.log" 2>&1 &
  echo $!
}

stop_server() {
  local pid="$1"
  [ -z "$pid" ] && return 0
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
}

echo "M0 验收开始（BIN=$BIN）"
echo

# ---------- 前置：构建二进制 ----------
if [ ! -x "$BIN" ]; then
  echo "构建二进制..."
  if ! ( cd "$ROOT" && go build -o "$BIN" . ); then
    echo "构建失败，无法继续" >&2
    exit 1
  fi
fi

OPTCTL="$WORK/optctl"
if ! ( cd "$ROOT" && go build -o "$OPTCTL" ./cmd/optctl ); then
  echo "构建 optctl 失败，无法继续" >&2
  exit 1
fi

# ---------- ① 零环境变量启动（SQLite）----------
# 在一个空目录里启动：确保不会读到仓库根的 .env（那会让"零环境变量"名不副实）。
echo "① 零环境变量启动（SQLite）"
D1="$WORK/t1"
mkdir -p "$D1"
PID1="$(start_server "$D1")"
PIDS+=("$PID1")

if BODY1="$(wait_status 3000 25)"; then
  DRIVER="$(json_get "$BODY1" db.driver)"
  DSNKIND="$(json_get "$BODY1" db.dsn_kind)"
  if [ "$DRIVER" = "sqlite" ]; then
    pass "① 零环境变量启动成功，db.driver=sqlite，dsn_kind=$DSNKIND"
  else
    fail "① 启动了但 db.driver=$DRIVER（期望 sqlite）"
  fi
  if [ -f "$D1/whats-api.db" ]; then
    pass "① SQLite 文件按默认名建在启动目录（whats-api.db）"
  else
    fail "① 未在启动目录看到 whats-api.db（$(ls -a "$D1")）"
  fi
else
  fail "① 不配任何环境变量时未能启动（端口 3000 可能被占用；日志见 $D1/server.log）"
  echo "     最后几行日志："
  tail -n 10 "$D1/server.log" 2>/dev/null | sed 's/^/       /'
fi
stop_server "$PID1"

# ---------- ①' Docker 形态 ----------
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  if ( cd "$ROOT" && docker build -q -t whats-api:m0 . >"$WORK/docker-build.log" 2>&1 ); then
    CID="$(docker run -d -p 3900:3000 whats-api:m0 2>/dev/null)"
    if DBODY="$(wait_status 3900 40)"; then
      if [ "$(json_get "$DBODY" db.driver)" = "sqlite" ]; then
        pass "① Docker 形态：容器零环境变量启动，db.driver=sqlite"
      else
        fail "① Docker 形态：驱动不符（$(json_get "$DBODY" db.driver)）"
      fi
    else
      fail "① Docker 形态：容器起来了但 /api/status 不响应"
    fi
    [ -n "${CID:-}" ] && docker rm -f "$CID" >/dev/null 2>&1
  else
    fail "① Docker 形态：docker build 失败（见 $WORK/docker-build.log）"
  fi
else
  need "① Docker 形态：本机无可用 docker，请在 Linux/docker 环境跑 docker compose up 复核"
fi

# ---------- ② 拿掉 Redis 也能启动 ----------
echo
echo "② 拿掉 Redis 也能启动（降级为内存缓存）"
D2="$WORK/t2"
mkdir -p "$D2"
# 故意配一个不可达的地址（端口 1 保留且无监听）：必须启动成功且降级。
PID2="$(start_server "$D2" "PORT=3001" "REDIS_CONN_STRING=127.0.0.1:1" "SQL_DSN=local:$D2/whats-api.db")"
PIDS+=("$PID2")

if BODY2="$(wait_status 3001 25)"; then
  BACKEND="$(json_get "$BODY2" cache.backend)"
  CONFIGURED="$(json_get "$BODY2" cache.redis_configured)"
  if [ "$BACKEND" = "memory" ]; then
    pass "② Redis 不可达时仍启动，cache.backend=memory（redis_configured=$CONFIGURED）"
  else
    fail "② cache.backend=$BACKEND，期望 memory"
  fi
  if [ "$CONFIGURED" = "true" ]; then
    pass "② 状态接口如实反映「配置了 Redis 但降级了」（redis_configured=true）"
  else
    fail "② redis_configured=$CONFIGURED，期望 true（应反映配置存在但未连上）"
  fi
  if grep -qi "降级" "$D2/server.log"; then
    pass "② 启动日志里有降级告警（不是静默忽略）"
  else
    fail "② 启动日志里没有降级告警"
  fi
else
  fail "② 配上不可达的 Redis 后启动失败（不应如此：缓存必须是非致命的）"
  tail -n 10 "$D2/server.log" 2>/dev/null | sed 's/^/       /'
fi
stop_server "$PID2"

# ---------- ③ 配置热更新与多实例收敛 ----------
echo
echo "③ 改配置：单实例立即生效 + 多实例 SYNC_FREQUENCY 内收敛"
D3="$WORK/t3"
mkdir -p "$D3"
SHARED_DB="local:$D3/shared.db"
MSG="脚本改的-$$"

PID3A="$(start_server "$D3" "PORT=3002" "SQL_DSN=$SHARED_DB" "SYNC_FREQUENCY=5" "NODE_NAME=node-a" "LOG_LEVEL=info")"
PIDS+=("$PID3A")
BODY_A="$(wait_status 3002 25)" || true

PID3B="$(start_server "$D3" "PORT=3003" "SQL_DSN=$SHARED_DB" "SYNC_FREQUENCY=5" "NODE_NAME=node-b" "LOG_LEVEL=info")"
PIDS+=("$PID3B")
BODY_B="$(wait_status 3003 25)" || true

if [ -z "${BODY_A:-}" ] || [ -z "${BODY_B:-}" ]; then
  fail "③ 两个实例未能同时启动（A=${BODY_A:0:40} B=${BODY_B:0:40}）"
else
  pass "③ 两个实例共享同一个 SQLite 库启动成功"

  BEFORE_B="$(json_get "$BODY_B" status_message)"

  # ★ 从进程外改配置：这正是"多实例同步"与"改内存"的区别所在
  if env "SQL_DSN=$SHARED_DB" "$OPTCTL" set system_setting.status_message "\"$MSG\"" >"$WORK/optctl.log" 2>&1; then
    pass "③ optctl 从进程外写入配置成功"
  else
    fail "③ optctl 写入失败（见 $WORK/optctl.log）"
  fi

  # 轮询实例 B：SYNC_FREQUENCY=5，给它 5 倍余量
  CONVERGED=""
  for _ in $(seq 1 25); do
    if CURRENT="$(json_get "$(curl -fsS http://127.0.0.1:3003/api/status 2>/dev/null)" status_message)" \
       && [ "$CURRENT" = "$MSG" ]; then
      CONVERGED="yes"
      break
    fi
    sleep 1
  done

  if [ -n "$CONVERGED" ]; then
    pass "③ 实例 B 在同步周期内收敛到新值（改前 $BEFORE_B → 改后 $MSG）"
  else
    fail "③ 实例 B 未收敛（期望 $MSG，实际 $(json_get "$(curl -fsS http://127.0.0.1:3003/api/status 2>/dev/null)" status_message)）"
  fi

  A_NOW="$(json_get "$(curl -fsS http://127.0.0.1:3002/api/status 2>/dev/null)" status_message)"
  if [ "$A_NOW" = "$MSG" ]; then
    pass "③ 实例 A 同样收敛"
  else
    fail "③ 实例 A 未收敛（实际 $A_NOW）"
  fi
fi
stop_server "$PID3A"
stop_server "$PID3B"

# ---------- ④ SIGTERM 优雅停机 ----------
echo
echo "④ SIGTERM 优雅停机"
if is_windows; then
  need "④ 优雅停机：Windows 无法向原生进程投递 SIGTERM，请在 Linux/docker 环境复核"
  echo "     本机可验证的等价路径已由单元测试覆盖："
  echo "       go test -race ./internal/app/ -run TestRunDrainsInFlightRequest"
else
  D4="$WORK/t4"
  mkdir -p "$D4"
  PID4="$(start_server "$D4" "PORT=3004" "SQL_DSN=local:$D4/whats-api.db")"
  if wait_status 3004 25 >/dev/null; then
    kill -TERM "$PID4"
    # 给足时间：停机超时默认 120 秒，但正常情况下应当秒级返回
    WAITED=0
    while kill -0 "$PID4" 2>/dev/null && [ "$WAITED" -lt 30 ]; do
      sleep 0.5
      WAITED=$((WAITED + 1))
    done

    if kill -0 "$PID4" 2>/dev/null; then
      fail "④ 收到 SIGTERM 后 15 秒内没有退出"
      kill -9 "$PID4" 2>/dev/null
    else
      wait "$PID4"
      CODE=$?
      if [ "$CODE" -eq 0 ]; then
        pass "④ 收到 SIGTERM 后正常退出（退出码 0）"
      else
        fail "④ 退出码 = $CODE，期望 0"
      fi
      if grep -q "已退出" "$D4/server.log"; then
        pass "④ 日志里有完整的停机序列（收到退出信号 → 已退出）"
      else
        fail "④ 日志里没有停机序列"
      fi
    fi
  else
    fail "④ 实例未能启动，无法测试停机"
  fi
fi

# ---------- 汇总 ----------
echo
echo "----------------------------------------"
printf 'PASS=%d  FAIL=%d  NEEDS-EXTERNAL=%d\n' "$PASS" "$FAIL" "$NEED"
if [ "$NEED" -gt 0 ]; then
  echo "说明：NEEDS-EXTERNAL 表示本机环境无法验证，需在 Linux/docker 环境复核（不是通过）。"
fi
if [ "$FAIL" -gt 0 ]; then
  echo "结论：验收未通过"
  exit 1
fi
echo "结论：本机可验证部分全部通过"
