// Package openai 是 OpenAI 兼容上游的适配器。
//
// M1 只做 Chat Completions：请求透传（只改 model）、响应透传（流式走 SSE）。
// usage 解析在 M2 计费时补上——接口已经返回 any，不用改签名。
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/relay/channel"
	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/relay/helper"
	"github.com/yyyCode/whats-api/internal/types"
)

// maxErrorBodyBytes 限制我们从上游错误响应里读多少：错误体只用于展示，
// 不能让一个异常上游用 1GB 的错误页把网关内存打爆。
const maxErrorBodyBytes = 64 << 10

type Adaptor struct {
	info *relaycommon.RelayInfo
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) { a.info = info }

// ConvertRequest 透传请求体；只有模型映射命中时才重新序列化。
//
// ★ 未映射时返回原字节：解析再序列化会改变字段顺序、丢未知字段的原始形状，
// 对"透传"来说这些都不是我们该做的决定。
func (a *Adaptor) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody []byte) ([]byte, error) {
	if !info.Attempt.ModelMapped || info.Attempt.UpstreamModelName == "" {
		return requestBody, nil
	}

	var payload map[string]json.RawMessage
	if err := common.Unmarshal(requestBody, &payload); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	modelBytes, err := common.Marshal(info.Attempt.UpstreamModelName)
	if err != nil {
		return nil, err
	}
	payload["model"] = modelBytes
	return common.Marshal(payload)
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(info.Attempt.BaseURL), "/")
	if base == "" {
		return "", errors.New("渠道未配置 base_url")
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions", nil
	}
	return base + "/v1/chat/completions", nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	header.Set("Authorization", "Bearer "+info.Attempt.APIKey)
	header.Set("Content-Type", "application/json")
	if info.Request.IsStream {
		header.Set("Accept", "text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	return nil
}

// DoRequest 复用共享出站传输：超时策略与连接复用只在 channel 包维护一份。
func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (*http.Response, error) {
	return channel.DoApiRequest(a, c, info, body)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.APIError) {
	// 非流式路径读完后必须归还连接；流式路径里 StreamScannerHandler
	// 也会关一次，重复 Close 是幂等的。
	defer resp.Body.Close()

	// ★ 先判状态码：非 2xx 时一个字节都还没写给客户端，调用方仍然可以换渠道。
	// 只判 >=400 会漏掉 3xx（例如带 Location 但被 CheckRedirect 拦下的响应），
	// 那会被误当成成功响应写出去，重试的前提就没了。
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, types.NewUpstreamError(resp.StatusCode, upstreamErrorMessage(raw))
	}

	if info.Request.IsStream {
		if apiErr := helper.StreamScannerHandler(c, resp, info); apiErr != nil {
			return nil, apiErr
		}
		// M1 不解析流内 usage；M2 会在 SSE 转发的同时累加 usage。
		return nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// 客户端先断开时，上游连接会随 ctx 一起被关闭，ReadAll 返回
		// context canceled / unexpected EOF。这不是渠道故障：
		// 若当成 TransportError，重试循环会禁用一条完全健康的渠道。
		if c.Request.Context().Err() != nil {
			return nil, types.NewClientGone()
		}
		return nil, types.NewTransportError("读取上游响应失败: " + err.Error())
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, body)
	info.AddResponseWritten(len(body))
	return nil, nil
}

func (a *Adaptor) GetChannelName() string { return "OpenAI" }

// upstreamErrorMessage 尽量从 OpenAI 错误体里取 message，取不到就截断原文。
// 截断是为了让错误响应可读，也避免把一大段 HTML 原样转给客户端。
func upstreamErrorMessage(raw []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := common.Unmarshal(raw, &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	if msg == "" {
		return "上游返回错误响应"
	}
	return msg
}
