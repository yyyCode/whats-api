// Package types 存放跨层共享的转发面类型。
//
// M1 只有一种：统一错误。它的价值在于把三件事绑在一起——
// 给客户端看的形状、给控制流用的可重试标志、给日志用的可读描述。
// 绝不拿错误字符串做控制流：重试与自动禁用只读这里的两个布尔值。
package types

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// APIError 是转发面唯一的错误类型。
type APIError struct {
	// StatusCode 是写给客户端的 HTTP 状态码；0 表示"不要写"（客户端已断开）。
	StatusCode int
	Message    string
	Type       string
	Code       string

	// Retryable 表示"还没向客户端写出任何字节时，值得换一个渠道再试"。
	Retryable bool
	// ChannelError 表示错误归因于渠道，可以自动禁用。
	ChannelError bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (status=%d type=%s code=%s)", e.Message, e.StatusCode, e.Type, e.Code)
}

// Write 按 OpenAI 的错误形状写给客户端。
func (e *APIError) Write(c *gin.Context) {
	if e == nil || e.StatusCode == 0 {
		return
	}
	c.JSON(e.StatusCode, gin.H{"error": gin.H{
		"message": e.Message,
		"type":    e.Type,
		"code":    e.Code,
	}})
}

// NewBadRequest 用于请求本身的问题（缺字段、JSON 非法、请求过大）。
func NewBadRequest(message string) *APIError {
	return &APIError{
		StatusCode: http.StatusBadRequest,
		Message:    message,
		Type:       "invalid_request_error",
		Code:       "invalid_request",
	}
}

// NewRequestTooLarge 单独一个构造函数：状态码不同（413），且调用方要能一眼看出。
func NewRequestTooLarge() *APIError {
	return &APIError{
		StatusCode: http.StatusRequestEntityTooLarge,
		Message:    "请求体过大",
		Type:       "invalid_request_error",
		Code:       "request_too_large",
	}
}

// NewAuthenticationError：没有令牌、令牌无效/停用/过期、用户不存在。
func NewAuthenticationError(message string) *APIError {
	return &APIError{
		StatusCode: http.StatusUnauthorized,
		Message:    message,
		Type:       "authentication_error",
		Code:       "invalid_api_key",
	}
}

// NewPermissionError：令牌有效，但不能做这件事（模型不在白名单、用户被停用）。
func NewPermissionError(message string) *APIError {
	return &APIError{
		StatusCode: http.StatusForbidden,
		Message:    message,
		Type:       "permission_error",
		Code:       "permission_denied",
	}
}

// NewQuotaError 对应 OpenAI 的 insufficient_quota。
func NewQuotaError(message string) *APIError {
	return &APIError{
		StatusCode: http.StatusForbidden,
		Message:    message,
		Type:       "insufficient_quota",
		Code:       "insufficient_quota",
	}
}

// NewNoChannelError：分组不匹配或该模型没有可用渠道。
// 503 而不是 404：这是"暂时没有可用的上游"，不是"这个模型永远不存在"。
func NewNoChannelError(modelName string) *APIError {
	return &APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    fmt.Sprintf("模型 %q 当前没有可用渠道", modelName),
		Type:       "server_error",
		Code:       "no_channel",
	}
}

// NewUpstreamError 保留上游状态码，并按状态码判定是否值得重试：
// 5xx 与 429 归因于渠道；其余 4xx 是请求本身的问题，重试只会放大故障。
func NewUpstreamError(status int, message string) *APIError {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	retryable := status == http.StatusTooManyRequests || status >= 500
	return &APIError{
		StatusCode:   status,
		Message:      message,
		Type:         "upstream_error",
		Code:         "upstream_error",
		Retryable:    retryable,
		ChannelError: retryable,
	}
}

// NewTransportError：连不上、DNS 失败、响应头超时。归因于渠道。
func NewTransportError(message string) *APIError {
	return &APIError{
		StatusCode:   http.StatusBadGateway,
		Message:      message,
		Type:         "upstream_error",
		Code:         "upstream_unreachable",
		Retryable:    true,
		ChannelError: true,
	}
}

// NewInternalError：网关自身的故障（配置没有适配器、查库失败）。
func NewInternalError(message string) *APIError {
	return &APIError{
		StatusCode: http.StatusInternalServerError,
		Message:    message,
		Type:       "server_error",
		Code:       "internal_error",
	}
}

// NewClientGone：客户端在流式过程中断开。
// StatusCode 为 0 —— 连接都没了，写响应既无意义也会掩盖真实原因。
func NewClientGone() *APIError {
	return &APIError{Message: "客户端已断开", Type: "client_gone", Code: "client_gone"}
}
