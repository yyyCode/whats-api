package model

import (
	"errors"
	"fmt"
)

// Log 是账本的一行：审计凭证，只追加。
//
// 它落在 LOG_DB（即 LOG_SQL_DSN 指定的库），与业务库分开——
// 日志量压垮业务库是最典型的线上事故之一，接缝要在第一天就留好。
type Log struct {
	ID               int    `gorm:"primaryKey"`
	UserID           int    `gorm:"index"`
	Type             int    `gorm:"index"` // constant.LogType*（ABI）
	Content          string `gorm:"type:text"`
	ModelName        string `gorm:"column:model_name;size:128;index"`
	Quota            int64
	PromptTokens     int
	CompletionTokens int
	UseTime          int // 秒
	IsStream         bool
	ChannelID        int    `gorm:"index"`
	GroupName        string `gorm:"size:64"`
	TokenName        string `gorm:"size:64"`
	IP               string `gorm:"column:ip;size:64"`
	RequestID        string `gorm:"column:request_id;size:64;index"`
	// Other 存计费上下文等扩展字段（JSON），用 TEXT 而不是数据库特有 JSON 类型。
	Other     string `gorm:"type:text"`
	CreatedAt int64  `gorm:"autoCreateTime"`
}

// WriteLog 同步写一条日志。
//
// 消费日志是审计凭证：它记录的是「这笔钱当时为什么这么算」，丢了就无法对账，
// 所以必须同步写、且写入失败要能被调用方看见。余额更新可以合批/延迟，
// 日志不行——账本与日志可以短暂不一致，但日志不能凭空少一行。
func WriteLog(l *Log) error {
	if l == nil {
		return errors.New("日志不能为空")
	}
	if LOG_DB == nil {
		return errors.New("日志库未初始化")
	}
	if err := LOG_DB.Create(l).Error; err != nil {
		return fmt.Errorf("写入日志失败: %w", err)
	}
	return nil
}
