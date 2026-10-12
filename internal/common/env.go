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

	var warn string

	Port, warn = envInt("PORT", DefaultPort)
	appendEnvWarning(warn)
	if Port < 1 || Port > 65535 {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"PORT=%d 不是合法端口（1-65535），已回退默认值 %d", Port, DefaultPort))
		Port = DefaultPort
	}

	GinMode = envStr("GIN_MODE", "release")
	CORSAllowOrigins, warn = envList("CORS_ALLOW_ORIGINS", DefaultCORSAllowOrigins)
	appendEnvWarning(warn)

	SQLDSN = envRaw("SQL_DSN")
	LogSQLDSN = envRaw("LOG_SQL_DSN")
	RedisConnString = envRaw("REDIS_CONN_STRING")

	// 先小写再比较：运维写成 NODE_TYPE=Slave 时，按原样比较会把从节点判成主节点，
	// 从节点于是去跑迁移——这是"多节点同时建表"最直接的来源。
	NodeType = strings.ToLower(envStr("NODE_TYPE", DefaultNodeType))
	switch NodeType {
	case "master", "slave":
	default:
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"NODE_TYPE=%q 无法识别（只认 master/slave），按 %s 处理", NodeType, DefaultNodeType))
		NodeType = DefaultNodeType
	}
	NodeName = envRaw("NODE_NAME")
	IsMasterNode = NodeType != "slave"

	SyncFrequency, warn = envInt("SYNC_FREQUENCY", DefaultSyncFrequencySeconds)
	appendEnvWarning(warn)
	if SyncFrequency < MinSyncFrequencySeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SYNC_FREQUENCY=%d 低于下限 %d 秒，已抬到下限", SyncFrequency, MinSyncFrequencySeconds))
		SyncFrequency = MinSyncFrequencySeconds
	}

	ShutdownTimeout, warn = envInt("SHUTDOWN_TIMEOUT_SECONDS", DefaultShutdownTimeoutSeconds)
	appendEnvWarning(warn)
	if ShutdownTimeout < MinShutdownTimeoutSeconds {
		EnvWarnings = append(EnvWarnings, fmt.Sprintf(
			"SHUTDOWN_TIMEOUT_SECONDS=%d 低于下限 %d 秒，已抬到下限",
			ShutdownTimeout, MinShutdownTimeoutSeconds))
		ShutdownTimeout = MinShutdownTimeoutSeconds
	}
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
// 一个写错的端口不该让服务完全起不来，但也**不能静默**：
// 回退必须留痕，否则 PORT=abc 会安安静静地跑在 3000 上，
// 而运维以为服务听在他们配的端口上。第二个返回值是告警文本。
func envInt(key string, def int) (value int, warn string) {
	v := envRaw(key)
	if v == "" {
		return def, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Sprintf("%s=%q 不是整数，已回退默认值 %d", key, v, def)
	}
	return n, ""
}

// appendEnvWarning 收集一条告警，空串表示无告警。
func appendEnvWarning(w string) {
	if w != "" {
		EnvWarnings = append(EnvWarnings, w)
	}
}

// envList 读取逗号分隔的列表，逐项去空白，空项丢弃。
//
// 它把"没配"和"配了但一个有效条目都没有"分开处理，这是安全语义上的区别：
//
//	没配（键不存在或为空） → 用默认值
//	配了但全是空项（如 ","） → 空列表，并告警
//
// 后一种情况若回落到默认值，默认值里的 "*" 是"允许所有来源"，
// 于是一次本想收紧的配置反而把 CORS 完全打开了。空列表是"谁都不允许"，
// 属于失败时收紧（fail-closed）。
//
// 第二个返回值是给 InitEnv 收集的告警文本，空串表示无告警。
func envList(key, def string) (values []string, warn string) {
	raw := envRaw(key)
	if raw == "" {
		return strings.Split(def, ","), ""
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{}, fmt.Sprintf(
			"%s 已配置但没有有效条目，按空列表处理（不信任任何跨域来源）；"+
				"要允许所有来源请显式写 *", key)
	}
	return out, ""
}
