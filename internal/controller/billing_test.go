package controller_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/model"
)

// postChatWithKey 与 postChat 相同，只是允许指定令牌 key：
// 计费用例需要非 unlimited 的令牌，以便同时观察钱包与令牌两侧的账本。
func postChatWithKey(t *testing.T, baseURL, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func loadUser(t *testing.T, id int) model.User {
	t.Helper()
	var u model.User
	if err := model.DB.First(&u, id).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return u
}

func loadToken(t *testing.T, id int) model.Token {
	t.Helper()
	var tok model.Token
	if err := model.DB.First(&tok, id).Error; err != nil {
		t.Fatalf("读取令牌失败: %v", err)
	}
	return tok
}

// logsOfType 按类型读日志。日志库与主库共用连接（LOG_SQL_DSN 为空）。
func logsOfType(t *testing.T, logType int) []model.Log {
	t.Helper()
	var logs []model.Log
	if err := model.LOG_DB.Where("type = ?", logType).Order("id").Find(&logs).Error; err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	return logs
}

func decodeOther(t *testing.T, l model.Log) map[string]any {
	t.Helper()
	var other map[string]any
	if err := json.Unmarshal([]byte(l.Other), &other); err != nil {
		t.Fatalf("日志 other 不是合法 JSON: %v\n%s", err, l.Other)
	}
	return other
}

func assertOtherNumber(t *testing.T, other map[string]any, key string, want float64) {
	t.Helper()
	got, ok := other[key].(float64)
	if !ok {
		t.Fatalf("other[%q] = %#v，不是数字", key, other[key])
	}
	if got != want {
		t.Errorf("other[%q] = %v，期望 %v", key, got, want)
	}
}

func assertOtherString(t *testing.T, other map[string]any, key, want string) {
	t.Helper()
	if got, _ := other[key].(string); got != want {
		t.Errorf("other[%q] = %q，期望 %q", key, got, want)
	}
}

func assertOtherBool(t *testing.T, other map[string]any, key string, want bool) {
	t.Helper()
	if got, _ := other[key].(bool); got != want {
		t.Errorf("other[%q] = %#v，期望 %v", key, other[key], want)
	}
}

const usage1000x200 = `{"id":"chatcmpl-1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200}}`

// 一次成功请求：按上游 usage 结算，钱包/令牌各扣 2250，
// 且消费日志能独立复盘金额的每一步。
func TestRelayChargesFromUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, usage1000x200)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	ch := seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, body)
	}

	const wantRemain int64 = 1_000_000 - 2250
	gotUser := loadUser(t, user.ID)
	gotToken := loadToken(t, tok.ID)
	if gotUser.Quota != wantRemain {
		t.Errorf("用户余额 = %d，期望 %d", gotUser.Quota, wantRemain)
	}
	if gotToken.RemainQuota != wantRemain {
		t.Errorf("令牌余额 = %d，期望 %d", gotToken.RemainQuota, wantRemain)
	}
	if gotUser.UsedQuota != 2250 {
		t.Errorf("用户 used_quota = %d，期望 2250", gotUser.UsedQuota)
	}
	if gotToken.UsedQuota != 2250 {
		t.Errorf("令牌 used_quota = %d，期望 2250", gotToken.UsedQuota)
	}

	consume := logsOfType(t, constant.LogTypeConsume)
	if len(consume) != 1 {
		t.Fatalf("消费日志数 = %d，期望恰好 1 条", len(consume))
	}
	l := consume[0]
	if l.Quota != 2250 {
		t.Errorf("消费日志 Quota = %d，期望 2250", l.Quota)
	}
	if l.PromptTokens != 1000 || l.CompletionTokens != 200 {
		t.Errorf("消费日志 token = (%d, %d)，期望 (1000, 200)", l.PromptTokens, l.CompletionTokens)
	}
	self := decodeOther(t, l)
	assertOtherString(t, self, "billing_mode", "ratio")
	assertOtherNumber(t, self, "model_ratio", 2.5)
	assertOtherNumber(t, self, "completion_ratio", 4)
	assertOtherNumber(t, self, "group_ratio", 1)
	assertOtherNumber(t, self, "actual", 2250)
	assertOtherString(t, self, "usage_source", "upstream")
	assertOtherNumber(t, self, "channel_id", float64(ch.ID))
	if got, _ := self["request_id"].(string); got == "" {
		t.Error("消费日志 other.request_id 为空，无法把日志与请求对上")
	}

	if logs := logsOfType(t, constant.LogTypeError); len(logs) != 0 {
		t.Errorf("成功请求不应有错误日志，实际 %d 条", len(logs))
	}
}

