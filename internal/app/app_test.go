package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/model"
)

// freePort 取一个空闲端口：并行跑测试时固定端口必然冲突。
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// startApp 在后台启动 Run，返回 base URL 与取消函数。
func startApp(t *testing.T, opts Options) (string, context.CancelFunc, chan error) {
	t.Helper()

	port := freePort(t)
	dir := t.TempDir()

	t.Setenv("PORT", port)
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "app.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("NODE_NAME", "app-test")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "10")
	t.Setenv("SYNC_FREQUENCY", "5")
	t.Setenv("GIN_MODE", "test")
	t.Setenv("LOG_LEVEL", "error") // 测试里只留错误日志，免得刷屏

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, opts) }()

	base := "http://127.0.0.1:" + port
	waitForStatus(t, base, 20*time.Second)

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
		_ = model.CloseDB()
		model.OptionMap = map[string]string{}
	})

	return base, cancel, done
}

func waitForStatus(t *testing.T, base string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/status")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var parsed map[string]any
				if json.Unmarshal(body, &parsed) == nil && parsed["success"] == true {
					return
				}
			}
			lastErr = fmt.Errorf("状态码 %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待 /api/status 就绪超时: %v", lastErr)
}

func TestRunServesStatusAndShutsDownCleanly(t *testing.T) {
	base, cancel, done := startApp(t, Options{})

	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if parsed["success"] != true {
		t.Errorf("success = %v", parsed["success"])
	}

	// ★ 停机路径：ctx 取消 → 干净退出（这就是本机可测版的"SIGTERM 优雅停机"）
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 应返回 nil，得到 %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run 未在超时内退出（后台 goroutine 泄漏或停机卡住）")
	}

	// DB 必须已经关闭：句柄被置空是 CloseDB 的契约，
	// 也是"停机时不会有 goroutine 再写已关闭的库"的可断言证据。
	if model.DB != nil || model.LOG_DB != nil {
		t.Error("Run 返回后数据库句柄应已关闭并置空")
	}
}

// ★ 在途请求必须跑完：这正是"优雅停机"与"直接退出"的区别。
// 用注入的慢端点验证——没有任何生产端点能撑这么久。
func TestRunDrainsInFlightRequest(t *testing.T) {
	slowStarted := make(chan struct{})
	base, cancel, done := startApp(t, Options{
		InjectRoutes: func(engine *gin.Engine) {
			engine.GET("/api/slow", func(c *gin.Context) {
				close(slowStarted)
				time.Sleep(800 * time.Millisecond)
				c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": "完成"})
			})
		},
	})

	type result struct {
		body string
		code int
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/api/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- result{body: string(b), code: resp.StatusCode}
	}()

	// 等在途请求真的进到 handler 里，再触发停机
	select {
	case <-slowStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("慢请求没有被处理")
	}
	cancel()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("停机期间的请求被中断了（没有等在途请求跑完）: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Errorf("在途请求状态码 = %d，期望 200", r.code)
		}
		if !json.Valid([]byte(r.body)) {
			t.Errorf("在途请求的响应不完整: %q", r.body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("在途请求一直没有返回")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run 返回错误: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("停机未完成")
	}
}

// 端口被占用时 Run 必须很快返回错误，而不是悄悄假装启动成功。
func TestRunFailsFastWhenPortInUse(t *testing.T) {
	// ★ 占位监听必须用通配地址 ":0"，与 Run 里 srv.Addr=":port" 的绑定形式一致。
	// 计划原文用 "127.0.0.1:0"：在 Windows 上 127.0.0.1:port 与 [::]:port 是两个
	// 不同的 socket，通配绑定照样成功，于是这条测试永远等不到冲突、必然超时。
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())

	dir := t.TempDir()
	t.Setenv("PORT", port)
	t.Setenv("SQL_DSN", "local:"+filepath.Join(dir, "occupied.db"))
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("NODE_TYPE", "master")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "5")
	t.Setenv("GIN_MODE", "test")
	t.Setenv("LOG_LEVEL", "error")

	t.Cleanup(func() { _ = model.CloseDB(); model.OptionMap = map[string]string{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("端口被占用时 Run 应返回错误")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("端口被占用时 Run 没有及时返回")
	}
}

// 环境变量非法不该阻断启动（比如 PORT 写错）：回退默认值继续。
func TestEnvDefaultsApplyWhenNothingConfigured(t *testing.T) {
	common.InitEnv()
	if common.Port != common.DefaultPort {
		t.Errorf("Port = %d，期望默认 %d", common.Port, common.DefaultPort)
	}
	if !common.IsMasterNode {
		t.Error("默认应为 master")
	}
}

// ---------- .env 装载（本任务自研实现，见 app.go 的说明）----------

// 这两条测的不是 godotenv 自己的解析能力，而是**我们依赖它的那两条语义**：
// 进程环境优先（容器注入的变量不会被 .env 覆盖）、以及文件缺失时报错而非静默。
// godotenv 升级若改掉其中任何一条，这里会红——比读文档靠谱。
func TestLoadDotEnvSetsMissingVarsWithoutOverriding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# 注释行\n" +
		"\n" +
		"EXISTING=from_file\n" +
		"NEW_KEY=新值\n" +
		"export EXPORTED=带导出前缀\n" +
		"QUOTED=\"带空格 的值\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// 先登记恢复，再按需清空/覆盖。
	for _, k := range []string{"EXISTING", "NEW_KEY", "EXPORTED", "QUOTED"} {
		t.Setenv(k, "sentinel")
		_ = os.Unsetenv(k)
	}
	t.Setenv("EXISTING", "from_process")

	if err := godotenv.Load(path); err != nil {
		t.Fatalf("godotenv.Load 失败: %v", err)
	}

	if got := os.Getenv("EXISTING"); got != "from_process" {
		t.Errorf("进程环境应优先，EXISTING = %q", got)
	}
	if got := os.Getenv("NEW_KEY"); got != "新值" {
		t.Errorf("NEW_KEY = %q，期望 新值", got)
	}
	if got := os.Getenv("EXPORTED"); got != "带导出前缀" {
		t.Errorf("EXPORTED = %q，期望 带导出前缀", got)
	}
	if got := os.Getenv("QUOTED"); got != "带空格 的值" {
		t.Errorf("QUOTED = %q，期望 带空格 的值", got)
	}
}

func TestLoadDotEnvMissingFileReturnsError(t *testing.T) {
	if err := godotenv.Load(filepath.Join(t.TempDir(), "nope.env")); err == nil {
		t.Fatal("文件缺失时 godotenv.Load 应返回错误")
	}
}
