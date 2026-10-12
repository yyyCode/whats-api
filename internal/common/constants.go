// Package common 是基础设施层：启动期常量、JSON 编解码、数值安全、Redis 封装。
//
// 它只依赖 internal/logger 与 internal/constant——两个只依赖标准库的叶子包。
package common

// Version 出现在启动日志、/api/status 与响应头 X-Whats-Api-Version。
const Version = "v0.1.0-m0"

// CacheNamespacePrefix 是缓存键前缀，避免与同一个 Redis 实例上的其他系统撞键。
const CacheNamespacePrefix = "whats-api"

// 默认值集中在这里：环境变量只做覆盖，代码里不再散落字面量。
//
// 命名注意：秒级默认值统一带 Seconds 后缀（测试与其他任务都按这个名字引用）。
const (
	DefaultPort                   = 3000
	DefaultSyncFrequencySeconds   = 60
	DefaultShutdownTimeoutSeconds = 120
	DefaultSQLiteFile             = "whats-api.db"
	DefaultLogLevel               = "info"
	DefaultCORSAllowOrigins       = "*"
	DefaultNodeType               = "master"
)

// MinSyncFrequencySeconds 是同步间隔的下限：再低就是在把业务库当轮询靶子。
const MinSyncFrequencySeconds = 5
