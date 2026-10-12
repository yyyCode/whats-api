package common

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 启动期常量：InitEnv 写一次、之后只读，因此并发安全。
//
// 与 Options（可热更新的运行期配置）严格区分：需要热更新的东西绝不能放这里。
var (
	Port             int
	GinMode          string
	CORSAllowOrigins []string

	SQLDSN          string
	LogSQLDSN       string
	RedisConnString string

	NodeType     string
	NodeName     string
	IsMasterNode bool

	SyncFrequency   int // 秒
	ShutdownTimeout int // 秒
	LogLevel        string

	// EnvWarnings 收集初始化期的可告警项。InitEnv 跑在 logger 之前，
	// 所以这里先攒着，等 logger 起来后由启动流程统一输出。
	EnvWarnings []string
)

// InitEnv 从环境变量装载启动期常量，必须在任何模块读配置之前调用。
//
// 所有变量都有默认值：不配任何环境变量也能启动。
func InitEnv() {
	EnvWarnings = nil

	Port = envInt("PORT", DefaultPort)
	GinMode = envStr("GIN_MODE", "release")
	CORSAllowOrigins = envList("CORS_ALLOW_ORIGINS", DefaultCORSAllowOrigins)

	SQLDSN = envRaw("SQL_DSN")
	LogSQLDSN = envRaw("LOG_SQL_DSN")
	RedisConnString = envRaw("REDIS_CONN_STRING")

	NodeType = envStr("NODE_TYPE", DefaultNodeType)
	NodeName = envRaw("NODE_NAME")
	IsMasterNode = NodeType != "slave"

	SyncFrequency = envInt("SYNC_FREQUENCY", DefaultSyncFrequencySeconds)
	if SyncFrequency < MinSyncFrequencySeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SYNC_FREQUENCY=%d 低于下限 %d 秒，已抬到下限", SyncFrequency, MinSyncFrequencySeconds))
		SyncFrequency = MinSyncFrequencySeconds
	}

	ShutdownTimeout = envInt("SHUTDOWN_TIMEOUT_SECONDS", DefaultShutdownTimeoutSeconds)
	LogLevel = envStr("LOG_LEVEL", DefaultLogLevel)
}

// envRaw 返回原样（仅去空白）的变量值，空字符串按"未配置"处理。
func envRaw(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func envStr(key, def string) string {
	if v := envRaw(key); v != "" {
		return v
	}
	return def
}

// envInt 读取整数。非法值不致命：回退默认值继续启动——
// 一个写错的端口不该让服务完全起不来，但也不该被静默接受成 0。
func envInt(key string, def int) int {
	v := envRaw(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envList 读取逗号分隔的列表，逐项去空白，空项丢弃。
func envList(key, def string) []string {
	raw := envStr(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{def}
	}
	return out
}
