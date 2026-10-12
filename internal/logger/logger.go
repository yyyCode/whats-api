// Package logger 是全项目的结构化日志入口（标准库 log/slog）。
//
// 日志级别可以在运行时更新（配置热更新的一条副作用），所以级别存在
// atomic 里，由自定义的 slog.Leveler 在每次判断时读取——而不是在
// SetupLogger 时把级别固化进 HandlerOptions。
//
// ★ 调用顺序：SetupLogger 必须在启动流程里**先于**任何 SetLevel 与任何配置读取。
//
// 原因是 SetupLogger 会用 LOG_LEVEL 重写级别。若配置域在 SetupLogger 之前
// 就读到了 system_setting.log_level 并调了 SetLevel，那条设置会被随后的
// SetupLogger 静默覆盖——现象是"改了日志级别没反应"，而日志里没有任何线索
// 说明它被谁改回去了。启动流程的正确顺序是：
//
//	InitEnv → SetupLogger(LOG_LEVEL) → 读配置 → 之后才开始 SetLevel
//
// 在此之前调用 SetLevel 也不会报错（它只写一个 atomic），但那次设置
// 对输出没有任何影响：Logger() 这时返回的是 slog 默认 logger，
// 它按自己的级别过滤。
package logger

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

var (
	current atomic.Pointer[slog.Logger]
	level   atomic.Int32 // 存 slog.Level
)

// dynamicLevel 让 Handler 每次判断是否输出时都读当前级别。
type dynamicLevel struct{}

func (dynamicLevel) Level() slog.Level { return slog.Level(level.Load()) }

// SetupLogger 建初始 logger。level 来自环境变量 LOG_LEVEL，
// 之后可由配置项 system_setting.log_level 覆盖。
// jsonFormat 为 false 时输出便于人读的文本格式（本地开发）。
//
// **它会用 levelName 重写当前级别**，所以必须在读配置之前调用一次，
// 且整个进程只调用一次。详见包注释里的调用顺序。
func SetupLogger(levelName string, jsonFormat bool) {
	level.Store(int32(ParseLevel(levelName)))

	opts := &slog.HandlerOptions{Level: dynamicLevel{}}
	var handler slog.Handler
	if jsonFormat {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	current.Store(slog.New(handler))
}

// SetLevel 运行时更新日志级别（配置热更新的副作用走这里）。
//
// 必须在 SetupLogger 之后调用：SetupLogger 会覆盖这里设的值。
func SetLevel(lv slog.Level) { level.Store(int32(lv)) }

// ParseLevel 解析 debug/info/warn/error，非法值回退 info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Logger 返回当前 logger。SetupLogger 之前调用会退回 slog 默认 logger，
// 而不是返回 nil——初始化早期打日志是很正常的事。
func Logger() *slog.Logger {
	if l := current.Load(); l != nil {
		return l
	}
	return slog.Default()
}

func Debug(msg string, args ...any) { Logger().Debug(msg, args...) }
func Info(msg string, args ...any)  { Logger().Info(msg, args...) }
func Warn(msg string, args ...any)  { Logger().Warn(msg, args...) }
func Error(msg string, args ...any) { Logger().Error(msg, args...) }
