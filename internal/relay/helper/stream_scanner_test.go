package helper

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

// scanBody 把一段 SSE 文本包成 *http.Response，供纯解析测试使用。
func scanBody(s string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(s))}
}

func newGinContext(w http.ResponseWriter) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c
}

func TestStreamScannerForwardsDataAndStopsAtDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c := newGinContext(w)

	body := "data: {\"a\":1}\n\n" +
		": upstream-comment\n\n" +
		"event: ping\n\n" +
		"data: {\"b\":2}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"after\":3}\n\n"

	info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: time.Hour}}
	if err := StreamScannerHandler(c, scanBody(body), info); err != nil {
		t.Fatalf("转发失败: %v", err)
	}

	out := w.Body.String()
	if !strings.Contains(out, `data: {"a":1}`) || !strings.Contains(out, `data: {"b":2}`) {
		t.Errorf("数据事件没有转发: %q", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("[DONE] 没有转发: %q", out)
	}
	if strings.Contains(out, "after") {
		t.Errorf("[DONE] 之后的事件不该再转发: %q", out)
	}
	if !info.WroteResponse() {
		t.Error("开流后必须标记已写响应，否则重试会重复内容")
	}
}

func TestStreamScannerErrorsWhenUpstreamEndsWithoutDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c := newGinContext(w)

	info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: time.Hour}}
	err := StreamScannerHandler(c, scanBody("data: {\"a\":1}\n\n"), info)
	if err == nil {
		t.Fatal("上游没发 [DONE] 就结束，必须报错（否则会把截断当成正常完成）")
	}
}

// TestStreamScannerKeepsNonZeroUsageAcrossTrailingZeroFrame 钉住 MergeNonZero 的语义：
// 某些网关会在真正带用量的 accumulate 帧之后，再补一帧全零 usage。
func TestStreamScannerKeepsNonZeroUsageAcrossTrailingZeroFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c := newGinContext(w)

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"

	info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: time.Hour}}
	if err := StreamScannerHandler(c, scanBody(body), info); err != nil {
		t.Fatalf("转发失败: %v", err)
	}

	want := types.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6}
	if info.Attempt.Usage == nil || *info.Attempt.Usage != want {
		t.Errorf("零值帧不该抹掉已采集的 usage，得到 %+v，期望 %+v", info.Attempt.Usage, want)
	}

	out := w.Body.String()
	for _, frame := range []string{
		`"prompt_tokens":5`,
		`"prompt_tokens":0`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, frame) {
			t.Errorf("客户端应收到所有转发帧（缺少 %q）: %q", frame, out)
		}
	}
}

// TestStreamScannerMergesUsageFromLaterFrame 覆盖 usage 出现在普通内容帧之后的情况。
func TestStreamScannerMergesUsageFromLaterFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c := newGinContext(w)

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: time.Hour}}
	if err := StreamScannerHandler(c, scanBody(body), info); err != nil {
		t.Fatalf("转发失败: %v", err)
	}

	want := types.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
	if info.Attempt.Usage == nil || *info.Attempt.Usage != want {
		t.Errorf("后到的 usage 帧没有被采集，得到 %+v，期望 %+v", info.Attempt.Usage, want)
	}
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Errorf("普通内容帧必须照常转发: %q", w.Body.String())
	}
}

// TestStreamScannerDoesNotParseDoneAsJSON 确认 [DONE] 不会被送进 JSON 解析。
func TestStreamScannerDoesNotParseDoneAsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c := newGinContext(w)

	info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: time.Hour}}
	if err := StreamScannerHandler(c, scanBody("data: [DONE]\n\n"), info); err != nil {
		t.Fatalf("[DONE] 应正常结束，实际: %v", err)
	}
	if info.Attempt.Usage != nil {
		t.Errorf("[DONE] 不是 JSON，不该产生 usage，实际 %+v", info.Attempt.Usage)
	}
}

func TestStreamScannerSendsKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// 先把响应头发出去，否则客户端要等服务端第一次写 body 才拿到响应，
		// 转发层根本没机会在沉默期发保活。这不是放宽窗口，而是必要的时序前提。
		flusher.Flush()
		// 先沉默 300ms：这段时间里转发层必须自己发保活。
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	engine := gin.New()
	engine.GET("/s", func(c *gin.Context) {
		resp, err := http.Get(upstream.URL)
		if err != nil {
			t.Errorf("连上游失败: %v", err)
			return
		}
		info := &relaycommon.RelayInfo{Stream: relaycommon.StreamOptions{PingInterval: 50 * time.Millisecond}}
		if apiErr := StreamScannerHandler(c, resp, info); apiErr != nil {
			t.Errorf("转发失败: %v", apiErr)
		}
	})
	srv := httptest.NewServer(engine)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	deadline := time.After(2 * time.Second)
	sawPing := false
	for !sawPing {
		lineCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			line, err := reader.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			lineCh <- line
		}()
		select {
		case <-deadline:
			t.Fatal("超时：没有收到保活注释")
		case err := <-errCh:
			t.Fatalf("读响应失败: %v", err)
		case line := <-lineCh:
			if strings.HasPrefix(line, ":") {
				sawPing = true
			}
		}
	}
}
