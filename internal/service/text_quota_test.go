package service

import (
	"strconv"
	"strings"
	"testing"

	"github.com/yyyCode/whats-api/internal/setting/config"
)

const (
	modelRatiosKey = "ratio_setting.model_ratios"
	groupRatiosKey = "ratio_setting.group_ratios"
)

// restoreRatioConfig 记录配置项当前值并在测试结束后写回。
// config.GlobalConfig 是进程级单例，本包的测试共享它；改过的测试必须收尾，
// 否则后续用例会读到别的用例留下的倍率。
func restoreRatioConfig(t *testing.T, key string) {
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

// pricingFor 是测试里读取当前配置的便捷入口；生产代码应在请求开始时
// 调用一次并把结果冻结起来，而不是每次计算都重读。
func pricingFor(model, group string) TextPricing {
	return ResolveTextPricing(model, group)
}

// 结算必须使用调用方传入的冻结倍率，而不是结算时的最新配置：
// 请求飞行途中调价，历史账目仍应按开始时的价格复盘。
func TestCalculateTextQuotaUsesFrozenPricing(t *testing.T) {
	restoreRatioConfig(t, modelRatiosKey)

	frozen := pricingFor("gpt-4o", "default")
	if frozen.ModelRatio != 2.5 {
		t.Fatalf("测试前提失效：gpt-4o 当前倍率 = %v，期望 2.5", frozen.ModelRatio)
	}

	// 模拟请求开始后管理员把价格从 2.5 改成 1.0。
	if err := config.GlobalConfig.UpdateConfig(modelRatiosKey, strconv.Quote(`{"gpt-4o":1}`)); err != nil {
		t.Fatalf("更新 model_ratios 失败: %v", err)
	}

	got, detail := CalculateTextQuota(1_000_000, 0, frozen)
	if got != 1_250_000 { // 仍按 2.5 结算
		t.Errorf("用冻结倍率结算 = %d，期望 1250000", got)
	}
	if detail.ModelRatio != 2.5 {
		t.Errorf("detail.ModelRatio = %v，期望冻结值 2.5", detail.ModelRatio)
	}
}

// 负 token 不可能算出正金额，任何调用方都不该让一次请求变成免费。
func TestCalculateTextQuotaClampsNegativeTokens(t *testing.T) {
	got, detail := CalculateTextQuota(-100, 10, pricingFor("gpt-4o", "default"))
	if got <= 0 {
		t.Errorf("负输入 token 的结算 = %d，期望 > 0", got)
	}
	if detail.PromptTokens != 0 {
		t.Errorf("detail.PromptTokens = %d，期望钳制为 0", detail.PromptTokens)
	}
}

func TestEstimatePreConsumeQuotaFormula(t *testing.T) {
	// gpt-4o = 2.5 美元/百万输入 token；预扣系数 1.2；QuotaPerUnit = 500000。
	// 1,000,000 × 2.5 × 1.2 × 500000 / 1e6 = 1,500,000 quota。
	const prompt = 1_000_000

	got, detail, err := EstimatePreConsumeQuota("gpt-4o", prompt, "default")
	if err != nil {
		t.Fatalf("EstimatePreConsumeQuota 返回错误: %v", err)
	}
	if got != 1_500_000 {
		t.Errorf("预扣额度 = %d，期望 1500000", got)
	}
	if detail.PreConsumedQuota != got {
		t.Errorf("detail.PreConsumedQuota = %d，期望与返回值一致 %d", detail.PreConsumedQuota, got)
	}
	if detail.PromptTokens != prompt {
		t.Errorf("detail.PromptTokens = %d，期望 %d", detail.PromptTokens, prompt)
	}
	if detail.CompletionTokens != 0 {
		t.Errorf("预扣阶段 detail.CompletionTokens = %d，期望 0", detail.CompletionTokens)
	}
	if detail.ModelRatio != 2.5 || detail.CompletionRatio != 4 ||
		detail.GroupRatio != 1 || detail.PreConsumeMultiplier != 1.2 {
		t.Errorf("detail 倍率 = (%v, %v, %v, %v)，期望 (2.5, 4, 1, 1.2)",
			detail.ModelRatio, detail.CompletionRatio, detail.GroupRatio, detail.PreConsumeMultiplier)
	}
	if detail.QuotaClamp != 0 {
		t.Errorf("正常金额不应有饱和标记，得到 %v", detail.QuotaClamp)
	}
}

func TestCalculateTextQuotaFormula(t *testing.T) {
	const prompt = 1_000_000

	// 只有输入：1,000,000 × 2.5 × 500000 / 1e6 = 1,250,000。
	got, detail := CalculateTextQuota(prompt, 0, pricingFor("gpt-4o", "default"))
	if got != 1_250_000 {
		t.Errorf("结算额度（无输出）= %d，期望 1250000", got)
	}
	if detail.ActualQuota != got {
		t.Errorf("detail.ActualQuota = %d，期望 %d", detail.ActualQuota, got)
	}
	if detail.PromptTokens != prompt || detail.CompletionTokens != 0 {
		t.Errorf("detail tokens = (%d, %d)，期望 (%d, 0)",
			detail.PromptTokens, detail.CompletionTokens, prompt)
	}

	// 输出 1,000,000 token，completionRatio=4：
	// (1e6 + 1e6×4) × 2.5 × 500000 / 1e6 = 6,250,000。
	got, detail = CalculateTextQuota(prompt, prompt, pricingFor("gpt-4o", "default"))
	if got != 6_250_000 {
		t.Errorf("结算额度（含输出）= %d，期望 6250000", got)
	}
	if detail.CompletionRatio != 4 || detail.ModelRatio != 2.5 ||
		detail.GroupRatio != 1 || detail.PreConsumeMultiplier != 1.2 {
		t.Errorf("detail 倍率 = (%v, %v, %v, %v)，期望 (2.5, 4, 1, 1.2)",
			detail.ModelRatio, detail.CompletionRatio, detail.GroupRatio, detail.PreConsumeMultiplier)
	}
	if detail.CompletionTokens != prompt {
		t.Errorf("detail.CompletionTokens = %d，期望 %d", detail.CompletionTokens, prompt)
	}
}

func TestQuotaUnknownModelUsesDefaults(t *testing.T) {
	// 未配置的模型：默认输入倍率 1、输出倍数 4、分组倍数 1。
	const prompt = 1_000_000

	pre, preDetail, err := EstimatePreConsumeQuota("no-such-model", prompt, "default")
	if err != nil {
		t.Fatalf("EstimatePreConsumeQuota 返回错误: %v", err)
	}
	if pre != 600_000 { // 1e6 × 1 × 1.2 × 0.5
		t.Errorf("未知模型预扣 = %d，期望 600000", pre)
	}
	if preDetail.ModelRatio != 1 || preDetail.CompletionRatio != 4 || preDetail.GroupRatio != 1 {
		t.Errorf("未知模型 detail 倍率 = (%v, %v, %v)，期望 (1, 4, 1)",
			preDetail.ModelRatio, preDetail.CompletionRatio, preDetail.GroupRatio)
	}

	actual, detail := CalculateTextQuota(prompt, prompt, pricingFor("no-such-model", "default"))
	// (1e6 + 1e6×4) × 1 × 0.5 = 2,500,000。
	if actual != 2_500_000 {
		t.Errorf("未知模型结算 = %d，期望 2500000", actual)
	}
	if detail.ModelRatio != 1 || detail.CompletionRatio != 4 {
		t.Errorf("未知模型结算 detail 倍率 = (%v, %v)，期望 (1, 4)",
			detail.ModelRatio, detail.CompletionRatio)
	}
}

func TestQuotaUsesGroupRatio(t *testing.T) {
	restoreRatioConfig(t, groupRatiosKey)

	if err := config.GlobalConfig.UpdateConfig(groupRatiosKey, strconv.Quote(`{"vip":2}`)); err != nil {
		t.Fatalf("更新 group_ratios 失败: %v", err)
	}

	const prompt = 1_000_000

	// 分组倍数 2：预扣 1,500,000 × 2；结算（无输出）1,250,000 × 2。
	pre, preDetail, err := EstimatePreConsumeQuota("gpt-4o", prompt, "vip")
	if err != nil {
		t.Fatalf("EstimatePreConsumeQuota 返回错误: %v", err)
	}
	if pre != 3_000_000 {
		t.Errorf("vip 分组预扣 = %d，期望 3000000", pre)
	}
	if preDetail.GroupRatio != 2 {
		t.Errorf("预扣 detail.GroupRatio = %v，期望 2", preDetail.GroupRatio)
	}

	actual, detail := CalculateTextQuota(prompt, 0, pricingFor("gpt-4o", "vip"))
	if actual != 2_500_000 {
		t.Errorf("vip 分组结算 = %d，期望 2500000", actual)
	}
	if detail.GroupRatio != 2 {
		t.Errorf("结算 detail.GroupRatio = %v，期望 2", detail.GroupRatio)
	}
}

func TestQuotaTinyAmountBumpsToOne(t *testing.T) {
	// gpt-4o-mini = 0.15 美元/百万 token，单 token 原始预扣远小于 1 quota；
	// 取整为 0 会让便宜模型被无限白嫖，所以必须抬到 1。
	got, detail, err := EstimatePreConsumeQuota("gpt-4o-mini", 1, "default")
	if err != nil {
		t.Fatalf("EstimatePreConsumeQuota 返回错误: %v", err)
	}
	if got != 1 {
		t.Errorf("极小金额预扣 = %d，期望抬到 1", got)
	}
	if detail.PreConsumedQuota != 1 {
		t.Errorf("detail.PreConsumedQuota = %d，期望 1", detail.PreConsumedQuota)
	}

	// 结算同样有下限：0.15 × 0.5 × prompt=1 → 0.075。
	actual, settleDetail := CalculateTextQuota(1, 0, pricingFor("gpt-4o-mini", "default"))
	if actual != 1 {
		t.Errorf("极小金额结算 = %d，期望抬到 1", actual)
	}
	if settleDetail.ActualQuota != 1 {
		t.Errorf("detail.ActualQuota = %d，期望 1", settleDetail.ActualQuota)
	}
}

func TestQuotaZeroRatioStaysZero(t *testing.T) {
	restoreRatioConfig(t, modelRatiosKey)

	// 显式配置为 0 表示免费；此时原始金额就是 0，不能被「抬到 1」规则误伤。
	if err := config.GlobalConfig.UpdateConfig(modelRatiosKey, strconv.Quote(`{"free":0}`)); err != nil {
		t.Fatalf("更新 model_ratios 失败: %v", err)
	}

	pre, preDetail, err := EstimatePreConsumeQuota("free", 1_000_000, "default")
	if err != nil {
		t.Fatalf("EstimatePreConsumeQuota 返回错误: %v", err)
	}
	if pre != 0 || preDetail.PreConsumedQuota != 0 {
		t.Errorf("免费模型预扣 = (%d, detail %d)，期望 0", pre, preDetail.PreConsumedQuota)
	}
	if preDetail.ModelRatio != 0 {
		t.Errorf("免费模型 detail.ModelRatio = %v，期望 0", preDetail.ModelRatio)
	}

	actual, settleDetail := CalculateTextQuota(1_000_000, 1_000_000, pricingFor("free", "default"))
	if actual != 0 || settleDetail.ActualQuota != 0 {
		t.Errorf("免费模型结算 = (%d, detail %d)，期望 0", actual, settleDetail.ActualQuota)
	}
	if settleDetail.ModelRatio != 0 {
		t.Errorf("免费模型结算 detail.ModelRatio = %v，期望 0", settleDetail.ModelRatio)
	}
}

func TestEstimatePromptTokensEnglish(t *testing.T) {
	// "hello world" = 11 runes → ceil(11/4) = 3；每消息 +4；末尾 +3。
	body := []byte(`{"messages":[{"content":"hello world"}]}`)
	if got := EstimatePromptTokens(body); got != 10 {
		t.Errorf("英文 prompt 估算 = %d，期望 10", got)
	}
}

func TestEstimatePromptTokensChinese(t *testing.T) {
	// "你好世界" = 4 runes → ceil(4/4) = 1；4 + 1 + 3 = 8。
	// 按字节数会算成 12 bytes → 3，中文必须按 rune 计才不会系统性低估。
	body := []byte(`{"messages":[{"content":"你好世界"}]}`)
	if got := EstimatePromptTokens(body); got != 8 {
		t.Errorf("中文 prompt 估算 = %d，期望 8", got)
	}
}

func TestEstimatePromptTokensMultimodalIgnoresImageSize(t *testing.T) {
	// 文本 "hello" = 5 runes → ceil(5/4) = 2；4 + 2 + 3 = 9。
	// image_url 的 base64 再长也不能影响结果：图像 token 成本与字节数无关。
	makeBody := func(payload string) []byte {
		return []byte(`{"messages":[{"content":[` +
			`{"type":"text","text":"hello"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + payload + `"}}` +
			`]}]}`)
	}

	small := EstimatePromptTokens(makeBody("AAAA"))
	huge := EstimatePromptTokens(makeBody(strings.Repeat("A", 200_000)))

	if small != 9 {
		t.Errorf("多模态 prompt 估算 = %d，期望 9", small)
	}
	if huge != small {
		t.Errorf("图片 base64 变长后估算从 %d 变成 %d——图片载荷不应计入 token", small, huge)
	}
}

func TestEstimatePromptTokensMalformedFallback(t *testing.T) {
	// "not json" = 8 runes → ceil(8/4) = 2。
	body := []byte("not json")
	if got := EstimatePromptTokens(body); got != 2 {
		t.Errorf("坏 JSON 的兜底估算 = %d，期望 2", got)
	}
}

func TestEstimatePromptTokensEmptyBody(t *testing.T) {
	// 空请求体算不出内容，但结果不能是 0：0 token 在计费语义里等于免费。
	if got := EstimatePromptTokens(nil); got != 1 {
		t.Errorf("空 body 估算 = %d，期望 1", got)
	}
	if got := EstimatePromptTokens([]byte{}); got != 1 {
		t.Errorf("空字节估算 = %d，期望 1", got)
	}
}
