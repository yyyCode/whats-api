package controller

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/model"
	"github.com/yyyCode/whats-api/internal/setting/config"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

// Deps 是 handler 的依赖。
//
// 计数器用接口而不是 *cachex.Counter：
//   - 依赖规则里 controller 直连 cachex 是一条多余的边（它经由 model 已经可达）；
//   - 单测里可以塞一个假计数器，不必真起缓存。
//
// 这是 Go 的惯例"在消费方定义小接口"，不是过度抽象。
type Deps struct {
	Requests  Counter
	StartedAt time.Time
}

// Counter 是本 handler 需要的最小缓存能力：读一个计数。
type Counter interface {
	Get(key string) (int64, bool)
}

// StatusData 是 /api/status 的 data 段。
//
// 这里**不含任何敏感信息**：没有 DSN 全文（DSN 里有密码）、没有密钥、
// 不返回配置项的完整值，只返回运维定位问题需要的开关与状态。
type StatusData struct {
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	NodeType      string `json:"node_type"`
	NodeName      string `json:"node_name"`
	IsMaster      bool   `json:"is_master"`

	DB     DBStatus     `json:"db"`
	LogDB  LogDBStatus  `json:"log_db"`
	Cache  CacheStatus  `json:"cache"`
	Config ConfigStatus `json:"config"`

	SetupComplete bool `json:"setup_complete"`

	StatusMessage               string  `json:"status_message"`
	LogLevel                    string  `json:"log_level"`
	EnableAccessLog             bool    `json:"enable_access_log"`
	SlowRequestThresholdSeconds float64 `json:"slow_request_threshold_seconds"`

	RequestsTotal int64 `json:"requests_total"`
}

type DBStatus struct {
	Driver  string `json:"driver"`
	DSNKind string `json:"dsn_kind"`
	OK      bool   `json:"ok"`
}

type LogDBStatus struct {
	Driver         string `json:"driver"`
	SharedWithMain bool   `json:"shared_with_main"`
	OK             bool   `json:"ok"`
}

type CacheStatus struct {
	Backend         string `json:"backend"`
	RedisConfigured bool   `json:"redis_configured"`
}

type ConfigStatus struct {
	Modules        []string `json:"modules"`
	RegisteredKeys int      `json:"registered_keys"`
}

// Status 是 M0 唯一的业务端点：既是探活，也是验收的观察窗。
//
// 管理面的写接口是 M3 的事，所以"改配置不重启即生效"这条验收标准
// 必须在 /api/status 上可被观察到——它不是调试信息，是验收装置。
func Status(deps Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		APISuccess(c, buildStatus(deps))
	}
}

func buildStatus(deps Deps) StatusData {
	cfg := system_setting.Get()

	// 探活失败不是错误：接口本身仍要返回 200，
	// 让运维能看到"服务活着但库连不上"这个事实（用 500 反而看不出是哪一段坏了）。
	setupComplete, _ := model.SetupComplete()

	data := StatusData{
		Version:       common.Version,
		UptimeSeconds: int64(time.Since(deps.StartedAt).Seconds()),
		NodeType:      common.NodeType,
		NodeName:      common.NodeName,
		IsMaster:      common.IsMasterNode,

		DB: DBStatus{
			Driver:  model.MainDriver(),
			DSNKind: model.DSNKind(),
			OK:      model.PingMain(),
		},
		LogDB: LogDBStatus{
			Driver:         model.LogDriver(),
			SharedWithMain: !model.LogDBSeparate(),
			OK:             model.PingLog(),
		},
		Cache: CacheStatus{
			Backend:         cacheBackend(deps),
			RedisConfigured: common.RedisConnString != "",
		},
		Config: ConfigStatus{
			Modules:        config.GlobalConfig.Modules(),
			RegisteredKeys: config.GlobalConfig.FieldCount(),
		},

		SetupComplete: setupComplete,
	}

	if cfg != nil {
		data.StatusMessage = cfg.StatusMessage
		data.LogLevel = cfg.LogLevel
		data.EnableAccessLog = cfg.EnableAccessLog
		data.SlowRequestThresholdSeconds = cfg.SlowRequestThresholdSeconds
	}

	if deps.Requests != nil {
		data.RequestsTotal, _ = deps.Requests.Get("total")
	}

	return data
}

// cacheBackend 报告实际生效的缓存后端。
// 读 RedisEnabled 而不是"是否配置了 REDIS_CONN_STRING"：
// 配了但连不上时应当显示 memory——那才是实际发生的事。
func cacheBackend(deps Deps) string {
	if common.RedisEnabled() {
		return "redis"
	}
	return "memory"
}
