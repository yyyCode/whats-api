package model

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/setting/config"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

const statusMsgKey = "system_setting.status_message"

// TestMain 先装一个真实 logger 再跑测试，与 app 的启动顺序一致
// （InitEnv → SetupLogger → 读配置 → 之后才 SetLevel）。
//
// 必须有这一步：logger.SetLevel 的契约是"必须在 SetupLogger 之后调用才有效"——
// 在此之前 Logger() 返回的是 slog 默认 logger，它按自己的固定级别过滤，
// 于是"改 log_level 后 debug 立即可见"这条断言在没装 logger 的测试二进制里
// 永远为假。计划里的 Task 3 测试默认了这一点却没写出来，这里补上。
func TestMain(m *testing.M) {
	logger.SetupLogger("info", false)
	os.Exit(m.Run())
}

// resetSystemSetting 把一个测试对 system_setting 域的写入还回去。
//
// 与 common 的包级全局同理：UpdateOption 走 UpdateConfig，直接写 system_setting 的
// registered 结构体并重建原子快照——这是进程级状态，没人替它做清理。不清的话，
// 上一个测试写进去的值会成为下一个测试眼中的「默认值」：InitOptionMap 以
// ExportAllConfigs 为基准，而它读的正是这个注册结构体。逐字段写回 Default 即复位。
func resetSystemSetting(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		d := system_setting.Default
		for key, value := range map[string]string{
			"system_setting.log_level":                      common.MustMarshalString(d.LogLevel),
			"system_setting.status_message":                 common.MustMarshalString(d.StatusMessage),
			"system_setting.enable_access_log":              common.MustMarshalString(d.EnableAccessLog),
			"system_setting.slow_request_threshold_seconds": common.MustMarshalString(d.SlowRequestThresholdSeconds),
		} {
			if err := config.GlobalConfig.UpdateConfig(key, value); err != nil {
				t.Errorf("复位配置域 %q 失败: %v", key, err)
			}
		}
	})
}

func initOptions(t *testing.T) {
	t.Helper()
	if err := InitOptionMap(); err != nil {
		t.Fatalf("InitOptionMap 失败: %v", err)
	}
}

// 默认值来自注册结构体，不来自库——库里一开始是空的。
func TestInitOptionMapSeedsDefaults(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	if got := OptionMap[statusMsgKey]; got == "" {
		t.Fatalf("默认值未装载，OptionMap[%q] 为空：%v", statusMsgKey, OptionMap)
	}
	if got := system_setting.Get().StatusMessage; got != system_setting.Default.StatusMessage {
		t.Errorf("快照 = %q，期望默认 %q", got, system_setting.Default.StatusMessage)
	}
	if got := OptionMap["system_setting.log_level"]; got != `"info"` {
		t.Errorf("log_level 默认值 = %q，期望 \"info\"", got)
	}
}

// 库里的值覆盖默认值——这是"同一份代码多实例读到不同配置"的唯一来源。
func TestInitOptionMapAppliesDatabaseOverrides(t *testing.T) {
	withTestDB(t)

	// 模拟别的实例之前写过这一项
	if err := saveOption(statusMsgKey, `"库里已有的值"`); err != nil {
		t.Fatalf("saveOption 失败: %v", err)
	}

	initOptions(t)

	if got := system_setting.Get().StatusMessage; got != "库里已有的值" {
		t.Errorf("快照 = %q，期望库里的值", got)
	}
	if got := OptionMap[statusMsgKey]; got != `"库里已有的值"` {
		t.Errorf("OptionMap = %q，期望库里的值", got)
	}
}

// 库里的脏数据（比如降级安装后残留的 key）不该让服务起不来。
func TestInitOptionMapSkipsUnapplicableRows(t *testing.T) {
	withTestDB(t)
	if err := saveOption("system_setting.nonexistent", `"x"`); err != nil {
		t.Fatal(err)
	}
	if err := saveOption("system_setting.log_level", `"不是合法级别"`); err != nil {
		t.Fatal(err)
	}
	if err := saveOption(statusMsgKey, `"正常的"`); err != nil {
		t.Fatal(err)
	}

	if err := InitOptionMap(); err != nil {
		t.Fatalf("脏数据不该让装载失败: %v", err)
	}

	// 注意：这里**不是**断言 LogLevel 保持默认值。计划原文写的是
	// 「落不了的项应保持默认值」，但 M0 的校验只到类型（设计文档 §4.2：
	// 没有 min/max、没有枚举成员检查），log_level 是字符串字段，
	// 「不是合法级别」照样装得进去；而 logger.ParseLevel 的契约本就是
	// 「非法值回退 info」，即系统刻意容忍未知级别而不是拒收它。
	// 真正"应用不了"的只有未注册的 key 与类型不符的值（上面那条）。
	if got := system_setting.Get().LogLevel; got != "不是合法级别" {
		t.Errorf("类型合法的值应被应用（M0 无语义校验），得到 %q", got)
	}
	if got := system_setting.Get().StatusMessage; got != "正常的" {
		t.Errorf("正常的项应该生效，得到 %q", got)
	}
}

