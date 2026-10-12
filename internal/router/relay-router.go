package router

import (
	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/logger"
)

// setRelayRouter 预留给转发面。
//
// ★ M0 在这里**不注册任何端点**——严格照 M0 范围（不做转发）。
// 这个函数存在的意义是把"两个面从一开始就分开"这条决策固化成代码形状：
// M1 直接往这个组里加 handler（token 鉴权 → 选渠道 → 转发 → 计费），
// 不需要那时再回过头重构路由结构。
//
// 注意它什么都不注册，所以这里不能写 `engine.Group("/v1")` 然后丢掉返回值——
// 那是个没有任何作用的语句。分组的创建留给 M1，那时它才有内容。
//
// engine 参数眼下用不到（Go 对未使用的**函数参数**不报错）：保留它是为了 M1
// 往这个组注册 handler 时不必改 SetRouter 的调用点。
// 这是本计划里唯一一处"当前为空"的接缝——别的地方都按 §6.1 的纪律不留死代码。
func setRelayRouter(engine *gin.Engine) {
	logger.Debug("转发面已预留（M0 不注册端点，M1 加入 /v1/chat/completions）")
}
