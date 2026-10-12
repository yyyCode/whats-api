package router

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/controller"
)

// setWebRouter 注册兜底路由。
//
// 兜底必须是 JSON 404，而不是"所有未知路径都返回前端 HTML"：
// 对于 API 路径，被吞成 200 + HTML 是极难排查的一类故障
// （SDK 报"解析响应失败"，而不是"404 路径不存在"）。
// 前端 SPA 兜底在 M3 引入时，也只对**明确的静态资源与非 API 路径**生效。
func setWebRouter(engine *gin.Engine) {
	engine.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path

		switch {
		case path == "/v1" || strings.HasPrefix(path, "/v1/"):
			// 转发面的错误形状与 /api 不同：将来是 OpenAI/Claude 形状的 error 对象。
			// M0 先给出同形状的明确 JSON，让客户端不必猜。
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"message": "转发端点尚未实现（计划在 M1）",
					"type":    "invalid_request_error",
					"code":    "not_implemented",
				},
			})

		case path == "/api" || strings.HasPrefix(path, "/api/"):
			controller.APIError(c, http.StatusNotFound, "接口不存在")

		case strings.HasPrefix(path, "/assets/"):
			controller.APIError(c, http.StatusNotFound, "静态资源不存在")

		default:
			controller.APIError(c, http.StatusNotFound, "前端资源尚未构建（计划在 M3）")
		}
	})
}
