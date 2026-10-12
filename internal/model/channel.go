package model

// Channel 是上游渠道（运营单位）：一个渠道 = 一组凭证 + 一份模型清单 + 一组覆盖规则。
type Channel struct {
	ID      int    `gorm:"primaryKey"`
	Type    int    `gorm:"index"`                // 持久化枚举：上游类型（ABI，只增不改）
	Key     string `gorm:"column:key;type:text"` // 上游密钥（可能多个，逗号分隔）
	BaseURL string `gorm:"column:base_url;size:255"`
	// Models 是逗号分隔的模型列表。
	Models    string `gorm:"type:text"`
	GroupName string `gorm:"size:64;index"`
	Priority  int64
	// Weight 为 0 是合法值（表示不参与加权随机），所以不能靠默认值区分"未设置"。
	Weight uint
	Tag    string `gorm:"size:64;index"`
	Status int    `gorm:"index"` // constant.ChannelStatus*
	// 四个 TEXT 列是"不用改代码就能接新供应商"的关键：模型映射、参数覆盖、请求头覆盖、渠道私有设置。
	ModelMapping   string `gorm:"column:model_mapping;type:text"`
	ParamOverride  string `gorm:"column:param_override;type:text"`
	HeaderOverride string `gorm:"column:header_override;type:text"`
	Setting        string `gorm:"type:text"`
	CreatedAt      int64  `gorm:"autoCreateTime"`
}
