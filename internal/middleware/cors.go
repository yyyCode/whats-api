package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
)

// CORS 按 CORS_ALLOW_ORIGINS 处理跨域。
//
// M0 不设置 Access-Control-Allow-Credentials：鉴权在 M1 引入，
// 届时若要用 Cookie 承载会话，必须改成"回显具体 Origin"——
// 通配符 * 与 credentials 在浏览器里不能共存，混用会让请求被静默拒绝。
func CORS() gin.HandlerFunc {
	allowAll := len(common.CORSAllowOrigins) == 1 && common.CORSAllowOrigins[0] == "*"
	allowed := make(map[string]bool, len(common.CORSAllowOrigins))
	for _, origin := range common.CORSAllowOrigins {
		allowed[origin] = true
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		switch {
		case origin == "":
			// 非浏览器请求（curl / SDK / 探活）：没有 Origin，不需要也不该有 CORS 头
		case allowAll:
			c.Header("Access-Control-Allow-Origin", "*")
		case allowed[origin]:
			c.Header("Access-Control-Allow-Origin", origin)
			// 按来源变化的响应必须声明 Vary，否则中间缓存会把 A 源的响应发给 B 源
			c.Header("Vary", "Origin")
		}

		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			c.Header("Access-Control-Max-Age", "86400")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
