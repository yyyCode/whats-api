package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 转发面的 panic 必须保持 OpenAI 错误形状：SDK 不解析管理面信封。
func TestRecoveryUsesOpenAIShapeOnRelayPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CustomRecovery())
	engine.POST("/v1/chat/completions", func(c *gin.Context) { panic("boom") })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", w.Code)
	}
	var body struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Error == nil || body.Error.Type != "server_error" || body.Error.Code != "internal_error" {
		t.Errorf("转发路径的 panic 应返回 OpenAI 错误形状，实际 %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "success") {
		t.Errorf("转发路径不能混入管理面信封: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("响应泄漏了 panic 细节: %s", w.Body.String())
	}
}
