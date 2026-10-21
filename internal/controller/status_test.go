package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/cachex"
	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/model"
	"github.com/yyyCode/whats-api/internal/setting/system_setting"
)

// setupModel 建一个临时 SQLite 主库与日志库（controller 依赖 model 的全局句柄）。
func setupModel(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "status.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("NODE_NAME", "node-under-test")
	common.InitEnv()

	t.Cleanup(func() {
		_ = model.CloseDB()
		model.OptionMap = map[string]string{}
	})

	if err := model.InitDB(); err != nil {
		t.Fatalf("model.InitDB 失败: %v", err)
	}
	if err := model.CheckSetup(); err != nil {
		t.Fatalf("model.CheckSetup 失败: %v", err)
	}
	if err := model.InitOptionMap(); err != nil {
		t.Fatalf("model.InitOptionMap 失败: %v", err)
	}
	if err := model.InitLogDB(); err != nil {
		t.Fatalf("model.InitLogDB 失败: %v", err)
	}
}

func getStatus(t *testing.T, deps Deps) map[string]any {
	t.Helper()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/api/status", Status(deps))

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	return body
}

func dataOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 字段缺失或类型不对: %v", body)
	}
	return data
}

func TestStatusShape(t *testing.T) {
	setupModel(t)

	counter := cachex.NewCounter("test_status", time.Minute)
	_, _ = counter.Incr("total")
	_, _ = counter.Incr("total")
	_, _ = counter.Incr("total")

	body := getStatus(t, Deps{Requests: counter, StartedAt: time.Now().Add(-5 * time.Second)})

	if body["success"] != true {
		t.Errorf("success = %v，期望 true", body["success"])
	}
	if _, ok := body["message"]; !ok {
		t.Error("信封必须带 message 字段（将来给 i18n 的 message_key 用）")
	}

	data := dataOf(t, body)

	checks := map[string]any{
		"version":           common.Version,
		"node_name":         "node-under-test",
		"is_master":         true,
		"setup_complete":    true,
		"requests_total":    float64(3),
		"status_message":    system_setting.Default.StatusMessage,
		"log_level":         "info",
		"enable_access_log": true,
	}
	for key, want := range checks {
		if got := data[key]; got != want {
			t.Errorf("data[%q] = %v，期望 %v", key, got, want)
		}
	}

	if got, ok := data["uptime_seconds"].(float64); !ok || got < 5 {
		t.Errorf("uptime_seconds 应 >= 5，得到 %v", data["uptime_seconds"])
	}

	db := data["db"].(map[string]any)
	if db["driver"] != "sqlite" || db["dsn_kind"] != "file" || db["ok"] != true {
		t.Errorf("db 段不符: %v", db)
	}

	logDB := data["log_db"].(map[string]any)
	if logDB["shared_with_main"] != true || logDB["ok"] != true {
		t.Errorf("未独立配置日志库时 shared_with_main 应为 true: %v", logDB)
	}

	cache := data["cache"].(map[string]any)
	if cache["backend"] != "memory" {
		t.Errorf("未配 Redis 时 backend 应为 memory，得到 %v", cache["backend"])
	}
	if cache["redis_configured"] != false {
		t.Errorf("redis_configured = %v，期望 false", cache["redis_configured"])
	}

	cfg := data["config"].(map[string]any)
	// 静态断言：新增配置域会让这里失败，提醒作者确认 /api/status 的形状变化是有意的。
	// M2 增加 ratio_setting（6 项）后：system_setting 4 项 + ratio_setting 6 项 = 10。
	if cfg["registered_keys"].(float64) != 10 {
		t.Errorf("registered_keys = %v，期望 10", cfg["registered_keys"])
	}
	modules := cfg["modules"].([]any)
	if len(modules) != 2 || modules[0] != "ratio_setting" || modules[1] != "system_setting" {
		t.Errorf("modules = %v，期望 [ratio_setting system_setting]", modules)
	}
}

// 配置一改，/api/status 立刻反映——验收标准③在这个进程内的落点。
func TestStatusReflectsConfigChange(t *testing.T) {
	setupModel(t)
	deps := Deps{StartedAt: time.Now()}

	before := dataOf(t, getStatus(t, deps))["status_message"]

	if err := model.UpdateOption("system_setting.status_message", `"改过了"`); err != nil {
		t.Fatalf("UpdateOption 失败: %v", err)
	}

	after := dataOf(t, getStatus(t, deps))["status_message"]
	if after != "改过了" {
		t.Errorf("status_message = %v，期望 改过了（改前是 %v）", after, before)
	}
}

// ★ 不得泄漏敏感信息：DSN 里有密码，绝不能整段回显。
func TestStatusDoesNotLeakSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "secret.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("REDIS_CONN_STRING", "redis://user:super-secret-password@127.0.0.1:6379")
	t.Setenv("SESSION_SECRET", "should-never-appear")
	common.InitEnv()

	t.Cleanup(func() {
		_ = model.CloseDB()
		model.OptionMap = map[string]string{}
	})
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	if err := model.InitOptionMap(); err != nil {
		t.Fatal(err)
	}
	if err := model.InitLogDB(); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/api/status", Status(Deps{StartedAt: time.Now()}))
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	raw := w.Body.String()
	for _, secret := range []string{"super-secret-password", "should-never-appear", dir} {
		if strings.Contains(raw, secret) {
			t.Errorf("响应体泄漏了 %q：%s", secret, raw)
		}
	}
	if strings.Contains(raw, "local:") {
		t.Errorf("响应体不该包含 DSN 全文：%s", raw)
	}
}

// 计数器没接上时（初始化早期）也不能 nil panic。
func TestStatusWithoutCounter(t *testing.T) {
	setupModel(t)

	data := dataOf(t, getStatus(t, Deps{StartedAt: time.Now()}))
	if data["requests_total"] != float64(0) {
		t.Errorf("计数器缺失时 requests_total 应为 0，得到 %v", data["requests_total"])
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/x", func(c *gin.Context) { APIError(c, http.StatusNotFound, "接口不存在") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["success"] != false || body["message"] != "接口不存在" {
		t.Errorf("信封不符: %v", body)
	}
	if body["data"] != nil {
		t.Errorf("data 应为 null，得到 %v", body["data"])
	}
}
