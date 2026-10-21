package helper

import (
	"strings"

	"github.com/yyyCode/whats-api/internal/common"
	"github.com/yyyCode/whats-api/internal/logger"
)

// MapModel 应用渠道的模型映射（channels.model_mapping，形如 {"客户端名":"上游名"}）。
//
// 映射是"可选优化"而不是"必须正确"：JSON 坏了就按原模型名转发并告警，
// 因为拒绝整个请求的代价远大于一次映射失效。
func MapModel(originModel, mappingJSON string) (string, bool) {
	if strings.TrimSpace(mappingJSON) == "" {
		return originModel, false
	}
	var mapping map[string]string
	if err := common.UnmarshalString(mappingJSON, &mapping); err != nil {
		logger.Warn("渠道 model_mapping 不是合法 JSON，按原模型名转发", "error", err)
		return originModel, false
	}
	if upstream, ok := mapping[originModel]; ok && strings.TrimSpace(upstream) != "" {
		return upstream, true
	}
	return originModel, false
}
