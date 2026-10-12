// Package constant 存放**持久化枚举**。
//
// 这些数值一旦写进数据库就是 ABI：只能追加，不能改变已有值，也不能重排。
// 改动它们必须同时改 constants_test.go —— 那一步失败正是在提醒你：
// 线上库里已经存着旧值，需要一次数据迁移。
package constant

// 用户角色。数值越大权限越高，比较用 >= 而不是 ==。
const (
	RoleUser  = 1
	RoleAdmin = 10
	RoleRoot  = 100
)

// 用户状态。
const (
	UserStatusEnabled  = 1
	UserStatusDisabled = 2
)

// 渠道状态。
//
// ManuallyDisabled 与 AutoDisabled 从第一天就分开：后续的自动恢复逻辑
// 只允许恢复 AutoDisabled，绝不能把运维手动禁用的渠道恢复回来。
// 如果这时把两者合并成一个值，将来想做这条区分就晚了（库里存量数据无法回溯）。
const (
	ChannelStatusEnabled          = 1
	ChannelStatusManuallyDisabled = 2
	ChannelStatusAutoDisabled     = 3
)

// 令牌状态。
const (
	TokenStatusEnabled   = 1
	TokenStatusDisabled  = 2
	TokenStatusExpired   = 3
	TokenStatusExhausted = 4
)

// 日志类型。logs.type 是账本的一列，用来区分"这笔记录是什么事"。
const (
	LogTypeUnknown = 0
	LogTypeConsume = 1
	LogTypeTopup   = 2
	LogTypeManage  = 3
	LogTypeSystem  = 4
	LogTypeError   = 5
	LogTypeRefund  = 6
	LogTypeLogin   = 7
)
