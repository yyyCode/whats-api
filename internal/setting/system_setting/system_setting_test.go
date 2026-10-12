package system_setting

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

func TestDefaults(t *testing.T) {
	got := Get()
	if got.LogLevel != "info" {
		t.Errorf("LogLevel = %q，期望 info", got.LogLevel)
	}
	if !got.EnableAccessLog {
		t.Error("EnableAccessLog 默认应为 true")
	}
	if got.SlowRequestThresholdSeconds != 3 {
		t.Errorf("SlowRequestThresholdSeconds = %v，期望 3", got.SlowRequestThresholdSeconds)
	}
	if got.StatusMessage == "" {
		t.Error("StatusMessage 默认不应为空")
	}
}

// 注册表里必须能导出这一域的默认值——这是 Option 表默认值的来源。
func TestRegisteredInGlobalConfig(t *testing.T) {
	all := config.GlobalConfig.ExportAllConfigs()
	for _, key := range []string{
		"system_setting.log_level",
		"system_setting.status_message",
		"system_setting.enable_access_log",
		"system_setting.slow_request_threshold_seconds",
	} {
		if _, ok := all[key]; !ok {
			t.Errorf("缺少配置项 %q；现有：%v", key, all)
		}
	}
}

// 改配置后热路径必须立刻看到新值（验收标准③的单元测试那一半）。
func TestUpdatePublishesNewSnapshot(t *testing.T) {
	before := Get()

	if err := config.GlobalConfig.UpdateConfig("system_setting.status_message", `"改过了"`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}

	after := Get()
	if after.StatusMessage != "改过了" {
		t.Errorf("StatusMessage = %q，期望 改过了", after.StatusMessage)
	}
	// 快照必须是**新对象**：旧快照不可被就地修改，否则读侧就白做了。
	if before == after {
		t.Fatal("更新后应发布新的快照指针，而不是就地改旧对象")
	}
	if before.StatusMessage == "改过了" {
		t.Error("旧快照被就地修改了——不可变快照的约定被破坏")
	}
	if after.EnableAccessLog != Default.EnableAccessLog {
		t.Error("未修改的字段不应被连带改动")
	}

	// 恢复默认值，避免污染同包其他测试（TestConcurrentReadDuringUpdates
	// 只接受默认值或 msg-N，见 plan 里同款清理约定）。
	if err := config.GlobalConfig.UpdateConfig(
		"system_setting.status_message", strconv.Quote(Default.StatusMessage)); err != nil {
		t.Fatal(err)
	}
}

// 布尔与浮点也要能热更新。
func TestUpdateBooleanAndFloat(t *testing.T) {
	if err := config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `false`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if Get().EnableAccessLog {
		t.Error("EnableAccessLog 应为 false")
	}

	if err := config.GlobalConfig.UpdateConfig("system_setting.slow_request_threshold_seconds", `1.25`); err != nil {
		t.Fatalf("UpdateConfig 失败: %v", err)
	}
	if got := Get().SlowRequestThresholdSeconds; got != 1.25 {
		t.Errorf("慢请求阈值 = %v，期望 1.25", got)
	}

	// 恢复默认值，避免影响同包其他测试与后续断言
	if err := config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `true`); err != nil {
		t.Fatal(err)
	}
	if err := config.GlobalConfig.UpdateConfig("system_setting.slow_request_threshold_seconds", `3`); err != nil {
		t.Fatal(err)
	}
}

// 这条测试是原子快照契约的核心：并发读 + 并发写，且必须用 -race 跑。
//
// 断言的不是"没有 race"（那是 -race 的活），而是"读者读到的永远是已发布的完整版本"：
// 任何一次读到的 status_message 都必须是默认值或某个完整写过的 msg-N，
// 绝不允许出现空串或半截字符串。
func TestConcurrentReadDuringUpdates(t *testing.T) {
	const iterations = 200
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			msg := strconv.Quote(fmt.Sprintf("msg-%d", i))
			if err := config.GlobalConfig.UpdateConfig("system_setting.status_message", msg); err != nil {
				t.Errorf("UpdateConfig 失败: %v", err)
				return
			}
		}
	}()

	isPublished := func(s string) bool {
		return s == Default.StatusMessage || strings.HasPrefix(s, "msg-")
	}
	for i := 0; i < iterations; i++ {
		snap := Get()
		if snap == nil {
			t.Fatal("快照不应为 nil")
		}
		if !isPublished(snap.StatusMessage) {
			t.Fatalf("读到未发布的值 %q，说明存在中间态可见", snap.StatusMessage)
		}
	}

	<-done

	// 收尾：恢复默认，避免污染后续测试
	if err := config.GlobalConfig.UpdateConfig(
		"system_setting.status_message", strconv.Quote(Default.StatusMessage)); err != nil {
		t.Fatal(err)
	}
}
