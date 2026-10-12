package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/logger"
)

// CustomRecovery 捕获 panic：记结构化日志（含堆栈），返回统一 JSON 500。
//
// 两个刻意的决定：
//  1. 堆栈只进日志、不进响应——堆栈里有内部路径与变量值，属于信息泄漏；
//  2. 已经开始写响应时（未来是 SSE 流）不再改状态码，只能中断连接——
//     往半截流里再写一段 JSON 只会让客户端更难解析。
func CustomRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}

			logger.Error("请求处理 panic",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"route_tag", GetRouteTag(c),
				"request_id", GetRequestID(c),
			)

			if c.Writer.Written() {
				c.Abort()
				return
			}
			// /api 的信封（与 controller/response.go 的 APIResponse 同形）。
			// 这里手写而不是调用 controller：中间件不依赖 handler 层，避免成环。
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"message": "服务器内部错误",
				"data":    nil,
			})
		}()

		c.Next()
	}
}
