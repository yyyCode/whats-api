package types

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteUsesOpenAIShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	NewAuthenticationError("令牌无效").Write(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", w.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Error.Message != "令牌无效" || body.Error.Type != "authentication_error" {
		t.Errorf("错误体 = %+v", body.Error)
	}
}

func TestClientGoneWritesNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	NewClientGone().Write(c)

	if w.Body.Len() != 0 {
		t.Errorf("客户端断开时不该写响应，实际写了 %q", w.Body.String())
	}
}

func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
		channel   bool
	}{
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusTooManyRequests, true, true},
		{http.StatusInternalServerError, true, true},
		{http.StatusBadGateway, true, true},
	}
	for _, c := range cases {
		e := NewUpstreamError(c.status, "上游错误")
		if e.StatusCode != c.status {
			t.Errorf("status %d 保留为 %d", c.status, e.StatusCode)
		}
		if e.Retryable != c.retryable || e.ChannelError != c.channel {
			t.Errorf("status %d: retryable=%v channel=%v，期望 %v/%v",
				c.status, e.Retryable, e.ChannelError, c.retryable, c.channel)
		}
	}
}

func TestTransportErrorIsRetryableChannelError(t *testing.T) {
	e := NewTransportError("连接被拒")
	if !e.Retryable || !e.ChannelError {
		t.Error("传输错误必须可重试且归因于渠道")
	}
}
