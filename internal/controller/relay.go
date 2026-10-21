// Package controller 的转发主循环在选渠道之前挂载请求级计费：
// 预扣成功才允许选路；成功后按真实 usage 结算；失败或无 usage 时退款并写错误日志。
package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/middleware"
	relay "github.com/yyyCode/whats-api/internal/relay"
	"github.com/yyyCode/whats-api/internal/relay/channel"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/relay/helper"
	"github.com/yyyCode/whats-api/internal/service"
	"github.com/yyyCode/whats-api/internal/types"
)

const (
	// maxRequestBodyBytes：多模态请求里的 base64 图片可以很大，
	// 32MB 是 OpenAI 同量级的上限。
	maxRequestBodyBytes = 32 << 20
	// maxRetryTimes 是换渠道的次数上限：总共最多 3 次尝试。
	// 再多只是在故障时放大负载——反模式清单里明确禁止"什么错误都重试"。
	maxRetryTimes = 2
)

// Relay 是转发主循环：解析 → 预扣 → 选渠道 → 转换 → 上游 → 响应 → 结算/退款；
// 失败时换渠道重试。计费状态跨重试共享，最终只会结算或退款一次。
func Relay(c *gin.Context) {
	auth := relaycommon.GetAuthInfo(c)
	if auth == nil {
		types.NewAuthenticationError("缺少鉴权信息").Write(c)
		return
	}

	body, apiErr := readRequestBody(c)
	if apiErr != nil {
		apiErr.Write(c)
		return
	}

	var payload relaycommon.RequestPayload
	if err := common.Unmarshal(body, &payload); err != nil {
		types.NewBadRequest("请求体必须是合法 JSON").Write(c)
		return
	}
	if strings.TrimSpace(payload.Model) == "" {
		types.NewBadRequest("缺少 model 字段").Write(c)
		return
	}
	if !modelAllowed(auth, payload.Model) {
		types.NewPermissionError(fmt.Sprintf("API Key 不允许使用模型 %q", payload.Model)).Write(c)
		return
	}

	info := relaycommon.GenRelayInfo(auth, &payload, middleware.GetRequestID(c), body)

	// ★ 预扣必须在选渠道之前：额度不足的请求不应占用任何上游渠道，
	// 否则并发请求可以拿同一份余额去上游消费，账本失去意义。
	billing, billingErr := relay.PrepareRequestBilling(c, info)
	if billingErr != nil {
		billingErr.Write(c)
		return
	}
	// 兜底退款：成功路径由 SettleRequestBilling 把状态机推进到 settled，
	// 这里的 defer 就成了幂等 no-op；任何提前 return 都不会把预扣留在账上。
	defer relay.RefundFailedRequestBilling(c, billing, info)

	tried := make(map[int]bool, maxRetryTimes+1)
	var lastErr *types.APIError

	for attempt := 0; attempt <= maxRetryTimes; attempt++ {
		info.Request.RetryIndex = attempt

		ch, selectErr := service.SelectChannel(info.Request.Group, info.Request.OriginModelName, tried)
		if selectErr != nil {
			lastErr = selectErr
			break
		}
		tried[ch.ID] = true

		upstreamModel, mapped := helper.MapModel(info.Request.OriginModelName, ch.ModelMapping)
		info.ResetAttempt(relaycommon.ChannelMeta{
			ChannelID:         ch.ID,
			ChannelType:       ch.Type,
			ChannelName:       fmt.Sprintf("channel#%d", ch.ID),
			BaseURL:           ch.BaseURL,
			APIKey:            firstAPIKey(ch.Key),
			UpstreamModelName: upstreamModel,
			ModelMapped:       mapped,
		})

		adaptor := relay.GetAdaptor(ch.Type)
		if adaptor == nil {
			lastErr = types.NewInternalError(fmt.Sprintf("渠道类型 %d 尚未支持", ch.Type))
			logger.Error("渠道类型没有适配器", "channel_id", ch.ID, "channel_type", ch.Type)
			break
		}
		adaptor.Init(info)

		lastErr = relayAttempt(c, info, adaptor)
		if lastErr == nil {
			// 先结算再 return：defer 里的退款会看到 settled 状态并变成 no-op，
			// 从而保证一次请求只产生一笔账。
			relay.SettleRequestBilling(c, billing, info)
			return
		}
		info.Request.LastError = lastErr

		logger.Warn("上游尝试失败",
			"request_id", info.Request.RequestID,
			"attempt", attempt,
			"channel_id", ch.ID,
			"model", info.Request.OriginModelName,
			"error", lastErr.Error())

		// ★ 客户端已经收到字节：不能再换渠道，否则会收到两段拼接的内容。
		if info.WroteResponse() {
			break
		}
		if !lastErr.Retryable {
			break
		}
		if lastErr.ChannelError {
			service.DisableChannel(ch.ID)
		}
	}

	if lastErr == nil {
		lastErr = types.NewNoChannelError(info.Request.OriginModelName)
	}
	if !info.WroteResponse() {
		lastErr.Write(c)
	}
}

// relayAttempt 是"一次尝试"：转换 → 发请求 → 处理响应。
func relayAttempt(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor) *types.APIError {
	converted, err := adaptor.ConvertRequest(c, info, info.Request.Body)
	if err != nil {
		// 转换失败是请求本身的问题：重试同一个坏请求没有意义。
		return types.NewBadRequest("请求转换失败: " + err.Error())
	}

	resp, err := adaptor.DoRequest(c, info, bytes.NewReader(converted))
	if err != nil {
		// 客户端先断开时，上游请求会随 ctx 一起取消——这不是渠道故障。
		if c.Request.Context().Err() != nil {
			return types.NewClientGone()
		}
		return types.NewTransportError("请求上游失败: " + err.Error())
	}
	defer resp.Body.Close()

	info.Attempt.UpstreamStatus = resp.StatusCode
	_, apiErr := adaptor.DoResponse(c, resp, info)
	return apiErr
}

func readRequestBody(c *gin.Context) ([]byte, *types.APIError) {
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, types.NewRequestTooLarge()
		}
		return nil, types.NewBadRequest("读取请求体失败: " + err.Error())
	}
	return body, nil
}

// firstAPIKey 支持渠道里用逗号分隔多个 key（M4 才做轮询，M1 只用第一个）。
func firstAPIKey(keys string) string {
	for _, k := range strings.Split(keys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			return k
		}
	}
	return ""
}

// modelAllowed 校验令牌的模型白名单。未启用白名单时一律允许。
func modelAllowed(auth *relaycommon.AuthInfo, modelName string) bool {
	if auth == nil || !auth.ModelLimitsEnabled {
		return true
	}
	allowed := parseModelLimits(auth.ModelLimits)
	if len(allowed) == 0 {
		// 开了白名单却没有有效条目：按"谁都不允许"处理（fail-closed）。
		return false
	}
	for _, m := range allowed {
		if m == modelName {
			return true
		}
	}
	return false
}

// parseModelLimits 同时接受 JSON 数组与逗号分隔两种历史写法。
func parseModelLimits(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := common.UnmarshalString(raw, &arr); err == nil {
			return arr
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
