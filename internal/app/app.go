// Package app 是进程的生命周期：启动时序、HTTP 服务、优雅停机。
//
// 为什么把启动做成 Run(ctx) 而不是写在 main() 里：
// 停机路径只有能被"直接取消 ctx"驱动，才是可测试的。
// 本机（Windows）无法向原生进程投递 SIGTERM，把信号处理留在 main.go 做一层薄壳，
// 这里就能真实测到"在途请求跑完 + DB 关闭 + 后台 goroutine 退出"。
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/yyyCode/whats-api/internal/cachex"
	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	"github.com/yyyCode/whats-api/internal/router"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

// requestsCounterTTL 是请求计数器的滚动窗口。
// 用 24 小时而不是进程生命周期：重启后计数不该从 0 开始"假装没流量"，
// 但也要有个终点的，否则键永远不失效。
const requestsCounterTTL = 24 * time.Hour

// Options 是 RunWithOptions 的可注入项。
type Options struct {
	// InjectRoutes 在 SetRouter 之后追加路由。
	//
	// 存在的唯一理由是测试：验收标准④要求"在途请求跑完"，而 M0 没有任何
	// 慢到能观测到停机排水的生产端点。生产代码永远不传它。
	InjectRoutes func(*gin.Engine)
}

// Run 启动服务直到 ctx 被取消，然后优雅停机。
func Run(ctx context.Context) error { return RunWithOptions(ctx, Options{}) }

// RunWithOptions 是 Run 的可注入版本。
func RunWithOptions(ctx context.Context, opts Options) error {
	// ---------- ① 环境变量与日志 ----------
	// godotenv.Load 只补"进程环境里还没有"的变量，因此容器里注入的环境变量优先，
	// .env 只是本地开发的便利。
	dotenvErr := godotenv.Load(".env")

	common.InitEnv()
	logger.SetupLogger(common.LogLevel, false)

	if dotenvErr != nil {
		logger.Debug("未加载 .env 文件，使用进程环境变量", "error", dotenvErr)
	}
	// InitEnv 跑在 logger 之前，告警先攒着，现在统一输出。
	for _, warning := range common.EnvWarnings {
		logger.Warn(warning)
	}

	logger.Info("启动 whats-api",
		"version", common.Version,
		"node_type", common.NodeType,
		"is_master", common.IsMasterNode,
		"port", common.Port,
		"sync_frequency_seconds", common.SyncFrequency,
		"shutdown_timeout_seconds", common.ShutdownTimeout,
	)

	// ---------- ② 数据层：唯一的致命初始化 ----------
	if err := model.InitDB(); err != nil {
		return err
	}
	if err := model.CheckSetup(); err != nil {
		return err
	}

	// ---------- ③ 配置必须在任何读配置的逻辑之前 ----------
	if err := model.InitOptionMap(); err != nil {
		return err
	}
	// 库里的配置可能覆盖了启动期的日志级别，这里对齐一次。
	// （InitOptionMap 里 applySideEffects 已经设过，这里是防御性的二次确认。）
	logger.SetLevel(logger.ParseLevel(system_setting.Get().LogLevel))

	// ---------- ④ 日志库（LOG_SQL_DSN 为空则复用主库）----------
	if err := model.InitLogDB(); err != nil {
		return err
	}

	// ---------- ⑤ 缓存：非致命，失败降级 ----------
	common.InitRedisClient()

	// ---------- ⑥ 后台任务 ----------
	// 全部接收 runCtx：取消它们就开始退出，停机时不会被写进已关闭的数据库。
	//
	// ★ 用从 ctx 派生的 runCtx，而不是直接用 ctx：
	// 服务可能不是因为收到了退出信号而结束，而是 ListenAndServe 直接失败返回
	// （例如端口被占用）。这时上层 ctx 并没有被取消，若后台 goroutine 只监听 ctx，
	// wg.Wait() 会永久阻塞，进程卡死在停机里。runCtx 让"任何一条退出路径"都能
	// 停掉后台任务。
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		model.SyncOptions(runCtx, time.Duration(common.SyncFrequency)*time.Second)
	}()

	// ---------- ⑦ HTTP ----------
	requests := cachex.NewCounter("requests", requestsCounterTTL)

	gin.SetMode(ginMode(common.GinMode))
	engine := gin.New()
	router.SetRouter(engine, router.Deps{Requests: requests, StartedAt: time.Now()})
	if opts.InjectRoutes != nil {
		opts.InjectRoutes(engine)
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", common.Port),
		Handler: engine,
		// ★ 刻意不设 ReadTimeout / WriteTimeout：
		// 流式响应（SSE）可能持续几分钟，写超时会把正常的流掐断。
		// 用 ReadHeaderTimeout 挡住慢速攻击，同时不影响响应时长。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务已启动", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// ---------- ⑧ 等待退出条件 ----------
	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停机", "timeout_seconds", common.ShutdownTimeout)
	case err := <-serveErr:
		runErr = err
		if err == nil {
			logger.Info("HTTP 服务已自行退出")
		} else {
			logger.Error("HTTP 服务异常退出", "error", err)
		}
	}

	// ---------- ⑨ 优雅停机 ----------
	// 用独立的 ctx：此时上层 ctx 已经取消，不能再拿它当超时来源。
	shutdownCtx, cancelShutdown := context.WithTimeout(
		context.Background(), time.Duration(common.ShutdownTimeout)*time.Second)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 超时不等于出错：可能是长流还没结束。记错误但继续收尾。
		logger.Error("优雅停机超时，仍有在途请求被中断", "error", err)
	}

	// ★ 保证后台任务一定收到取消（serveErr 分支下 ctx 仍存活）。
	// 等它们退出**再**关库：反过来会让它们写到已关闭的连接上。
	cancelRun()
	wg.Wait()

	if err := model.CloseDB(); err != nil {
		logger.Warn("关闭数据库出错", "error", err)
	}
	if err := common.CloseRedis(); err != nil {
		logger.Warn("关闭 Redis 出错", "error", err)
	}

	logger.Info("已退出")
	return runErr
}

// ginMode 把环境变量映射成 gin 模式，未知值回退 release
// （回退到 debug 会让 gin 打印一堆警告，回退到 test 会静默一些错误日志）。
func ginMode(mode string) string {
	switch mode {
	case gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		return mode
	default:
		return gin.ReleaseMode
	}
}
