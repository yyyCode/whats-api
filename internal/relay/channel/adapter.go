// Package channel 定义上游适配器接口与所有适配器共用的出站传输。
//
// 适配器只做四件事：转换请求、算出 URL、拼认证头、解析响应。
// 它不读数据库、不读全局配置、不算钱——需要什么由 RelayInfo 传入。
package channel

import (
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	relaycommon "github.com/yyyCode/whats-api/internal/relay/common"
	"github.com/yyyCode/whats-api/internal/types"
)

// Adaptor 是一个上游供应商的实现。
//
// M1 只做 Chat Completions，所以接口只有一条转换路径；
// 协议互转（Claude/Gemini）在 M6 触发，届时按 03 篇扩展成一组 Convert*。
type Adaptor interface {
	// Init 每次尝试开始时调用（重试会重新调用）。
	Init(info *relaycommon.RelayInfo)

	// ConvertRequest 下游 OpenAI 请求 → 上游请求。M1 是透传 + 模型映射。
	ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody []byte) ([]byte, error)

	// GetRequestURL 与 SetupRequestHeader 负责定位与认证。
	GetRequestURL(info *relaycommon.RelayInfo) (string, error)
	SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error

	// DoRequest 发 HTTP。绝大多数实现直接复用 DoApiRequest。
	DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (*http.Response, error)

	// DoResponse 读上游响应、转发给客户端、返回 usage 供计费（M1 丢弃 usage）。
	DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.APIError)

	GetChannelName() string
}

// sharedClient 是所有适配器共用的出站客户端。
//
// ★ 刻意不设 Client.Timeout：SSE 响应可能持续几分钟，总超时会把正常的流掐断。
// 用 ResponseHeaderTimeout 卡住"上游一直不回响应头"，用拨号超时卡住"连不上"。
var sharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// DoApiRequest 是所有适配器共用的 POST 实现。
//
// ★ 请求绑定 c.Request.Context()：客户端断开时，上游请求会被一起取消。
// 这是"客户端走了就别继续烧 token"的第一道保障，不需要额外的心跳检测。
func DoApiRequest(a Adaptor, c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (*http.Response, error) {
	url, err := a.GetRequestURL(info)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	if err := a.SetupRequestHeader(c, &req.Header, info); err != nil {
		return nil, err
	}
	return sharedClient.Do(req)
}
