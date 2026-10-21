// Package common 是基础设施层：启动期常量、JSON 编解码、数值安全、Redis 封装。
//
// 它只依赖 internal/logger（一个只依赖标准库的横切叶子包）。
// 持久化枚举在 internal/constant，那是给 model 层用的，common 不引用它——
// 这两处常量服务的目的不同：这里的默认值是"没配环境变量时用什么"，
// constant 里的是"已经写进库里的值不能变"。
package common

// Version 出现在启动日志、/api/status 与响应头 X-Whats-Api-Version。
const Version = "v0.2.0-m1"

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

// MinShutdownTimeoutSeconds 是优雅退出超时的下限。
//
// 它必须大于 0：超时会变成 context.WithTimeout 的 deadline，0 或负数
// 意味着"立刻取消"——正在处理的请求会被直接掐断，优雅退出就成了强杀。
const MinShutdownTimeoutSeconds = 5
