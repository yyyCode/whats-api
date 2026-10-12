package logger

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" info ", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"bogus", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("ParseLevel(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// 配置项 system_setting.log_level 改完必须立即生效，不需要重启。
func TestSetLevelTakesEffectImmediately(t *testing.T) {
	SetupLogger("info", false)

	if Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("info 级别下 debug 不该启用")
	}
	if !Logger().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info 级别下 info 应启用")
	}

	SetLevel(ParseLevel("debug"))
	if !Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("改成 debug 后 debug 应启用")
	}

	SetLevel(ParseLevel("error"))
	if Logger().Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("改成 error 后 warn 不该启用")
	}
}

// SetupLogger 之前就有人打日志时不能 panic。
func TestLoggerBeforeSetupDoesNotPanic(t *testing.T) {
	Info("这条日志发生在 SetupLogger 之前")
	if Logger() == nil {
		t.Fatal("Logger() 不应返回 nil")
	}
}

// 钉住调用顺序契约：SetupLogger 会用 LOG_LEVEL 覆盖此前 SetLevel 设的值。
//
// 启动流程若把"读配置"放在 SetupLogger 之前，配置里的 log_level 就会被
// 静默吃掉，现象是"改了日志级别没反应"而日志里毫无线索。这条测试让
// 覆盖语义一旦变化就地报警，也说明为什么顺序不能反。
func TestSetupLoggerOverridesEarlierSetLevel(t *testing.T) {
	SetLevel(slog.LevelDebug)  // 假装配置域先跑了一步
	SetupLogger("info", false) // 启动流程随后才建 logger

	if Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("SetupLogger 应覆盖此前 SetLevel 设的级别（这正是顺序不能反的原因）")
	}
	if !Logger().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("SetupLogger 之后应按 LOG_LEVEL 生效")
	}

	// SetupLogger 之后再 SetLevel，才是唯一有效的用法。
	SetLevel(slog.LevelDebug)
	if !Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("SetupLogger 之后的 SetLevel 应生效")
	}
}
