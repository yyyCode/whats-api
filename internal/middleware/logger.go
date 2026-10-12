package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/cachex"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

// TotalCounterKey 是请求总数计数器的键名。
const TotalCounterKey = "total"

// AccessLog 输出结构化访问日志，并累加请求总数。
//
// 配置读取走 system_setting.Get() 这份**不可变快照**，不读可写结构体：
// 直接读可写实例会在热更新时产生 data race（设计文档 §4.3）。
//
// 计数器为 nil 时照常工作（单测与极端初始化顺序下不该因为一个 nil 崩掉）。
func AccessLog(requests *cachex.Counter) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		// 计数与日志是两件事：关掉访问日志后计数仍然要涨，
		// 否则 /api/status 的 requests_total 会随日志开关一起"假装没流量"。
		if requests != nil {
			_, _ = requests.Incr(TotalCounterKey)
		}

		cfg := system_setting.Get()
		if cfg == nil || !cfg.EnableAccessLog {
			return
		}

		latency := time.Since(start)
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"size", c.Writer.Size(),
			"route_tag", GetRouteTag(c),
			"request_id", GetRequestID(c),
			"client_ip", c.ClientIP(),
		}
		if q := c.Request.URL.RawQuery; q != "" {
			attrs = append(attrs, "query", q)
		}
		// 慢请求标记：运维筛日志时"先看 slow"比按延迟排序方便。
		if cfg.SlowRequestThresholdSeconds > 0 && latency.Seconds() >= cfg.SlowRequestThresholdSeconds {
			attrs = append(attrs, "slow", true)
		}

		switch status := c.Writer.Status(); {
		case status >= http.StatusInternalServerError:
			logger.Error("请求完成", attrs...)
		case status >= http.StatusBadRequest:
			logger.Warn("请求完成", attrs...)
		default:
			logger.Info("请求完成", attrs...)
		}
	}
}
