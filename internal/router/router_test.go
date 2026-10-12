package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/model"
)

func setupEngine(t *testing.T) *gin.Engine {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "router.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("CORS_ALLOW_ORIGINS", "*")
	common.InitEnv()

	t.Cleanup(func() {
		_ = model.CloseDB()
		model.OptionMap = map[string]string{}
	})

	if err := model.InitDB(); err != nil {
		t.Fatalf("model.InitDB 失败: %v", err)
	}
	if err := model.InitOptionMap(); err != nil {
		t.Fatalf("model.InitOptionMap 失败: %v", err)
	}
	if err := model.InitLogDB(); err != nil {
		t.Fatalf("model.InitLogDB 失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetRouter(engine, Deps{StartedAt: time.Now()})
	return engine
}

func doGet(t *testing.T, engine *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestAPIRoutesRegistered(t *testing.T) {
	engine := setupEngine(t)

	w := doGet(t, engine, "/api/status")
	if w.Code != http.StatusOK {
		t.Fatalf("/api/status 状态码 = %d，body=%s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["success"] != true {
		t.Errorf("success = %v，期望 true", body["success"])
	}

	// 中间件链的产物都应该在
	if got := w.Header().Get("X-Whats-Api-Version"); got != common.Version {
		t.Errorf("版本头 = %q", got)
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("缺少 X-Request-Id 响应头")
	}
}

// ★ 两个面从第一天就分开：/v1 未实现的路径必须返回 JSON 404，
// 而不是被 SPA 兜底吞成 200 + HTML——那会让写错路径的 SDK 拿到一坨 HTML。
func TestRelayPathsNeverFallThroughToSPA(t *testing.T) {
	engine := setupEngine(t)

	for _, path := range []string{"/v1/chat/completions", "/v1/models", "/v1"} {
		w := doGet(t, engine, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 状态码 = %d，期望 404", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Errorf("%s 的 Content-Type = %q，期望 JSON", path, ct)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 的响应不是 JSON: %s", path, w.Body.String())
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s 的响应缺少 error 对象: %v", path, body)
		}
	}
}

func TestUnknownAPIPathReturnsEnvelope(t *testing.T) {
	engine := setupEngine(t)

	w := doGet(t, engine, "/api/nonexistent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["success"] != false || body["message"] == "" {
		t.Errorf("/api 的错误应使用信封形状: %v", body)
	}
}

func TestUnknownWebPathExplainsM3(t *testing.T) {
	engine := setupEngine(t)

	w := doGet(t, engine, "/dashboard")
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "M3") {
		t.Errorf("提示信息应说明前端在 M3，得到 %q", msg)
	}
}
