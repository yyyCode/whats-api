package service

import (
	"fmt"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

// BuildConsumeLog 构造一条消费日志。
//
// 日志必须能独立复盘「这笔钱是怎么算出来的」：用了哪组倍率、估算与实际的
// token 数、预扣了多少、最终结算多少、是否发生数值饱和。所以计价要素全部
// 从 detail 里取（那是本次请求实际使用的快照），**不在写日志时再读配置**——
// 配置可以热更新，事后再读会得到另一版倍率，历史账目就对不上了。
//
// ip 由调用方从 gin.Context 取：service 层不引入 gin，保持计费逻辑与 HTTP 框架无关。
func BuildConsumeLog(info *relaycommon.RelayInfo, usage *types.Usage, quota int, detail QuotaDetail, ip string) *model.Log {
	promptTokens, completionTokens := usageTokens(usage)

	l := newBillingLog(info, detail, ip, "upstream", nil)
	l.Type = constant.LogTypeConsume
	l.Quota = int64(quota)
	l.PromptTokens = promptTokens
	l.CompletionTokens = completionTokens
	l.Content = fmt.Sprintf("模型 %s，输入 %d tokens，输出 %d tokens，消费 %d quota",
		fieldModelName(info), promptTokens, completionTokens, quota)
	return l
}

// BuildBillingErrorLog 构造一条计费错误日志（上游无 usage、重试全部失败等）。
//
// 这类请求不产生消费：Quota 恒为 0，并且 other 里明确记 refunded=true，
// 让对账时能区分「退了预扣」与「本来就没扣」。usage_source 固定为 missing，
// 因为当前唯一的计费错误来源就是拿不到可用 usage。
func BuildBillingErrorLog(info *relaycommon.RelayInfo, reason string, detail QuotaDetail, ip string) *model.Log {
	l := newBillingLog(info, detail, ip, "missing", map[string]any{
		"refunded": true,
		"reason":   reason,
	})
	l.Type = constant.LogTypeError
	l.Quota = 0
	l.Content = reason
	return l
}

// newBillingLog 填两类日志共有的请求元数据与计价上下文。
// usageSource 与 extra 由调用方区分消费/错误语义。
func newBillingLog(info *relaycommon.RelayInfo, detail QuotaDetail, ip, usageSource string, extra map[string]any) *model.Log {
	l := &model.Log{
		ModelName: fieldModelName(info),
		ChannelID: fieldChannelID(info),
		GroupName: fieldGroup(info),
		RequestID: fieldRequestID(info),
		IP:        ip,
		IsStream:  fieldIsStream(info),
		UseTime:   requestUseTime(info),
		Other:     marshalBillingOther(info, detail, usageSource, extra),
	}
	if info != nil {
		// UserID 不在本函数的入参里，但审计日志必须能回答「谁被扣了钱」。
		l.UserID = info.Request.UserID
	}
	return l
}

// marshalBillingOther 把计费上下文序列化成 other 字段。
//
// 键名是审计格式的一部分（下游对账脚本会按名取值），不要随意改名。
// 序列化失败只可能是出现不可序列化的值，此时返回一个固定标记而不是空串：
// 空串会被误读为「这条日志没有上下文」，标记则明确表示上下文丢失。
func marshalBillingOther(info *relaycommon.RelayInfo, detail QuotaDetail, usageSource string, extra map[string]any) string {
	// 预扣阶段的估算 token 与冻结金额在选渠道之前就已确定，它们是「当时为什么
	// 扣了这笔钱」的唯一证据，所以优先取请求级快照。detail 是结算时用真实 usage
	// 重新算出的上下文，PromptTokens 是实际输入量而不是预扣估算值；错误日志的
	// detail 甚至没有 usage，只能退回快照。0 表示快照缺失（例如仅构造日志的调用），
	// 此时才回退到 detail，保持与既有日志格式兼容。
	estimatedPromptTokens := detail.PromptTokens
	preConsumedQuota := detail.PreConsumedQuota
	if info != nil {
		if info.Request.Billing.EstimatedPromptTokens != 0 {
			estimatedPromptTokens = info.Request.Billing.EstimatedPromptTokens
		}
		if preConsumedQuota == 0 {
			preConsumedQuota = info.Request.Billing.PreConsumedQuota
		}
	}

	other := map[string]any{
		"billing_mode":            "ratio",
		"model_ratio":             detail.ModelRatio,
		"completion_ratio":        detail.CompletionRatio,
		"group_ratio":             detail.GroupRatio,
		"pre_consume_multiplier":  detail.PreConsumeMultiplier,
		"estimated_prompt_tokens": estimatedPromptTokens,
		"prompt_tokens":           detail.PromptTokens,
		"completion_tokens":       detail.CompletionTokens,
		"pre_consumed":            preConsumedQuota,
		"actual":                  detail.ActualQuota,
		"quota_saturation":        detail.QuotaClamp.String(),
		"upstream_model":          fieldUpstreamModel(info),
		"retry_index":             fieldRetryIndex(info),
		"upstream_status":         fieldUpstreamStatus(info),
		"usage_source":            usageSource,
		"request_id":              fieldRequestID(info),
		"channel_id":              fieldChannelID(info),
		"channel_name":            fieldChannelName(info),
		"is_stream":               fieldIsStream(info),
	}
	for k, v := range extra {
		other[k] = v
	}

	data, err := common.Marshal(other)
	if err != nil {
		logger.Error("序列化计费日志上下文失败", "error", err)
		return `{"error":"marshal_failed"}`
	}
	return string(data)
}

// usageTokens 容错地取出 usage 的 token 数：调用方在正常路径会先做 Valid 判断，
// 但日志构造不应因为一个 nil 就 panic、连带丢掉整条审计记录。
func usageTokens(usage *types.Usage) (promptTokens, completionTokens int) {
	if usage == nil {
		return 0, 0
	}
	return usage.PromptTokens, usage.CompletionTokens
}

// requestUseTime 返回请求耗时（秒），四舍五入到秒之前先截断。
// StartTime 为零值（例如未经过 GenRelayInfo 的调用）时返回 0，而不是
// time.Since(零值) 得到的巨大数字——那会把日志变成一个假信号。
func requestUseTime(info *relaycommon.RelayInfo) int {
	if info == nil || info.Request.StartTime.IsZero() {
		return 0
	}
	elapsed := time.Since(info.Request.StartTime)
	if elapsed <= 0 {
		return 0
	}
	return int(elapsed.Seconds())
}

// 以下字段访问器只为容忍 nil info（日志构造不应让请求路径 panic）。
func fieldRequestID(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return info.Request.RequestID
}

func fieldModelName(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return info.Request.OriginModelName
}

func fieldGroup(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return info.Request.Group
}

func fieldIsStream(info *relaycommon.RelayInfo) bool {
	return info != nil && info.Request.IsStream
}

func fieldRetryIndex(info *relaycommon.RelayInfo) int {
	if info == nil {
		return 0
	}
	return info.Request.RetryIndex
}

func fieldChannelID(info *relaycommon.RelayInfo) int {
	if info == nil {
		return 0
	}
	return info.Attempt.ChannelID
}

func fieldChannelName(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return info.Attempt.ChannelName
}

func fieldUpstreamModel(info *relaycommon.RelayInfo) string {
	if info == nil {
		return ""
	}
	return info.Attempt.UpstreamModelName
}

func fieldUpstreamStatus(info *relaycommon.RelayInfo) int {
	if info == nil {
		return 0
	}
	return info.Attempt.UpstreamStatus
}
