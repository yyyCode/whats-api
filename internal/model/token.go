package model

// Token 是转发用的 API Key：它同时是权限载体与限额载体。
type Token struct {
	ID     int    `gorm:"primaryKey"`
	Key    string `gorm:"column:key;size:64;uniqueIndex"` // 转发时携带的密钥
	UserID int    `gorm:"index"`
	Status int    `gorm:"index"` // constant.TokenStatus*
	// ExpiredTime 是 unix 秒；-1 表示永不过期（与 New API 的约定一致）。
	ExpiredTime int64
	RemainQuota int64
	// UnlimitedQuota 为真时忽略 RemainQuota（管理员令牌）。
	UnlimitedQuota     bool
	ModelLimitsEnabled bool
	// ModelLimits 是逗号分隔或 JSON 的模型白名单，用 TEXT 存。
	ModelLimits string `gorm:"type:text"`
	// AllowIPs 是逗号分隔的允许来源 IP；列名必须是 allow_ips。
	AllowIPs   string `gorm:"column:allow_ips;type:text"`
	UsedQuota  int64
	GroupName  string `gorm:"size:64;index"`
	AccessedAt int64
	CreatedAt  int64 `gorm:"autoCreateTime"`
}
