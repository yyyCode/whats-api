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
