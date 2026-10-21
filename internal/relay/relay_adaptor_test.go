package relay

import (
	"testing"

	"github.com/yyyCode/whats-api/internal/constant"
)

func TestGetAdaptor(t *testing.T) {
	if GetAdaptor(constant.APITypeOpenAI) == nil {
		t.Error("OpenAI 类型必须有适配器")
	}
	if GetAdaptor(9999) != nil {
		t.Error("未知类型应返回 nil，由调用方给出明确错误")
	}
}
