package model

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
