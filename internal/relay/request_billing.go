// 本文件把计费状态机挂到一次转发请求上：预扣 → 结算或退款。
//
// ★ 挂载顺序是正确性的一部分：
//  1. 选渠道之前必须预扣成功——否则并发请求可以先拿到渠道、再用同一个余额
//     去上游消费，余额不足也照样出流量；
//  2. 请求结束的 defer 兜底退款，成功路径用结算抢先推进状态机；
//     BillingSession 的幂等状态机保证两者只有一个真正执行。
package relay

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/service"
	"github.com/yyyCode/whats-api/internal/types"
)

// PrepareRequestBilling 估算输入 token 并预扣额度。
//
// 这一步必须发生在 SelectChannel 之前：预扣失败时一个上游请求都不该发出，
// 更不能因为"先把渠道占下来"而让额度不足的请求消耗上游资源。
//
// 返回的 session 在预扣失败时为 nil；调用方用非 nil 判断是否需要挂 defer。
func PrepareRequestBilling(c *gin.Context, info *relaycommon.RelayInfo) (*service.BillingSession, *types.APIError) {
	promptTokens := service.EstimatePromptTokens(info.Request.Body)
	quota, detail, err := service.EstimatePreConsumeQuota(
		info.Request.OriginModelName, promptTokens, info.Request.Group)
	if err != nil {
		// 倍率配置异常（溢出/NaN）属于网关自身的问题：拒绝请求，
		// 而不是按一个错误金额冻结用户余额。
		logger.Error("计算预扣额度失败",
			"request_id", info.Request.RequestID,
			"model", info.Request.OriginModelName,
			"error", err)
		return nil, types.NewInternalError("预扣额度失败")
	}

	// 先把快照写进 RequestState 再预扣：即使后续预扣失败，日志/错误处理
	// 也能拿到本次请求实际使用的倍率。
	info.Request.Billing = relaycommon.BillingMeta{
		EstimatedPromptTokens: promptTokens,
		PreConsumedQuota:      quota,
		ModelRatio:            detail.ModelRatio,
		CompletionRatio:       detail.CompletionRatio,
		GroupRatio:            detail.GroupRatio,
		PreConsumeMultiplier:  detail.PreConsumeMultiplier,
	}

	session := service.NewBillingSession(
		info.Request.UserID, info.Request.TokenID, info.Request.TokenUnlimited)
	preConsumed, err := session.PreConsume(quota)
	if err != nil {
		if errors.Is(err, service.ErrInsufficientQuota) {
			// 余额不足是用户可修正的状态：403 + insufficient_quota，
			// 与鉴权中间件对令牌额度用尽的响应保持同一形状。
			return nil, types.NewQuotaError("额度不足，请充值后再试")
		}
		logger.Error("预扣额度失败",
			"request_id", info.Request.RequestID,
			"user_id", info.Request.UserID,
			"token_id", info.Request.TokenID,
			"quota", quota,
			"error", err)
		return nil, types.NewInternalError("预扣额度失败")
	}
	// 预扣金额以状态机的实际结果为准（免费模型为 0），日志据此复盘。
	info.Request.Billing.PreConsumedQuota = preConsumed
	return session, nil
}

// SettleRequestBilling 在请求成功（或流正常结束）后按真实 usage 结算。
//
// 没有可用的 usage 时退款并写错误日志，而不是按 0 计费：
// 零用量是上游/网关的异常信号，按 0 计费等于免费放行，
// 而按估算值计费会让没有用量的请求被多扣钱——两者都不可接受。
func SettleRequestBilling(c *gin.Context, s *service.BillingSession, info *relaycommon.RelayInfo) {
	if s == nil {
		return
	}

	usage := info.Attempt.Usage
	if usage == nil || !usage.Valid() {
		if _, err := s.Refund(); err != nil {
			// 退款暂时失败：会话仍是 preConsumed，defer 会重试。
			// 这里不写审计日志——否则 defer 退款成功后还会再写一条，
			// 同一次请求出现两条互相矛盾的「已退款」记录。
			// 把原因放进 LastError，让 defer 写出的那条日志带上上下文。
			logger.Error("退还预扣额度失败",
				"request_id", info.Request.RequestID,
				"user_id", info.Request.UserID,
				"token_id", info.Request.TokenID,
				"error", err)
			info.Request.LastError = types.NewUpstreamError(
				http.StatusBadGateway, "上游未返回 usage，退款失败")
			return
		}

		const reason = "上游未返回 usage，已退还预扣"
		if err := model.WriteLog(service.BuildBillingErrorLog(
			info, reason, billingDetail(info), true, c.ClientIP())); err != nil {
			logger.Error("写入计费错误日志失败",
				"request_id", info.Request.RequestID,
				"error", err)
		}
		logger.Warn("上游未返回 usage，已退还预扣",
			"request_id", info.Request.RequestID,
			"user_id", info.Request.UserID,
			"channel_id", info.Attempt.ChannelID,
			"model", info.Request.OriginModelName,
			"pre_consumed", s.PreConsumed())
		return
	}

	// ★ 用请求开始时冻结的倍率结算，而不是再读一次配置：
	// 请求飞行途中配置可能被热更新，用新倍率结算会让金额与日志里
	// 记录的预扣依据（同一组倍率）对不上，历史账目无法复盘。
	quota, detail := service.CalculateTextQuota(
		usage.PromptTokens, usage.CompletionTokens, billingPricing(info))
	// 日志里的 pre_consumed 必须来自状态机的实际预扣，而不是重新估算：
	// 两者在免费模型/取整边界上可能不同。
	detail.PreConsumedQuota = s.PreConsumed()

	settled, err := s.Settle(quota)
	if err != nil {
		// ★ 上游已经执行、响应也已经写给客户端，这里不能再改 HTTP 响应；
		// 只能把账本失败留在日志里，由对账/告警接手。
		logger.Error("结算额度失败",
			"request_id", info.Request.RequestID,
			"user_id", info.Request.UserID,
			"token_id", info.Request.TokenID,
			"quota", quota,
			"error", err)
		return
	}
	if !settled {
		// 已经结算/退款过（例如重复挂载）：幂等 no-op，不是故障。
		return
	}

	if err := model.WriteLog(service.BuildConsumeLog(
		info, usage, quota, detail, c.ClientIP())); err != nil {
		// 日志是审计凭证，写入失败必须显式告警；但不能因此改变已经确定的账本结果。
		logger.Error("写入消费日志失败",
			"request_id", info.Request.RequestID,
			"user_id", info.Request.UserID,
			"quota", quota,
			"error", err)
	}
}

