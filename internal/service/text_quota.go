package service

import (
	"fmt"
	"unicode/utf8"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/setting/ratio_setting"
)

// QuotaDetail 是一次计价的完整上下文。
//
// 它必须记录**实际使用**的每一个倍率，而不是事后再去读配置：
// 配置可以热更新，请求结束后再读会得到另一版倍率，日志就无法复盘
// 「这笔钱当时是怎么算出来的」。
type QuotaDetail struct {
	ModelRatio           float64
	CompletionRatio      float64
	GroupRatio           float64
	PreConsumeMultiplier float64
	PromptTokens         int
	CompletionTokens     int
	PreConsumedQuota     int
	ActualQuota          int
	QuotaClamp           common.QuotaClamp
}

// EstimatePromptTokens 估算请求体的 prompt token 数。
//
// 这是预扣阶段的粗略估计，上游返回真实 usage 后会用实际值结算，
// 所以目标不是精确，而是「不低估、且与内容长度成比例」。规则：
//   - 解析 {"messages":[{"content":...}]}；
//   - 每条消息固定计 4 token（role/name/分隔符等 JSON 结构开销的统一代表，
//     不逐字段解析——估得太细并不会更准，反而让规则难以解释）；
//   - 字符串内容按 rune 数计，ceil(runes/4) 近似 BPE 的「约 4 字符 1 token」；
//   - 数组内容（多模态）只累计 type=="text" 的 text 字段。图片的 base64
//     长度由分辨率/压缩率决定，与图像 token 成本无关，按字节折算会估出
//     几十万 token，导致正常请求因预扣不足被拒；
//   - 全部消息之后 +3（assistant 起始等固定开销）；
//   - 整体无法解析时回退 ceil(runeCount(body)/4)。
//
// 任何路径的结果都不小于 1：0 token 的预扣在用量为 0 时与「免费」无法区分，
// 而计费链路把 0 用量当作异常信号。
func EstimatePromptTokens(body []byte) int {
	// 用 any 接收 content：JSON 字符串解成 string、数组解成 []any，
	// 这样只依赖 common.Unmarshal，不必直接引 encoding/json 的 RawMessage。
	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := common.Unmarshal(body, &req); err != nil {
		return fallbackPromptTokens(body)
	}

	total := 3
	for _, msg := range req.Messages {
		runes, ok := contentRuneCount(msg.Content)
		if !ok {
			// content 出现了无法理解的结构（例如数字/对象）。与其按 0 低估、
			// 让预扣不足，不如退回到按整个请求体长度估算。
			return fallbackPromptTokens(body)
		}
		total += 4 + (runes+3)/4 // +3 后再除 4 等价于向上取整
	}
	if total < 1 {
		total = 1
	}
	return total
}

// contentRuneCount 返回一条消息内容里应计入 token 的 rune 数。
// 第二个返回值是 false 表示内容形状无法识别，调用方应整体回退。
func contentRuneCount(content any) (int, bool) {
	switch v := content.(type) {
	case nil:
		// content 缺失或为 null：仍然保留每条消息的 4 token 固定开销。
		return 0, true
	case string:
		return utf8.RuneCountInString(v), true
	case []any:
		total := 0
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return 0, false
			}
			typ, _ := part["type"].(string)
			// 只统计文本片段；image_url 等载荷直接忽略。
			if typ != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				total += utf8.RuneCountInString(text)
			}
		}
		return total, true
	default:
		return 0, false
	}
}

// fallbackPromptTokens 是整体解析失败时的兜底估算。
func fallbackPromptTokens(body []byte) int {
	n := (utf8.RuneCount(body) + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

// EstimatePreConsumeQuota 计算请求开始前的预扣额度。
//
// 公式（modelRatio 的单位是「美元 / 100 万输入 token」）：
//
//	prompt × modelRatio × preConsumeMultiplier × QuotaPerUnit / 1e6 × groupRatio
//
// 只预扣输入：输出长度在调用前未知，靠 preConsumeMultiplier 留出安全余量，
// 结算时再按实际 usage 补扣或退款。预扣用 Strict 取整——溢出/NaN 时
// 宁可拒绝请求，也不能按一个错误的金额冻结用户余额。
func EstimatePreConsumeQuota(model string, promptTokens int, group string) (int, QuotaDetail, error) {
	modelRatio := ratio_setting.ModelRatio(model)
	groupRatio := ratio_setting.GroupRatio(group)
	multiplier := ratio_setting.PreConsumeMultiplier()

	detail := QuotaDetail{
		ModelRatio:           modelRatio,
		CompletionRatio:      ratio_setting.CompletionRatio(model),
		GroupRatio:           groupRatio,
		PreConsumeMultiplier: multiplier,
		PromptTokens:         promptTokens,
	}

	preRaw := float64(promptTokens) * modelRatio * multiplier *
		float64(common.QuotaPerUnit) / 1e6 * groupRatio

	quota, err := common.QuotaRoundStrict(preRaw)
	if err != nil {
		// Strict 只返回错误、不返回饱和类型；这里再取一次 Checked 的结果，
		// 只是为了让调用方能区分 overflow/NaN 并写进日志。
		_, detail.QuotaClamp = common.QuotaRoundChecked(preRaw)
		return 0, detail, fmt.Errorf("预扣金额计算失败: %w", err)
	}

	quota = bumpToMinimumQuota(quota, preRaw)
	detail.PreConsumedQuota = quota
	return quota, detail, nil
}

// CalculateTextQuota 按实际 usage 计算本次请求的结算额度。
//
// 公式：
//
//	(prompt + completion × completionRatio) × modelRatio × QuotaPerUnit / 1e6 × groupRatio
//
// 结算用 Checked：数值异常时饱和并在 detail.QuotaClamp 里留痕，
// 由调用方写日志；这里不能返回 error，因为上游请求已经执行完，
// 结算金额再异常也必须走完记账流程，不能把异常响应回给客户端。
func CalculateTextQuota(model string, promptTokens, completionTokens int, group string) (int, QuotaDetail) {
	modelRatio := ratio_setting.ModelRatio(model)
	completionRatio := ratio_setting.CompletionRatio(model)
	groupRatio := ratio_setting.GroupRatio(group)

	detail := QuotaDetail{
		ModelRatio:           modelRatio,
		CompletionRatio:      completionRatio,
		GroupRatio:           groupRatio,
		PreConsumeMultiplier: ratio_setting.PreConsumeMultiplier(),
		PromptTokens:         promptTokens,
		CompletionTokens:     completionTokens,
	}

	settleRaw := (float64(promptTokens) + float64(completionTokens)*completionRatio) *
		modelRatio * float64(common.QuotaPerUnit) / 1e6 * groupRatio

	quota, clamp := common.QuotaRoundChecked(settleRaw)
	detail.QuotaClamp = clamp
	quota = bumpToMinimumQuota(quota, settleRaw)
	detail.ActualQuota = quota
	return quota, detail
}

// bumpToMinimumQuota 把「原始值 > 0、但取整后为 0」的金额抬到 1。
//
// 便宜的模型（例如 0.15 美元/百万 token）单次请求的金额远小于 1 quota，
// 四舍五入后是 0；如果不抬到 1，同一账号就能无限并发白嫖。
// 反之，显式把倍率配置成 0 的免费模型原始值就是 0，必须保持 0，
// 所以判断条件是「原始值 > 0」而不是只看结果是否为 0。
func bumpToMinimumQuota(quota int, raw float64) int {
	if quota == 0 && raw > 0 {
		return 1
	}
	return quota
}
