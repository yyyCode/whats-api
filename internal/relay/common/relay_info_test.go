package relaycommon

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestResetAttemptKeepsRequestState(t *testing.T) {
	info := GenRelayInfo(&AuthInfo{TokenID: 7, UserID: 3, Group: "vip"}, &RequestPayload{Model: "gpt-4o", Stream: true}, "req-1", []byte(`{}`))

	info.AddResponseWritten(12)
	info.Request.RetryIndex = 2
	info.ResetAttempt(ChannelMeta{ChannelID: 9, UpstreamModelName: "gpt-4o-2024"})

	if info.Request.TokenID != 7 || info.Request.OriginModelName != "gpt-4o" || info.Request.RetryIndex != 2 {
		t.Errorf("跨尝试状态被误清: %+v", info.Request)
	}
	if info.Attempt.ChannelID != 9 || info.WroteResponse() {
		t.Errorf("每次尝试的状态没有重置: %+v", info.Attempt)
	}
	if info.Stream.PingInterval != DefaultPingInterval {
		t.Errorf("默认保活间隔 = %v，期望 %v", info.Stream.PingInterval, DefaultPingInterval)
	}
}

func TestAuthInfoRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	want := &AuthInfo{TokenID: 1, UserID: 2, Group: "default", ModelLimitsEnabled: true, ModelLimits: `["gpt-4o"]`}
	SetAuthInfo(c, want)

	got := GetAuthInfo(c)
	if got == nil || got.TokenID != 1 || got.ModelLimits != want.ModelLimits {
		t.Fatalf("读回的 AuthInfo = %+v", got)
	}
	if GetAuthInfo(&gin.Context{}) != nil {
		t.Error("没有鉴权信息时应返回 nil，而不是 panic")
	}
}

func TestDefaultPingIntervalIsTenSeconds(t *testing.T) {
	if DefaultPingInterval != 10*time.Second {
		t.Errorf("保活间隔 = %v，期望 10s", DefaultPingInterval)
	}
}
