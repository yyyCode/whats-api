package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

func TestConvertRequestPassesThroughWhenNotMapped(t *testing.T) {
	a := &Adaptor{}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"custom":123}`)

	got, err := a.ConvertRequest(nil, &relaycommon.RelayInfo{
		Attempt: relaycommon.AttemptState{ChannelMeta: relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o", ModelMapped: false,
		}},
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("未映射时必须字节级透传，实际 %s", got)
	}
}

func TestConvertRequestRewritesMappedModel(t *testing.T) {
	a := &Adaptor{}
	body := []byte(`{"model":"gpt-alias","messages":[]}`)

	got, err := a.ConvertRequest(nil, &relaycommon.RelayInfo{
		Attempt: relaycommon.AttemptState{ChannelMeta: relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o", ModelMapped: true,
		}},
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"model":"gpt-4o"`) {
		t.Errorf("映射后的模型名没写进请求体: %s", got)
	}
	if strings.Contains(string(got), "gpt-alias") {
		t.Errorf("原始模型名不该残留: %s", got)
	}
}

// failingBody 模拟"上游响应头已到、body 读到一半连接被取消"。
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, context.Canceled }
func (failingBody) Close() error             { return nil }

func TestDoResponseClientGoneIsNotChannelError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       failingBody{},
	}
	_, apiErr := (&Adaptor{}).DoResponse(c, resp, &relaycommon.RelayInfo{})
	if apiErr == nil {
		t.Fatal("读取 body 失败必须返回错误")
	}
	if apiErr.StatusCode != 0 || apiErr.ChannelError || apiErr.Retryable {
		t.Errorf("客户端断开应返回不可重试、不归因渠道的 ClientGone，实际 %+v", apiErr)
	}
}

// TestDoResponseCollectsUsageAndForwardsBody 验证非流式响应：
// usage 被采集进 Attempt，同时写给客户端的字节与上游响应完全一致。
func TestDoResponseCollectsUsageAndForwardsBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	body := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	info := &relaycommon.RelayInfo{}
	if _, apiErr := (&Adaptor{}).DoResponse(c, resp, info); apiErr != nil {
		t.Fatalf("DoResponse 失败: %+v", apiErr)
	}

	want := &types.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	if info.Attempt.Usage == nil || *info.Attempt.Usage != *want {
		t.Errorf("usage 采集错误，得到 %+v，期望 %+v", info.Attempt.Usage, want)
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Errorf("响应体必须原样转发，实际 %s", w.Body.String())
	}
}

func TestConvertRequestStreamInjectsIncludeUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)

	got, err := (&Adaptor{}).ConvertRequest(nil, &relaycommon.RelayInfo{
		Request: relaycommon.RequestState{IsStream: true},
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"include_usage":true`) {
		t.Errorf("流式请求必须补 include_usage=true: %s", got)
	}
}

func TestConvertRequestStreamMergesExistingStreamOptions(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true,` +
		`"stream_options":{"include_usage":false,"foo":1}}`)

	got, err := (&Adaptor{}).ConvertRequest(nil, &relaycommon.RelayInfo{
		Request: relaycommon.RequestState{IsStream: true},
	}, body)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		StreamOptions map[string]json.RawMessage `json:"stream_options"`
	}
	if err := common.Unmarshal(got, &payload); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if string(payload.StreamOptions["include_usage"]) != "true" {
		t.Errorf("include_usage 必须被改为 true: %s", got)
	}
	if string(payload.StreamOptions["foo"]) != "1" {
		t.Errorf("stream_options 的其它键必须保留: %s", got)
	}
}

func TestConvertRequestStreamReplacesNullStreamOptions(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true,"stream_options":null}`)

	got, err := (&Adaptor{}).ConvertRequest(nil, &relaycommon.RelayInfo{
		Request: relaycommon.RequestState{IsStream: true},
	}, body)
	if err != nil {
		t.Fatal(err)
	}

	var payload struct {
		StreamOptions map[string]json.RawMessage `json:"stream_options"`
	}
	if err := common.Unmarshal(got, &payload); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if len(payload.StreamOptions) != 1 ||
		string(payload.StreamOptions["include_usage"]) != "true" {
		t.Errorf("stream_options 为 null 时必须新建对象且只含 include_usage: %s", got)
	}
}

func TestGetRequestURL(t *testing.T) {
	a := &Adaptor{}
	cases := map[string]string{
		"http://127.0.0.1:8080":    "http://127.0.0.1:8080/v1/chat/completions",
		"http://127.0.0.1:8080/":   "http://127.0.0.1:8080/v1/chat/completions",
		"http://127.0.0.1:8080/v1": "http://127.0.0.1:8080/v1/chat/completions",
	}
	for base, want := range cases {
		info := &relaycommon.RelayInfo{Attempt: relaycommon.AttemptState{ChannelMeta: relaycommon.ChannelMeta{BaseURL: base}}}
		got, err := a.GetRequestURL(info)
		if err != nil || got != want {
			t.Errorf("base %q → %q (err=%v)，期望 %q", base, got, err, want)
		}
	}

	_, err := a.GetRequestURL(&relaycommon.RelayInfo{})
	if err == nil {
		t.Error("没有 base_url 时应报错")
	}
}
