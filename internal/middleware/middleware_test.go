package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/cachex"
	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/setting/config"
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

// 访问日志必须能热关闭，且关掉日志后请求计数仍然继续（计数是探活数据，不是日志）。
func TestAccessLogRespectsConfigAndStillCounts(t *testing.T) {
	counter := cachex.NewCounter("test_access_log", time.Minute)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestId(), RouteTag(), AccessLog(counter))
	engine.GET("/api/status", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	doGet(t, engine, "/api/status", nil)
	doGet(t, engine, "/api/status", nil)

	if got, _ := counter.Get("total"); got != 2 {
		t.Errorf("请求计数 = %d，期望 2", got)
	}

	// 热关闭日志：计数仍在涨
	if err := config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `false`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = config.GlobalConfig.UpdateConfig("system_setting.enable_access_log", `true`)
	})

	doGet(t, engine, "/api/status", nil)
	if got, _ := counter.Get("total"); got != 3 {
		t.Errorf("关掉访问日志后计数应继续，得到 %d", got)
	}
}

func TestAccessLogToleratesNilCounter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(AccessLog(nil))
	engine.GET("/api/status", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	if w := doGet(t, engine, "/api/status", nil); w.Code != http.StatusOK {
		t.Errorf("计数器为 nil 时不该影响请求，状态码 = %d", w.Code)
	}
}

func TestCORSWildcard(t *testing.T) {
	t.Setenv("CORS_ALLOW_ORIGINS", "*")
	common.InitEnv()

	engine := newTestEngine(CORS())

	w := doGet(t, engine, "/api/status", map[string]string{"Origin": "https://any.example.com"})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q，期望 *", got)
	}

	// 预检请求
	req := httptest.NewRequest(http.MethodOptions, "/api/status", nil)
	req.Header.Set("Origin", "https://any.example.com")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("预检状态码 = %d，期望 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("预检响应缺少 Access-Control-Allow-Methods")
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Error("预检响应应允许 Authorization 头（M1 会用到）")
	}
}

func TestCORSAllowlistRejectsUnknownOrigin(t *testing.T) {
	t.Setenv("CORS_ALLOW_ORIGINS", "https://a.example.com, https://b.example.com")
	common.InitEnv()
	t.Cleanup(func() {
		t.Setenv("CORS_ALLOW_ORIGINS", "*")
		common.InitEnv()
	})

	engine := newTestEngine(CORS())

	allowed := doGet(t, engine, "/api/status", map[string]string{"Origin": "https://a.example.com"})
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://a.example.com" {
		t.Errorf("白名单内的来源应被回显，得到 %q", got)
	}
	// 回显来源的响应必须带 Vary：否则中间缓存会把 A 源的响应发给 B 源
	if got := allowed.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("回显来源时必须带 Vary: Origin，得到 %q", got)
	}

	denied := doGet(t, engine, "/api/status", map[string]string{"Origin": "https://evil.example.com"})
	if got := denied.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("白名单外的来源不该拿到 ACAO，得到 %q", got)
	}
	// 请求本身仍然正常处理：CORS 是浏览器侧的限制，不是服务端鉴权
	if denied.Code != http.StatusOK {
		t.Errorf("白名单外来源的请求仍应被正常处理，状态码 = %d", denied.Code)
	}
}
