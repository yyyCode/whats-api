package ratio_setting

import (
	"strconv"
	"testing"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

const (
	modelRatiosKey      = "ratio_setting.model_ratios"
	completionRatiosKey = "ratio_setting.completion_ratios"
	groupRatiosKey      = "ratio_setting.group_ratios"
)

// restoreConfig 记录配置项当前的 JSON 值，并在测试结束后写回。
// config.GlobalConfig 是进程级单例，本包任何一次 UpdateConfig 都会影响
// 后面的测试，所以改过配置的测试必须用它收尾。
func restoreConfig(t *testing.T, key string) {
	t.Helper()

	orig, ok := config.GlobalConfig.GetConfig(key)
	if !ok {
		t.Fatalf("配置项 %q 未注册", key)
	}
	t.Cleanup(func() {
		if err := config.GlobalConfig.UpdateConfig(key, orig); err != nil {
			t.Errorf("恢复配置项 %q 失败: %v", key, err)
		}
	})
}

func TestDefaultsParse(t *testing.T) {
	snap := Get()
	if snap == nil {
		t.Fatal("初始快照不应为 nil")
	}

	if got := snap.ModelRatio("gpt-4o"); got != 2.5 {
		t.Errorf("gpt-4o 输入倍率 = %v，期望 2.5", got)
	}
	if got := snap.ModelRatio("gpt-4o-mini"); got != 0.15 {
		t.Errorf("gpt-4o-mini 输入倍率 = %v，期望 0.15", got)
	}
	if got := snap.CompletionRatio("gpt-4o"); got != 4 {
		t.Errorf("gpt-4o 输出倍数 = %v，期望 4", got)
	}
	if got := snap.PreConsumeMultiplier(); got != 1.2 {
		t.Errorf("预扣倍数 = %v，期望 1.2", got)
	}

	// 包级函数必须与快照读到同一份数据（它们是计费代码实际调用的入口）。
	if got := ModelRatio("gpt-4o"); got != 2.5 {
		t.Errorf("包级 ModelRatio(gpt-4o) = %v，期望 2.5", got)
	}
	if got := CompletionRatio("gpt-4o"); got != 4 {
		t.Errorf("包级 CompletionRatio(gpt-4o) = %v，期望 4", got)
	}
	if got := PreConsumeMultiplier(); got != 1.2 {
		t.Errorf("包级 PreConsumeMultiplier() = %v，期望 1.2", got)
	}
}

func TestUnknownModelAndGroupFallBack(t *testing.T) {
	if got := ModelRatio("no-such-model"); got != Default.DefaultModelRatio {
		t.Errorf("未配置模型的输入倍率 = %v，期望兜底值 %v", got, Default.DefaultModelRatio)
	}
	if got := CompletionRatio("no-such-model"); got != Default.DefaultCompletionRatio {
		t.Errorf("未配置模型的输出倍数 = %v，期望兜底值 %v", got, Default.DefaultCompletionRatio)
	}
	if got := GroupRatio("no-such-group"); got != 1 {
		t.Errorf("未配置分组的分组倍数 = %v，期望 1", got)
	}
}

// 写坏一张表时，OnRebuild 必须整体保留旧快照：
// 一次配置手误不该让服务带着半张新表、半张旧表继续计费。
func TestInvalidJSONKeepsPreviousSnapshot(t *testing.T) {
	restoreConfig(t, modelRatiosKey)

	before := Get()
	if before == nil {
		t.Fatal("初始快照不应为 nil")
	}

	// UpdateConfig 只校验字段类型：字符串字段写任意文本都会成功，
	// 倍率表本身的合法性由 OnRebuild 负责。
	if err := config.GlobalConfig.UpdateConfig(modelRatiosKey, strconv.Quote("不是 JSON")); err != nil {
		t.Fatalf("UpdateConfig 写入字符串失败: %v", err)
	}

	after := Get()
	if after != before {
		t.Fatal("非法 JSON 不应发布新快照（旧快照必须原样保留）")
	}
	if got := ModelRatio("gpt-4o"); got != 2.5 {
		t.Errorf("非法 JSON 后 gpt-4o 倍率 = %v，期望仍是 2.5", got)
	}
}

// 负倍率不是「便宜」，而是坏配置：它会让结算金额变成 0 或负数，
// 必须与语法错误一样整体拒绝，保留上一份快照。
func TestNegativeRatioKeepsPreviousSnapshot(t *testing.T) {
	restoreConfig(t, modelRatiosKey)

	before := Get()
	if err := config.GlobalConfig.UpdateConfig(
		modelRatiosKey, strconv.Quote(`{"gpt-4o":-2.5}`)); err != nil {
		t.Fatalf("UpdateConfig 写入字符串失败: %v", err)
	}

	after := Get()
	if after != before {
		t.Fatal("负倍率不应发布新快照（旧快照必须原样保留）")
	}
	if got := ModelRatio("gpt-4o"); got != 2.5 {
		t.Errorf("负倍率被拒后 gpt-4o 倍率 = %v，期望仍是 2.5", got)
	}
}

// 合法 JSON 写入后，下一次读取就必须看到新值。
func TestValidJSONUpdatesSnapshot(t *testing.T) {
	restoreConfig(t, modelRatiosKey)
	restoreConfig(t, completionRatiosKey)
	restoreConfig(t, groupRatiosKey)

	before := Get()

	if err := config.GlobalConfig.UpdateConfig(
		modelRatiosKey, strconv.Quote(`{"gpt-4o":9.5,"free-model":0}`)); err != nil {
		t.Fatalf("更新 model_ratios 失败: %v", err)
	}
	if err := config.GlobalConfig.UpdateConfig(
		completionRatiosKey, strconv.Quote(`{"gpt-4o":2}`)); err != nil {
		t.Fatalf("更新 completion_ratios 失败: %v", err)
	}
	if err := config.GlobalConfig.UpdateConfig(
		groupRatiosKey, strconv.Quote(`{"vip":0.8}`)); err != nil {
		t.Fatalf("更新 group_ratios 失败: %v", err)
	}

	after := Get()
	if after == before {
		t.Error("合法更新后应发布新的快照指针")
	}
	if got := ModelRatio("gpt-4o"); got != 9.5 {
		t.Errorf("更新后 gpt-4o 输入倍率 = %v，期望 9.5", got)
	}
	if got := CompletionRatio("gpt-4o"); got != 2 {
		t.Errorf("更新后 gpt-4o 输出倍数 = %v，期望 2", got)
	}
	if got := GroupRatio("vip"); got != 0.8 {
		t.Errorf("更新后 vip 分组倍数 = %v，期望 0.8", got)
	}
	// 显式配置为 0 表示免费，不能因为零值被当成「未配置」而回退到默认值。
	if got := ModelRatio("free-model"); got != 0 {
		t.Errorf("显式配置为 0 的模型倍率 = %v，期望 0", got)
	}
	// 新表里没有的模型改走兜底值，而不是沿用旧表。
	if got := ModelRatio("gpt-4o-mini"); got != Default.DefaultModelRatio {
		t.Errorf("新表未配置的模型倍率 = %v，期望兜底值 %v", got, Default.DefaultModelRatio)
	}
}

func TestNonPositivePreConsumeMultiplierFallsBack(t *testing.T) {
	const key = "ratio_setting.input_pre_consume_multiplier"
	restoreConfig(t, key)

	for _, raw := range []string{"0", "-0.5"} {
		if err := config.GlobalConfig.UpdateConfig(key, raw); err != nil {
			t.Fatalf("更新预扣倍数失败: %v", err)
		}
		if got := PreConsumeMultiplier(); got != 1 {
			t.Errorf("预扣倍数配置为 %s 时读取值 = %v，期望回退到 1", raw, got)
		}
	}
}
