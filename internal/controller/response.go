// Package controller 是 HTTP handler 层。
//
// /api（管理面）与 /v1（转发面）的错误形状从第一天就不同：
// 管理面用这里的 {success, message, data} 信封（将来 message 旁边加 message_key 供 i18n），
// 转发面将来是 OpenAI/Claude 形状的错误 JSON。两套形状各自演进，不互相将就。
package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// APIResponse 是管理面的统一信封。
type APIResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// APISuccess 返回成功信封。data 为 nil 时是合法的"无数据成功"。
func APISuccess(c *gin.Context, data any) {
	c.JSON(http.StatusOK, APIResponse{Success: true, Message: "", Data: data})
}

// APIError 返回失败信封。message 是给人读的；
// 将来接 i18n 时在旁边加 message_key，而不是改 message 的内容。
func APIError(c *gin.Context, status int, message string) {
	c.JSON(status, APIResponse{Success: false, Message: message, Data: nil})
}
