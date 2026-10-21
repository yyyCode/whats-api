package openai

import (
	"strings"
	"testing"

	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
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
