// Package system_setting 是 M0 唯一的配置域。
//
// 配置域按「变更的人是谁」划分，而不是按技术分层：这个域放的是运维会改的运行时开关。
// 将来 billing_setting 归财务、model_setting 归模型负责人——分域即权限边界。
package system_setting

import (
	"sync/atomic"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

// SystemSetting 是运维可热更新的运行时开关。
//
// 这一个结构体同时承担四件事：类型（字段类型）、默认值（Default）、
// 持久化 key（模块名 + json tag）、文档（字段注释）。
// 加一个配置项只需要加一个字段——不需要改 schema、校验、默认值或文档的第二份定义。
type SystemSetting struct {
	// LogLevel 日志级别：debug / info / warn / error。改完立即生效。
	LogLevel string `json:"log_level"`

	// StatusMessage 会出现在 /api/status 里。
	// 它的存在意义是给验收标准③一个可观察点：改它能在不重启的情况下被 HTTP 看到。
	StatusMessage string `json:"status_message"`

	// EnableAccessLog 关掉后不再输出访问日志（请求计数仍然继续统计）。
	EnableAccessLog bool `json:"enable_access_log"`

	// SlowRequestThresholdSeconds 访问日志里慢请求的判定阈值（秒）。
	SlowRequestThresholdSeconds float64 `json:"slow_request_threshold_seconds"`
}

// Default 是本域的默认值。
//
// 默认值写在代码里，不写进数据库、也不写进 GORM tag：
// 只有"管理员显式改过"的项才落库，这样将来调默认值能影响所有未显式修改的实例。
var Default = SystemSetting{
	LogLevel:                    "info",
	StatusMessage:               "M0 技术骨架运行中",
	EnableAccessLog:             true,
	SlowRequestThresholdSeconds: 3,
}

// registered 是反射写入的目标，只允许配置系统访问。
// 业务代码一律用 Get()——直接读它会读到正在被写入的中间态。
var registered = Default

// current 是热路径读的不可变快照。
var current atomic.Pointer[SystemSetting]

func init() {
	d := Default
	current.Store(&d)

	config.GlobalConfig.Register("system_setting", &registered)
	config.GlobalConfig.OnRebuild("system_setting", func() {
		// 本回调由 config.Manager 在写锁内调用，所以这里读 registered 没有并发写。
		// 值拷贝 + 原子替换：读者从此只会看到完整版本。
		snap := registered
		current.Store(&snap)
	})
}

// Get 返回当前快照。返回值是共享的只读对象，**不要修改它的字段**。
func Get() *SystemSetting { return current.Load() }
