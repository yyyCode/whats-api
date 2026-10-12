// Package middleware 是全局中间件。
//
// 依赖规则：本包不 import controller（避免与路由层成环）。
// 因此这里的错误响应是手写的 JSON 字面量，而不是复用 controller 的信封函数——
// 这是一处刻意的重复：三个字段的字面量重复一次，好过让中间件依赖 handler 层。
package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gin-gonic/gin"
)

const (
	// RequestIDKey 是 gin.Context 里的键名。
	RequestIDKey = "request_id"
	// RequestIDHeader 是入站沿用、出站写回的响应头。
	RequestIDHeader = "X-Request-Id"
)

// RequestId 保证每个请求都有一个可追踪的 ID：
// 入站带了就沿用（便于跨服务串联），没带就生成。
// 没有它，线上排查只能靠时间戳对日志。
func RequestId() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = generateRequestID()
		}
		c.Set(RequestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// GetRequestID 读取当前请求的 ID；缺失时返回空串（不 panic）。
func GetRequestID(c *gin.Context) string {
	if v, ok := c.Get(RequestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// generateRequestID 生成 128 位随机 ID（32 位十六进制）。
// 用 crypto/rand 而不是 math/rand：ID 会出现在日志与响应头里，
// 可预测的 ID 意味着可以被用来伪造/关联他人请求。
func generateRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败是系统级异常；退回空串让上层拒绝而不是用弱随机数冒充。
		return ""
	}
	return hex.EncodeToString(buf)
}
