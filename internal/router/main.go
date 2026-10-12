// Package router 是路由注册层。
//
// 三层分离（照 02 篇）：/api 管理面、/v1 转发面、以及兜底的 web/NoRoute。
// 分离的意义不只是整洁：两个面的错误形状、中间件、鉴权方式、SLA 都不同，
// 混在一个组里，将来给 /v1 加鉴权就会误伤管理面。
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/cachex"
	"github.com/yyyCode/whats-api/internal/controller"
	"github.com/yyyCode/whats-api/internal/middleware"
)

// Deps 是路由层需要的进程级依赖。
type Deps struct {
	// Requests 是请求总数计数器（/api/status 暴露）。
	Requests *cachex.Counter
	// StartedAt 是进程启动时间，用来算 uptime。
	StartedAt time.Time
}

// SetRouter 注册全部路由与全局中间件。
//
// 中间件顺序是有讲究的（设计文档 §7.1）：
// recovery 最外层（panic 也要被日志与统一响应覆盖）→ request-id（后续所有日志都要带它）
// → version → route-tag → access-log（此时前几层都注入了，日志才完整）→ cors（最内层，预检可提前 abort）。
func SetRouter(engine *gin.Engine, deps Deps) {
	// 不用 gin.Default()：它自带的 Logger 会往 stderr 打一份非结构化日志，
	// 与自研访问日志重复；用 gin.New() + 自己的中间件。
	engine.Use(
		middleware.CustomRecovery(),
		middleware.RequestId(),
		middleware.Version(),
		middleware.RouteTag(),
		middleware.AccessLog(deps.Requests),
		middleware.CORS(),
	)

	// ★ 只在非 nil 时赋值：controller.Deps.Requests 是 Counter 接口，
	// 把一个 nil 的 *cachex.Counter 赋进去会让接口本身非 nil（类型已知、值为 nil），
	// 于是 buildStatus 里的 `deps.Requests != nil` 判不住，调用 Get 直接崩。
	cdeps := controller.Deps{StartedAt: deps.StartedAt}
	if deps.Requests != nil {
		cdeps.Requests = deps.Requests
	}

	setAPIRouter(engine, cdeps)
	setRelayRouter(engine)
	setWebRouter(engine)
}