// 重试到第二个渠道成功：整次请求只结算一次，账本只减少一次。
func TestRelaySettlesOnceAcrossRetries(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, usage1000x200)
	}))
	defer healthy.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	seedChannel(t, failing.URL, "gpt-4o", 10) // 优先命中，返回 500
	seedChannel(t, healthy.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应切到健康渠道: status=%d body=%s", resp.StatusCode, body)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Errorf("尝试次数 A=%d B=%d，期望各 1", hitsA.Load(), hitsB.Load())
	}

	consume := logsOfType(t, constant.LogTypeConsume)
	if len(consume) != 1 {
		t.Fatalf("消费日志数 = %d，期望恰好 1 条（重试不能重复结算）", len(consume))
	}
	if consume[0].Quota != 2250 {
		t.Errorf("消费日志 Quota = %d，期望 2250", consume[0].Quota)
	}

	const wantRemain int64 = 1_000_000 - 2250
	if got := loadUser(t, user.ID); got.Quota != wantRemain || got.UsedQuota != 2250 {
		t.Errorf("用户账本 = (quota %d, used %d)，期望 (%d, 2250)",
			got.Quota, got.UsedQuota, wantRemain)
	}
	if got := loadToken(t, tok.ID); got.RemainQuota != wantRemain || got.UsedQuota != 2250 {
		t.Errorf("令牌账本 = (remain %d, used %d)，期望 (%d, 2250)",
			got.RemainQuota, got.UsedQuota, wantRemain)
	}
	if logs := logsOfType(t, constant.LogTypeError); len(logs) != 0 {
		t.Errorf("最终成功不应写错误日志，实际 %d 条", len(logs))
	}
}

// 所有渠道都失败：预扣全额退回，没有任何消费；写一条可对账的错误日志。
func TestRelayRefundsWhenAllAttemptsFail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	ch := seedChannel(t, upstream.URL, "gpt-4o", 10)

	resp := postChatWithKey(t, base, tok.Key, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Fatalf("上游全部失败时应返回服务端错误: status=%d body=%s", resp.StatusCode, body)
	}

	gotUser := loadUser(t, user.ID)
	gotToken := loadToken(t, tok.ID)
	if gotUser.Quota != 1_000_000 || gotToken.RemainQuota != 1_000_000 {
		t.Errorf("预扣应全额退还：用户=%d 令牌=%d", gotUser.Quota, gotToken.RemainQuota)
	}
	if gotUser.UsedQuota != 0 || gotToken.UsedQuota != 0 {
		t.Errorf("失败请求不应累加 used_quota：用户=%d 令牌=%d",
			gotUser.UsedQuota, gotToken.UsedQuota)
	}

	if consume := logsOfType(t, constant.LogTypeConsume); len(consume) != 0 {
		t.Fatalf("失败请求不应产生消费日志，实际 %d 条", len(consume))
	}
	errLogs := logsOfType(t, constant.LogTypeError)
	if len(errLogs) != 1 {
		t.Fatalf("错误日志数 = %d，期望恰好 1 条", len(errLogs))
	}
	if errLogs[0].Quota != 0 {
		t.Errorf("错误日志 Quota = %d，期望 0", errLogs[0].Quota)
	}
	if !strings.Contains(errLogs[0].Content, "boom") {
		t.Errorf("错误日志应包含最后的上游错误，实际 %q", errLogs[0].Content)
	}
	other := decodeOther(t, errLogs[0])
	assertOtherBool(t, other, "refunded", true)

	if got := loadChannel(t, ch.ID); got.Status != constant.ChannelStatusAutoDisabled {
		t.Errorf("渠道状态 = %d，期望 auto_disabled", got.Status)
	}
}

// 上游 200 但没有 usage：不能按 0 计费，必须退款并留下错误日志。
func TestRelayRefundsWhenUsageMissing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, body)
	}

	if got := loadUser(t, user.ID); got.Quota != 1_000_000 || got.UsedQuota != 0 {
		t.Errorf("用户账本 = (quota %d, used %d)，期望全额退还", got.Quota, got.UsedQuota)
	}
	if got := loadToken(t, tok.ID); got.RemainQuota != 1_000_000 || got.UsedQuota != 0 {
		t.Errorf("令牌账本 = (remain %d, used %d)，期望全额退还", got.RemainQuota, got.UsedQuota)
	}

	if consume := logsOfType(t, constant.LogTypeConsume); len(consume) != 0 {
		t.Fatalf("无 usage 不应产生消费日志，实际 %d 条", len(consume))
	}
	errLogs := logsOfType(t, constant.LogTypeError)
	if len(errLogs) != 1 {
		t.Fatalf("错误日志数 = %d，期望恰好 1 条", len(errLogs))
	}
	if !strings.Contains(errLogs[0].Content, "usage") {
		t.Errorf("错误日志应说明缺少 usage，实际 %q", errLogs[0].Content)
	}
	assertOtherBool(t, decodeOther(t, errLogs[0]), "refunded", true)
}

