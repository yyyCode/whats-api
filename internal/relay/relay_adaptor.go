// Package relay 是转发内核的装配层：把 channels.type 映射成适配器实现。
//
// 用朴素的 switch 而不是注册表：供应商实现由我们自己发版，
// 编译期完整性与可读性比运行时注册更值钱（03 篇 §3）。
package relay

import (
	"github.com/yyyCode/whats-api/internal/constant"
	"github.com/yyyCode/whats-api/internal/relay/channel"
	"github.com/yyyCode/whats-api/internal/relay/channel/openai"
)

func GetAdaptor(apiType int) channel.Adaptor {
	switch apiType {
	case constant.APITypeOpenAI:
		return &openai.Adaptor{}
	default:
		return nil
	}
}
