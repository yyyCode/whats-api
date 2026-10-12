package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// 三个面：转发面 / 管理面 / 其他（静态资源与 SPA）。
const (
	RouteTagRelay = "relay"
	RouteTagAPI   = "api"
	RouteTagWeb   = "web"

	// RouteTagKey 是 gin.Context 里的键名。
	RouteTagKey = "route_tag"
)

// RouteTag 按路径前缀给请求打面标签，供访问日志按面聚合。
//
// 为什么用全局中间件而不是挂在路由组上：M0 还没有真正的 /v1 端点，
// 按组挂会得到一个没有 handler 的组、也拿不到标签。
// M1 有了 /v1 handler 后，若需要更细的标签（如 /v1/tasks 与 /v1/chat 分开），
// 改成按组挂即可——AccessLog 读的是同一个 context key，不用动。
func RouteTag() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tagForPath(c.Request.URL.Path))
		c.Next()
	}
}

func tagForPath(path string) string {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return RouteTagRelay
	case path == "/api" || strings.HasPrefix(path, "/api/"):
		return RouteTagAPI
	default:
		return RouteTagWeb
	}
}

// GetRouteTag 读取当前请求的面标签。
func GetRouteTag(c *gin.Context) string {
	if v, ok := c.Get(RouteTagKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return RouteTagWeb
}
