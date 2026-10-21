package controller_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
	"github.com/yyyCode/whats-api/internal/router"
)

// newRelayServer 起一个完整的网关（真实路由 + 真实 SQLite），返回它的 URL。
func newRelayServer(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	common.SQLDSN = "local:" + filepath.Join(dir, "relay.db")
	common.LogSQLDSN = ""
	common.IsMasterNode = true
	logger.SetupLogger("error", false)

	if err := model.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = model.CloseDB() })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	router.SetRouter(engine, router.Deps{StartedAt: time.Now()})

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv.URL
}

func seedUserToken(t *testing.T) *model.Token {
	t.Helper()
	u := model.User{Username: "u1", Status: constant.UserStatusEnabled, GroupName: "default"}
	if err := model.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	tok := model.Token{
		Key: "sk-test", UserID: u.ID, Status: constant.TokenStatusEnabled,
		ExpiredTime: -1, UnlimitedQuota: true, GroupName: "default",
	}
	if err := model.DB.Create(&tok).Error; err != nil {
		t.Fatal(err)
	}
	return &tok
}

func seedChannel(t *testing.T, baseURL, models string, priority int64) *model.Channel {
	t.Helper()
	ch := model.Channel{
		Type: constant.APITypeOpenAI, Key: "sk-upstream", BaseURL: baseURL,
		Models: models, GroupName: "default", Priority: priority, Weight: 1,
		Status: constant.ChannelStatusEnabled,
	}
	if err := model.DB.Create(&ch).Error; err != nil {
		t.Fatal(err)
	}
	return &ch
}

func postChat(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func TestRelayNonStreamPassthrough(t *testing.T) {
	const upstreamBody = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`

	var gotModel atomic.Value
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &payload)
		gotModel.Store(payload.Model)
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChat(t, base, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, body)
	}
	if string(body) != upstreamBody {
		t.Errorf("非流式响应应原样透传:\n got %s\nwant %s", body, upstreamBody)
	}
	if gotModel.Load() != "gpt-4o" {
		t.Errorf("上游收到的 model = %v", gotModel.Load())
	}
	if gotAuth.Load() != "Bearer sk-upstream" {
		t.Errorf("上游收到的 Authorization = %v", gotAuth.Load())
	}
}

func TestRelayAppliesModelMapping(t *testing.T) {
	var gotModel atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &payload)
		gotModel.Store(payload.Model)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	ch := seedChannel(t, upstream.URL, "gpt-alias", 0)
	ch.ModelMapping = `{"gpt-alias":"gpt-4o"}`
	if err := model.DB.Save(ch).Error; err != nil {
		t.Fatal(err)
	}

	resp := postChat(t, base, `{"model":"gpt-alias","messages":[]}`)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if gotModel.Load() != "gpt-4o" {
		t.Errorf("上游收到的 model = %v，期望映射后的 gpt-4o", gotModel.Load())
	}
}

func TestRelayRetriesOnUpstream5xxAndDisablesChannel(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"from":"B"}`)
	}))
	defer healthy.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	chA := seedChannel(t, failing.URL, "gpt-4o", 10) // 优先级高，先被选中
	seedChannel(t, healthy.URL, "gpt-4o", 0)

	resp := postChat(t, base, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"from":"B"`) {
		t.Fatalf("应切到健康渠道: status=%d body=%s", resp.StatusCode, body)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Errorf("尝试次数 A=%d B=%d，期望各 1", hitsA.Load(), hitsB.Load())
	}

	var stored model.Channel
	if err := model.DB.First(&stored, chA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != constant.ChannelStatusAutoDisabled {
		t.Errorf("失败渠道状态 = %d，期望 auto_disabled", stored.Status)
	}

	// 后续请求应直接跳过被禁用的渠道。
	resp2 := postChat(t, base, `{"model":"gpt-4o","messages":[]}`)
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if hitsA.Load() != 1 {
		t.Errorf("被禁用的渠道不该再收到请求，实际累计 %d 次", hitsA.Load())
	}
}

func TestRelayDoesNotRetryAfterStreamStarted(t *testing.T) {
	var hitsB atomic.Int32

	// 渠道 A：发一个事件就结束（没有 [DONE]）——流已经开始，不能换渠道。
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		flusher.Flush()
	}))
	defer broken.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer healthy.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	seedChannel(t, broken.URL, "gpt-4o", 10)
	seedChannel(t, healthy.URL, "gpt-4o", 0)

	resp := postChat(t, base, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "partial") {
		t.Errorf("客户端应收到已发出的部分内容: %q", body)
	}
	if hitsB.Load() != 0 {
		t.Errorf("开流之后不该再试第二个渠道，实际命中 %d 次", hitsB.Load())
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Errorf("第二个渠道的内容不该出现: %q", body)
	}
}

func TestRelayStreamsIncrementally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"n\":1}\n\n")
		flusher.Flush()
		time.Sleep(500 * time.Millisecond)
		io.WriteString(w, "data: {\"n\":2}\n\n")
		flusher.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	start := time.Now()
	resp := postChat(t, base, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读第一个事件失败: %v", err)
	}
	elapsed := time.Since(start)
	if !strings.Contains(first, `"n":1`) {
		t.Errorf("第一个事件 = %q", first)
	}
	// 上游第二个事件在 500ms 之后才发；首事件必须先到（留 50ms 余量给 Windows 调度）。
	if elapsed > 450*time.Millisecond {
		t.Errorf("第一个事件耗时 %v，说明没有逐事件转发", elapsed)
	}

	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), `"n":2`) || !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("后续事件缺失: %q", rest)
	}
}

func TestRelayStopsUpstreamWhenClientDisconnects(t *testing.T) {
	upstreamCancelled := make(chan struct{}, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				upstreamCancelled <- struct{}{}
				return
			default:
			}
			io.WriteString(w, fmt.Sprintf("data: {\"n\":%d}\n\n", i))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("读首个事件失败: %v", err)
	}

	cancel() // 模拟客户端断开
	resp.Body.Close()

	select {
	case <-upstreamCancelled:
		// 正确：下游断开后上游请求被取消，不再继续烧 token。
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后上游请求没有被取消")
	}
}

// 上游返回 3xx 时不能跟随重定向（POST 可能变 GET、丢 body 与凭证），
// 也不能把随后的登录页当成功响应透传；应归一化为 502 且不禁用渠道。
func TestRelayDoesNotFollowUpstreamRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>login</html>")
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/login", http.StatusFound)
	}))
	defer redirector.Close()

	base := newRelayServer(t)
	seedUserToken(t)
	ch := seedChannel(t, redirector.URL, "gpt-4o", 0)

	resp := postChat(t, base, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；body=%s", resp.StatusCode, body)
	}
	if targetHits.Load() != 0 {
		t.Errorf("不该跟随上游重定向，目标被命中 %d 次", targetHits.Load())
	}
	if strings.Contains(string(body), "login") {
		t.Errorf("重定向目标的内容不该透传给客户端: %q", body)
	}

	var stored model.Channel
	if err := model.DB.First(&stored, ch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != constant.ChannelStatusEnabled {
		t.Errorf("3xx 是配置问题，不该自动禁用渠道，实际 status=%d", stored.Status)
	}
}

func TestRelayAuthFailureUsesOpenAIShape(t *testing.T) {
	base := newRelayServer(t)
	seedUserToken(t)
	seedChannel(t, "http://127.0.0.1:1", "gpt-4o", 0)

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "authentication_error" {
		t.Errorf("错误 type = %q", body.Error.Type)
	}
}
