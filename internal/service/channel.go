package service

import (
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/logger"
	"github.com/yyyCode/whats-api/internal/model"
)

// DisableChannel 是自动禁用的日志外壳。
// 它不返回错误：调用点在重试路径上，禁用失败不该改变"这次请求已经失败"的事实。
func DisableChannel(id int) {
	ok, err := model.DisableChannel(id)
	if err != nil {
		logger.Error("自动禁用渠道失败", "channel_id", id, "error", err)
		return
	}
	if ok {
		logger.Warn("渠道已自动禁用，后续请求会跳过",
			"channel_id", id, "status", constant.ChannelStatusAutoDisabled)
	}
}
