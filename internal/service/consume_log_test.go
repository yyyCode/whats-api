package service

import (
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/model"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

// consumeLogFixture 返回一份固定的计价快照与请求上下文。
// 金额按「gpt-4o、120 输入 + 80 输出、分组倍率 1.2」构造，
// 数值不求真实，只求每个字段都能和日志一一对应。
func consumeLogFixture() (info *relaycommon.RelayInfo, usage *types.Usage, detail QuotaDetail) {
	usage = &types.Usage{PromptTokens: 120, CompletionTokens: 80, TotalTokens: 200}
	detail = QuotaDetail{
		ModelRatio:           2.5,
		CompletionRatio:      4,
		GroupRatio:           1.2,
		PreConsumeMultiplier: 1.2,
		PromptTokens:         120,
		CompletionTokens:     80,
		PreConsumedQuota:     180,
		ActualQuota:          1920,
		QuotaClamp:           common.ClampNone,
	}
	info = &relaycommon.RelayInfo{
		Request: relaycommon.RequestState{
			RequestID:       "req-consume-1",
			UserID:          42,
			Group:           "default",
			OriginModelName: "gpt-4o",
			IsStream:        true,
			RetryIndex:      1,
			StartTime:       time.Now().Add(-2 * time.Second),
		},
		Attempt: relaycommon.AttemptState{
			ChannelMeta: relaycommon.ChannelMeta{
				ChannelID:         7,
				ChannelName:       "channel-7",
				UpstreamModelName: "gpt-4o-2024-08-06",
			},
			UpstreamStatus: 200,
			Usage:          usage,
		},
	}
	return info, usage, detail
}

// decodeLogOther 把 other 字段反序列化回 map，确认落库格式可被对账逻辑消费。
func decodeLogOther(t *testing.T, l *model.Log) map[string]any {
	t.Helper()
	var other map[string]any
	if err := common.UnmarshalString(l.Other, &other); err != nil {
		t.Fatalf("other 不是合法 JSON: %v\n%s", err, l.Other)
	}
	return other
}

func assertOtherNumber(t *testing.T, other map[string]any, key string, want float64) {
	t.Helper()
	got, ok := other[key].(float64)
	if !ok {
		t.Fatalf("other[%q] = %#v，不是数字", key, other[key])
	}
	if got != want {
		t.Errorf("other[%q] = %v，期望 %v", key, got, want)
	}
}

func assertOtherString(t *testing.T, other map[string]any, key, want string) {
	t.Helper()
	if got, _ := other[key].(string); got != want {
		t.Errorf("other[%q] = %q，期望 %q", key, got, want)
	}
}

// 消费日志：列字段与 other 里的每个计价要素都要和 detail 一致，
// 否则事后无法复盘「这笔 quota 用了哪些倍率和 token」。
func TestBuildConsumeLog(t *testing.T) {
	info, usage, detail := consumeLogFixture()
	quota := detail.ActualQuota // 正常结算路径：写入日志的金额就是 detail.ActualQuota

	l := BuildConsumeLog(info, usage, quota, detail, "203.0.113.9")

	if l.Type != constant.LogTypeConsume {
		t.Errorf("Type = %d，期望 %d", l.Type, constant.LogTypeConsume)
	}
	if quota != detail.ActualQuota {
		t.Fatalf("前置条件不成立：quota=%d detail.ActualQuota=%d", quota, detail.ActualQuota)
	}
	if l.Quota != int64(detail.ActualQuota) {
		t.Errorf("Quota = %d，期望 %d", l.Quota, detail.ActualQuota)
	}
	if l.PromptTokens != usage.PromptTokens || l.CompletionTokens != usage.CompletionTokens {
		t.Errorf("token 字段 = (%d, %d)，期望 (%d, %d)",
			l.PromptTokens, l.CompletionTokens, usage.PromptTokens, usage.CompletionTokens)
	}
	if l.ModelName != "gpt-4o" {
		t.Errorf("ModelName = %q，期望 gpt-4o", l.ModelName)
	}
	if l.ChannelID != 7 {
		t.Errorf("ChannelID = %d，期望 7", l.ChannelID)
	}
	if l.GroupName != "default" {
		t.Errorf("GroupName = %q，期望 default", l.GroupName)
	}
	if l.RequestID != "req-consume-1" {
		t.Errorf("RequestID = %q，期望 req-consume-1", l.RequestID)
	}
	if l.IP != "203.0.113.9" {
		t.Errorf("IP = %q，期望 203.0.113.9", l.IP)
	}
	if !l.IsStream {
		t.Error("IsStream = false，期望 true")
	}
	// StartTime 是 2 秒前；给机器抖动留出余量，但不允许为 0（零值守卫会吞掉耗时）。
	if l.UseTime < 2 || l.UseTime > 5 {
		t.Errorf("UseTime = %d，期望约 2 秒", l.UseTime)
	}
	if l.CreatedAt != 0 {
		t.Errorf("CreatedAt 应留给 GORM 填充，当前为 %d", l.CreatedAt)
	}

	other := decodeLogOther(t, l)
	assertOtherString(t, other, "billing_mode", "ratio")
	assertOtherNumber(t, other, "model_ratio", detail.ModelRatio)
	assertOtherNumber(t, other, "completion_ratio", detail.CompletionRatio)
	assertOtherNumber(t, other, "group_ratio", detail.GroupRatio)
	assertOtherNumber(t, other, "pre_consume_multiplier", detail.PreConsumeMultiplier)
	assertOtherNumber(t, other, "estimated_prompt_tokens", float64(detail.PromptTokens))
	assertOtherNumber(t, other, "prompt_tokens", float64(usage.PromptTokens))
	assertOtherNumber(t, other, "completion_tokens", float64(usage.CompletionTokens))
	assertOtherNumber(t, other, "pre_consumed", float64(detail.PreConsumedQuota))
	assertOtherNumber(t, other, "actual", float64(detail.ActualQuota))
	assertOtherString(t, other, "quota_saturation", detail.QuotaClamp.String())
	assertOtherString(t, other, "upstream_model", "gpt-4o-2024-08-06")
	assertOtherNumber(t, other, "retry_index", float64(info.Request.RetryIndex))
	assertOtherNumber(t, other, "upstream_status", float64(info.Attempt.UpstreamStatus))
	assertOtherString(t, other, "usage_source", "upstream")
	assertOtherString(t, other, "request_id", info.Request.RequestID)
	assertOtherNumber(t, other, "channel_id", float64(info.Attempt.ChannelID))
	assertOtherString(t, other, "channel_name", info.Attempt.ChannelName)
	if got, ok := other["is_stream"].(bool); !ok || !got {
		t.Errorf("other[is_stream] = %#v，期望 true", other["is_stream"])
	}
}

// 计价错误日志：不产生消费（Quota=0），必须能看出预扣已退款、usage 缺失。
func TestBuildBillingErrorLog(t *testing.T) {
	info, _, detail := consumeLogFixture()
	reason := "上游未返回 usage，已退还预扣"

	l := BuildBillingErrorLog(info, reason, detail, "198.51.100.7")

	if l.Type != constant.LogTypeError {
		t.Errorf("Type = %d，期望 %d", l.Type, constant.LogTypeError)
	}
	if l.Quota != 0 {
		t.Errorf("错误日志 Quota = %d，期望 0", l.Quota)
	}
	if l.Content != reason {
		t.Errorf("Content = %q，期望 %q", l.Content, reason)
	}
	if l.RequestID != info.Request.RequestID || l.ChannelID != info.Attempt.ChannelID {
		t.Errorf("错误日志的请求元数据缺失：request_id=%q channel_id=%d",
			l.RequestID, l.ChannelID)
	}
	if l.IP != "198.51.100.7" {
		t.Errorf("IP = %q，期望 198.51.100.7", l.IP)
	}

	other := decodeLogOther(t, l)
	if refunded, ok := other["refunded"].(bool); !ok || !refunded {
		t.Errorf("other[refunded] = %#v，期望 true", other["refunded"])
	}
	assertOtherString(t, other, "reason", reason)
	assertOtherString(t, other, "usage_source", "missing")
	// 错误日志仍保留同一套计价要素，便于核对「退了多少钱」。
	assertOtherNumber(t, other, "model_ratio", detail.ModelRatio)
	assertOtherNumber(t, other, "pre_consumed", float64(detail.PreConsumedQuota))
	assertOtherNumber(t, other, "actual", float64(detail.ActualQuota))
	assertOtherString(t, other, "quota_saturation", detail.QuotaClamp.String())
}

// 日志库未初始化时必须返回明确错误，而不是静默丢弃审计记录。
func TestWriteLogWithoutLogDB(t *testing.T) {
	// 临时摘掉句柄再恢复：不能把包级全局留在 nil，否则同包后续用例全部写不了日志。
	saved := model.LOG_DB
	model.LOG_DB = nil
	defer func() { model.LOG_DB = saved }()

	if err := model.WriteLog(&model.Log{Type: constant.LogTypeConsume}); err == nil {
		t.Fatal("LOG_DB 为 nil 时 WriteLog 应返回错误")
	}
	if err := model.WriteLog(nil); err == nil {
		t.Fatal("入参为 nil 时 WriteLog 应返回错误")
	}
}

// 落库往返：WriteLog 之后能从 LOG_DB 读回同一行，且 other 仍可反序列化。
func TestWriteLogRoundTrip(t *testing.T) {
	withBillingDB(t)
	if err := model.InitLogDB(); err != nil {
		t.Fatalf("InitLogDB 失败: %v", err)
	}

	info, usage, detail := consumeLogFixture()
	l := BuildConsumeLog(info, usage, detail.ActualQuota, detail, "203.0.113.10")

	if err := model.WriteLog(l); err != nil {
		t.Fatalf("WriteLog 失败: %v", err)
	}
	if l.ID == 0 {
		t.Fatal("WriteLog 后 ID 仍为 0，GORM 未回填主键")
	}

	var got model.Log
	if err := model.LOG_DB.First(&got, l.ID).Error; err != nil {
		t.Fatalf("读回日志 %d 失败: %v", l.ID, err)
	}
	if got.Type != constant.LogTypeConsume || got.Quota != int64(detail.ActualQuota) {
		t.Errorf("读回 type=%d quota=%d，期望 %d/%d",
			got.Type, got.Quota, constant.LogTypeConsume, detail.ActualQuota)
	}
	if got.ModelName != "gpt-4o" || got.RequestID != "req-consume-1" || got.IP != "203.0.113.10" {
		t.Errorf("读回元数据不符：model=%q request_id=%q ip=%q",
			got.ModelName, got.RequestID, got.IP)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt 应由 GORM 自动填充，读回为 0")
	}

	other := decodeLogOther(t, &got)
	assertOtherString(t, other, "billing_mode", "ratio")
	assertOtherNumber(t, other, "actual", float64(detail.ActualQuota))
	assertOtherNumber(t, other, "pre_consumed", float64(detail.PreConsumedQuota))
}
