package router

import (
	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/controller"
)

// setAPIRouter 注册管理面。
//
// M0 只有探活接口：管理面的 CRUD 是 M3 的事。
// 分组先建好，M3 往这里加 user/token/channel/log 路由即可。
func setAPIRouter(engine *gin.Engine, deps controller.Deps) {
	api := engine.Group("/api")

	api.GET("/status", controller.Status(deps))
}