// ★ 单一路径：一次 UpdateOption 必须同时改到 内存 map、运行时快照、数据库 三处。
func TestUpdateOptionUpdatesAllThreePlaces(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	if err := UpdateOption(statusMsgKey, `"新消息"`); err != nil {
		t.Fatalf("UpdateOption 失败: %v", err)
	}

	if got := OptionMap[statusMsgKey]; got != `"新消息"` {
		t.Errorf("内存 map = %q，期望 %q", got, `"新消息"`)
	}
	if got := system_setting.Get().StatusMessage; got != "新消息" {
		t.Errorf("运行时快照 = %q，期望 新消息", got)
	}

	var row Option
	if err := DB.First(&row, "key = ?", statusMsgKey).Error; err != nil {
		t.Fatalf("库里没有这一行（重启后会回退）: %v", err)
	}
	if row.Value != `"新消息"` {
		t.Errorf("库里的值 = %q，期望 %q", row.Value, `"新消息"`)
	}

	// 再写一次同一个 key 应该是更新而不是插入失败（upsert）
	if err := UpdateOption(statusMsgKey, `"又改了"`); err != nil {
		t.Fatalf("第二次 UpdateOption 失败（upsert 有问题）: %v", err)
	}
	var count int64
	if err := DB.Model(&Option{}).Where("key = ?", statusMsgKey).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("同一 key 应只有一行，得到 %d 行", count)
	}
}

// 日志级别是 M0 唯一需要"非结构化副作用"的配置项。
func TestUpdateOptionAppliesLogLevelSideEffect(t *testing.T) {
	withTestDB(t)
	t.Cleanup(func() {
		logger.SetLevel(logger.ParseLevel("info"))
	})
	initOptions(t)

	if err := UpdateOption("system_setting.log_level", `"debug"`); err != nil {
		t.Fatalf("UpdateOption 失败: %v", err)
	}

	if !logger.Logger().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("改 log_level 后 debug 应立即可见（副作用没生效）")
	}
	if got := system_setting.Get().LogLevel; got != "debug" {
		t.Errorf("快照 log_level = %q，期望 debug", got)
	}
}

// ★ 非法输入必须一个字节都不写：库里的脏值会被别的实例同步过去，一处错变成处处错。
func TestUpdateOptionRejectsBadInputWithoutWriting(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	before := OptionMap[statusMsgKey]

	cases := []struct{ name, key, value string }{
		{"未注册的字段", "system_setting.nonexistent", `"x"`},
		{"未注册的模块", "nonexistent_setting.log_level", `"x"`},
		{"浮点项给字符串", "system_setting.slow_request_threshold_seconds", `"慢"`},
		{"布尔项给字符串", "system_setting.enable_access_log", `"yes"`},
		{"字符串项给数字", "system_setting.status_message", `123`},
		{"非法 JSON", "system_setting.status_message", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := UpdateOption(c.key, c.value); err == nil {
				t.Fatalf("UpdateOption(%q, %q) 应该报错", c.key, c.value)
			}
		})
	}

	var count int64
	for _, c := range cases {
		if err := DB.Model(&Option{}).Where("key = ?", c.key).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("失败的写入在库里留下了 %q 这一行（别的实例会被它污染）", c.key)
		}
	}
	if OptionMap[statusMsgKey] != before {
		t.Errorf("失败的写入改变了内存 map：%q → %q", before, OptionMap[statusMsgKey])
	}
	if system_setting.Get().StatusMessage != system_setting.Default.StatusMessage {
		t.Error("失败的写入改变了运行时快照")
	}
}

// 单实例热更新立即生效（验收标准③的单机那一半；多实例那一半由验收脚本覆盖）。
func TestUpdateOptionTakesEffectImmediately(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	if err := UpdateOption("system_setting.enable_access_log", `false`); err != nil {
		t.Fatal(err)
	}
	if system_setting.Get().EnableAccessLog {
		t.Error("改完应立即生效")
	}

	if err := UpdateOption("system_setting.slow_request_threshold_seconds", `1.5`); err != nil {
		t.Fatal(err)
	}
	if got := system_setting.Get().SlowRequestThresholdSeconds; got != 1.5 {
		t.Errorf("慢请求阈值 = %v，期望 1.5", got)
	}
}

// ★ 多实例语义：别的实例改了库，本实例在 SyncOptions 一轮后收敛。
func TestSyncConvergesAfterExternalChange(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	// 模拟"另一个实例"：直接写库，不经过本进程的内存
	if err := saveOption(statusMsgKey, `"来自另一个实例"`); err != nil {
		t.Fatal(err)
	}

	if got := system_setting.Get().StatusMessage; got == "来自另一个实例" {
		t.Fatal("未同步前不该看到别的实例写的新值（否则测的不是同步路径）")
	}

	applied, err := syncOptionsOnce()
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if applied != 1 {
		t.Errorf("本轮应恰好应用 1 项，得到 %d", applied)
	}
	if got := system_setting.Get().StatusMessage; got != "来自另一个实例" {
		t.Errorf("同步后 = %q，期望 来自另一个实例", got)
	}
}

// 全量重载但只应用"值真的变了"的项：不然每轮 SYNC_FREQUENCY 都在重建快照与刷日志。
func TestSyncSkipsUnchangedKeys(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	applied, err := syncOptionsOnce()
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Errorf("库里没有外部变更时不该应用任何项，得到 %d", applied)
	}

	// 变了才应用
	if err := saveOption("system_setting.log_level", `"warn"`); err != nil {
		t.Fatal(err)
	}
	applied, err = syncOptionsOnce()
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Errorf("有变更时应恰好应用 1 项，得到 %d", applied)
	}
	if got := system_setting.Get().LogLevel; got != "warn" {
		t.Errorf("快照 = %q，期望 warn", got)
	}
}

// 同步轮失败不该 panic，也不该把已装好的配置清掉。
func TestSyncToleratesDatabaseFailure(t *testing.T) {
	withTestDB(t)
	initOptions(t)

	if err := CloseDB(); err != nil {
		t.Fatal(err)
	}

	if _, err := syncOptionsOnce(); err == nil {
		t.Error("库关闭后同步应该报错（由调用方记日志，而不是 panic）")
	}
	if got := system_setting.Get().StatusMessage; got != system_setting.Default.StatusMessage {
		t.Errorf("同步失败不该丢掉已有配置，得到 %q", got)
	}
}
