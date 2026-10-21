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
