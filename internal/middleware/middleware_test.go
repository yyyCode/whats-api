package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
)

func newTestEngine(middlewares ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middlewares...)
	engine.GET("/api/status", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	engine.GET("/v1/models", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	engine.GET("/index.html", func(c *gin.Context) { c.String(http.StatusOK, "html") })
	engine.GET("/panic", func(c *gin.Context) { panic("模拟业务 panic：含内部细节 db password=secret") })
	return engine
}

func doGet(t *testing.T, engine *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestRequestIdPreservesInbound(t *testing.T) {
	engine := newTestEngine(RequestId())

	w := doGet(t, engine, "/api/status", map[string]string{"X-Request-Id": "from-client-123"})

	if got := w.Header().Get("X-Request-Id"); got != "from-client-123" {
		t.Errorf("入站 request id 应被沿用，得到 %q", got)
	}
}

func TestRequestIdGeneratesWhenMissing(t *testing.T) {
	engine := newTestEngine(RequestId())

	first := doGet(t, engine, "/api/status", nil).Header().Get("X-Request-Id")
	second := doGet(t, engine, "/api/status", nil).Header().Get("X-Request-Id")

	if first == "" {
		t.Fatal("缺失时应生成 request id")
	}
	if first == second {
		t.Error("每次请求应生成不同的 request id（否则日志无法区分请求）")
	}
	if len(first) != 32 {
		t.Errorf("生成的 request id 期望 32 位十六进制，得到 %q", first)
	}
}

func TestRequestIdIsReadableFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestId())

	var seen string
	engine.GET("/x", func(c *gin.Context) {
		seen = GetRequestID(c)
		c.Status(http.StatusOK)
	})

	doGet(t, engine, "/x", map[string]string{"X-Request-Id": "ctx-id"})

	if seen != "ctx-id" {
		t.Errorf("context 里的 request id = %q，期望 ctx-id", seen)
	}
}

func TestVersionHeader(t *testing.T) {
	engine := newTestEngine(Version())

	w := doGet(t, engine, "/api/status", nil)
	if got := w.Header().Get("X-Whats-Api-Version"); got != common.Version {
		t.Errorf("版本头 = %q，期望 %q", got, common.Version)
	}
}

func TestRouteTag(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/api/status", RouteTagAPI},
		{"/v1/models", RouteTagRelay},
		{"/index.html", RouteTagWeb},
	}
	for _, c := range cases {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.Use(RouteTag())

		var seen string
		engine.GET("/*any", func(ctx *gin.Context) {
			seen = GetRouteTag(ctx)
			ctx.Status(http.StatusOK)
		})

		doGet(t, engine, c.path, nil)
		if seen != c.want {
			t.Errorf("%s 的标签 = %q，期望 %q", c.path, seen, c.want)
		}
	}
}

// panic 必须变成 500 JSON，且**不能把堆栈细节返回给客户端**。
func TestRecoveryReturnsSafeJSON(t *testing.T) {
	engine := newTestEngine(CustomRecovery())

	w := doGet(t, engine, "/panic", nil)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（body=%q）", err, w.Body.String())
	}
	if body["success"] != false {
		t.Errorf("success 应为 false，得到 %v", body["success"])
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "panic") {
		t.Errorf("响应体泄漏了内部细节: %s", w.Body.String())
	}
}