// RefundFailedRequestBilling 是请求结束时的兜底退款。
//
// 只有仍在 preConsumed 状态才会真正退款：成功路径已经由 Settle 推进到
// settled，这个 defer 就是幂等的 no-op。退款成功后写一条错误日志，
// 并带上最后一个上游错误，便于区分「没打到上游」与「上游执行失败」。
func RefundFailedRequestBilling(c *gin.Context, s *service.BillingSession, info *relaycommon.RelayInfo) {
	if s == nil {
		return
	}

	refunded, err := s.Refund()
	if err != nil {
		// 退款失败意味着预扣仍冻结在账上，这是需要人工对账的状态，
		// 必须留下一条明确写着 refunded=false 的审计记录。
		logger.Error("退还预扣额度失败",
			"request_id", info.Request.RequestID,
			"user_id", info.Request.UserID,
			"token_id", info.Request.TokenID,
			"error", err)
		reason := "退款失败，预扣仍冻结：" + err.Error()
		if info.Request.LastError != nil {
			reason += "；最后错误：" + info.Request.LastError.Message
		}
		if writeErr := model.WriteLog(service.BuildBillingErrorLog(
			info, reason, billingDetail(info), false, c.ClientIP())); writeErr != nil {
			logger.Error("写入计费错误日志失败",
				"request_id", info.Request.RequestID,
				"error", writeErr)
		}
		return
	}
	if !refunded {
		// 已经结算（或已退款）：说明这次请求不是失败收尾，不写错误日志。
		return
	}

	reason := "请求失败，已退还预扣"
	if info.Request.LastError != nil {
		reason += "：" + info.Request.LastError.Message
	}
	if err := model.WriteLog(service.BuildBillingErrorLog(
		info, reason, billingDetail(info), true, c.ClientIP())); err != nil {
		logger.Error("写入计费错误日志失败",
			"request_id", info.Request.RequestID,
			"error", err)
	}

	logger.Info("请求失败，预扣额度已退还",
		"request_id", info.Request.RequestID,
		"user_id", info.Request.UserID,
		"channel_id", info.Attempt.ChannelID,
		"model", info.Request.OriginModelName,
		"pre_consumed", s.PreConsumed())
}

// billingPricing 取请求开始时冻结的倍率快照。
//
// 正常路径上 PrepareRequestBilling 一定先执行并填好 Billing；
// EstimatedPromptTokens 为 0 只可能出现在直接调用 SettleRequestBilling
// 的测试/内部路径，此时退回当前配置，保持行为可用。
func billingPricing(info *relaycommon.RelayInfo) service.TextPricing {
	b := info.Request.Billing
	if b.EstimatedPromptTokens == 0 {
		return service.ResolveTextPricing(info.Request.OriginModelName, info.Request.Group)
	}
	return service.TextPricing{
		ModelRatio:           b.ModelRatio,
		CompletionRatio:      b.CompletionRatio,
		GroupRatio:           b.GroupRatio,
		PreConsumeMultiplier: b.PreConsumeMultiplier,
	}
}

// billingDetail 从请求级快照构造错误日志的计价上下文。
//
// 失败路径上没有真实 usage，ActualQuota 保持 0；PromptTokens 用预扣时的估算值，
// 这样错误日志里仍能看出「这笔预扣是基于多少 token 算出来的」。
func billingDetail(info *relaycommon.RelayInfo) service.QuotaDetail {
	b := info.Request.Billing
	return service.QuotaDetail{
		ModelRatio:           b.ModelRatio,
		CompletionRatio:      b.CompletionRatio,
		GroupRatio:           b.GroupRatio,
		PreConsumeMultiplier: b.PreConsumeMultiplier,
		PromptTokens:         b.EstimatedPromptTokens,
		PreConsumedQuota:     b.PreConsumedQuota,
	}
}