// 余额不足时必须在选渠道之前拒绝：上游一次都不能被命中。
func TestRelayRejectsInsufficientQuota(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, usage1000x200)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 0, 1000, false)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key, `{"model":"gpt-4o","messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403；body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v body=%s", err, body)
	}
	if payload.Error.Type != "insufficient_quota" || payload.Error.Code != "insufficient_quota" {
		t.Errorf("错误形状 = (type %q, code %q)，期望 insufficient_quota",
			payload.Error.Type, payload.Error.Code)
	}
	if hits.Load() != 0 {
		t.Errorf("额度不足时上游不该被命中，实际 %d 次", hits.Load())
	}

	if got := loadUser(t, user.ID); got.Quota != 0 || got.UsedQuota != 0 {
		t.Errorf("用户账本不该变化：quota=%d used=%d", got.Quota, got.UsedQuota)
	}
	// 预扣事务先扣令牌、后扣用户；用户不足必须把令牌的扣减一起回滚。
	if got := loadToken(t, tok.ID); got.RemainQuota != 1000 || got.UsedQuota != 0 {
		t.Errorf("令牌账本不该变化：remain=%d used=%d", got.RemainQuota, got.UsedQuota)
	}
}

// 流式 usage 帧必须转发给客户端，并按该帧结算（113 quota）。
func TestRelayStreamsUsageAndCharges(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n")
		flusher.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"prompt_tokens":10`) {
		t.Errorf("客户端应收到 usage 帧，实际 body=%q", body)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("流应以 [DONE] 结束，实际 body=%q", body)
	}

	// (10 + 20*4) * 2.5 * 500000 / 1e6 = 112.5 → 113。
	const wantRemain int64 = 1_000_000 - 113
	if got := loadUser(t, user.ID); got.Quota != wantRemain || got.UsedQuota != 113 {
		t.Errorf("用户账本 = (quota %d, used %d)，期望 (%d, 113)",
			got.Quota, got.UsedQuota, wantRemain)
	}
	consume := logsOfType(t, constant.LogTypeConsume)
	if len(consume) != 1 || consume[0].Quota != 113 {
		t.Fatalf("消费日志 = %+v，期望恰好 1 条且 Quota=113", consume)
	}
	if !consume[0].IsStream {
		t.Error("流式请求的消费日志 IsStream 应为 true")
	}
}

// 流式请求必须向上游补 stream_options.include_usage，且保留客户端已有键。
func TestRelayStreamRequestsIncludeUsage(t *testing.T) {
	gotBody := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody <- string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	_, tok := seedBillingUserToken(t, 1_000_000, 1_000_000, false)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	resp := postChatWithKey(t, base, tok.Key,
		`{"model":"gpt-4o","stream":true,"stream_options":{"foo":1},"messages":[]}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var raw string
	select {
	case raw = <-gotBody:
	case <-time.After(5 * time.Second):
		t.Fatal("上游没有收到请求")
	}
	var payload struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v\n%s", err, raw)
	}
	if got, ok := payload.StreamOptions["include_usage"].(bool); !ok || !got {
		t.Errorf("stream_options.include_usage = %#v，期望 true；body=%s",
			payload.StreamOptions["include_usage"], raw)
	}
	if got, ok := payload.StreamOptions["foo"].(float64); !ok || got != 1 {
		t.Errorf("stream_options.foo = %#v，期望保留为 1；body=%s",
			payload.StreamOptions["foo"], raw)
	}
}

// 并发预扣不能被同一份余额重复使用：成功数受令牌额度约束，余额永不为负。
// SQLite 是单写者，30 个请求已经足够覆盖预扣的竞态窗口。
func TestConcurrentRelayNeverOverdraws(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 人为拉长请求：让所有请求先完成预扣，再进入结算，
		// 这样"余额是否会被同一份额度重复放行"才是真实的竞态场景。
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer upstream.Close()

	base := newRelayServer(t)
	user, tok := seedBillingUserToken(t, 1_000_000, 50, false)
	seedChannel(t, upstream.URL, "gpt-4o", 0)

	const requests = 30
	// messages:[] 的预扣 = round(3*2.5*1.2*0.5) = 5；令牌 50 只够 10 次。
	statuses := make([]int, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 10 * time.Second}

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
			if err != nil {
				statuses[i] = -1
				return
			}
			req.Header.Set("Authorization", "Bearer "+tok.Key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				statuses[i] = -1 // 连接失败不计入成功，也不在 goroutine 里 Fatal
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, code := range statuses {
		if code == http.StatusOK {
			successes++
		}
	}
	if successes < 1 || successes > 10 {
		t.Errorf("成功请求数 = %d，期望 1..10（令牌 50 / 每次预扣 5）", successes)
	}

	if got := loadToken(t, tok.ID); got.RemainQuota < 0 {
		t.Errorf("令牌余额被透支：%d", got.RemainQuota)
	}
	if got := loadUser(t, user.ID); got.Quota < 0 {
		t.Errorf("用户余额被透支：%d", got.Quota)
	}
}

func loadChannel(t *testing.T, id int) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := model.DB.First(&ch, id).Error; err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	return ch
}
