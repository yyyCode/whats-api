package router

import (
	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/controller"
	"github.com/yyyCode/whats-api/internal/middleware"
)

// setRelayRouter 注册转发面。
//
// 鉴权挂在组上而不是全局：管理面（/api）将来是另一套身份（会话），
// 两者的错误形状也不同（这里必须保持 OpenAI 形状）。
func setRelayRouter(engine *gin.Engine) {
	v1 := engine.Group("/v1")
	v1.Use(middleware.TokenAuth())
	v1.POST("/chat/completions", controller.Relay)
}
