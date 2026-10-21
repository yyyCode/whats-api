// Package relaycommon 是转发内核的请求上下文。
//
// ★ 核心纪律：状态显式分成 RequestState（跨尝试共享）与 AttemptState（每次重置）。
// 重试时清错一边的后果都很严重：清了 RequestState 会重复扣费/丢身份，
// 不清 AttemptState 会把上一个渠道的密钥、模型映射和写出的字节数带到下一个渠道。
package relaycommon

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yyyCode/whats-api/internal/types"
)

// DefaultPingInterval 是流式保活间隔。
// 中间的 Nginx/负载均衡通常 60s 无数据就断；10s 留出足够余量。
const DefaultPingInterval = 10 * time.Second

// AuthInfo 由鉴权中间件填充，handler 只读。
type AuthInfo struct {
	TokenID        int
	UserID         int
	TokenName      string
	Group          string
	UnlimitedQuota bool

	ModelLimitsEnabled bool
	ModelLimits        string
}

// gin.Context 的键。全部经本文件读写，不让字符串字面量散落在各层。
const (
	ContextKeyAuth      = "relay_auth"
	ContextKeyRequestID = "relay_request_id"
)

func SetAuthInfo(c *gin.Context, a *AuthInfo) { c.Set(ContextKeyAuth, a) }

func GetAuthInfo(c *gin.Context) *AuthInfo {
	if v, ok := c.Get(ContextKeyAuth); ok {
		if a, ok := v.(*AuthInfo); ok {
			return a
		}
	}
	return nil
}

// RequestPayload 是转发面需要从请求体读出的少数几个字段。
// 其余字段不解析——M1 是透传，解析得越少，丢字段/改形状的风险越小。
type RequestPayload struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// RequestState 跨尝试共享：身份、原始请求体、模型名、重试计数、最后一个错误。
type RequestState struct {
	RequestID       string
	TokenID         int
	UserID          int
	Group           string
	OriginModelName string
	IsStream        bool
	Body            []byte
	RetryIndex      int
	LastError       *types.APIError
	// StartTime 是请求开始的时刻，用于消费日志里的 UseTime（请求耗时）。
	// 它必须留在 RequestState：重试会重置 AttemptState，但一次请求的耗时
	// 应覆盖所有尝试，不能从最后一个渠道才开始计时。
	StartTime time.Time
}

// ChannelMeta 描述"这一次尝试选中的渠道"。
type ChannelMeta struct {
	ChannelID         int
	ChannelType       int
	ChannelName       string
	BaseURL           string
	APIKey            string
	UpstreamModelName string
	ModelMapped       bool
}

// AttemptState 每次尝试重置。
//
// ResponseWritten > 0 是"不能再重试"的唯一判据：客户端已经收到字节，
// 再接一段来自另一个渠道的内容就是错乱。流式请求在响应头发出时就置位。
type AttemptState struct {
	ChannelMeta
	ResponseWritten int64
	UpstreamStatus  int
	// Usage 是本次尝试从上游采集到的 token 用量。
	// 它必须属于 AttemptState 而不是 RequestState：ResetAttempt 会整体换成一个新的
	// AttemptState，这样重试到下一个渠道时不会把上一个渠道的 usage 带过来——
	// 否则一次失败的尝试可能凭别人的用量被计费。
	Usage *types.Usage
}

// StreamOptions 控制流式转发的时钟。做成字段而不是常量，
// 测试才能用 50ms 的保活间隔观察 ping。
type StreamOptions struct {
	PingInterval time.Duration
}

// RelayInfo 是一整次请求的上下文。
type RelayInfo struct {
	Request RequestState
	Attempt AttemptState
	Stream  StreamOptions
}

func GenRelayInfo(auth *AuthInfo, payload *RequestPayload, requestID string, body []byte) *RelayInfo {
	return &RelayInfo{
		Request: RequestState{
			RequestID:       requestID,
			TokenID:         auth.TokenID,
			UserID:          auth.UserID,
			Group:           auth.Group,
			OriginModelName: payload.Model,
			IsStream:        payload.Stream,
			Body:            body,
			StartTime:       time.Now(),
		},
		Stream: StreamOptions{PingInterval: DefaultPingInterval},
	}
}

// ResetAttempt 换渠道/重试时调用：只重置每次尝试的状态。
func (i *RelayInfo) ResetAttempt(meta ChannelMeta) {
	i.Attempt = AttemptState{ChannelMeta: meta}
}

// AddResponseWritten 累计已经写给客户端的字节数。
func (i *RelayInfo) AddResponseWritten(n int) {
	if n > 0 {
		i.Attempt.ResponseWritten += int64(n)
	}
}

func (i *RelayInfo) WroteResponse() bool { return i.Attempt.ResponseWritten > 0 }
