package model

// Ability 是反范式选路索引：一行 = (分组, 模型, 渠道)。
//
// 它的价值是"把查询结果预先存下来"——选渠道时直接按 (group, model) 取出候选集，
// 而不是每次 JOIN channels 再解析 models 字段。
// 代价是它会漂移，所以必须配一个整表重建函数（RebuildAbilities）——
// 那是 M4 的职责：M0 没有渠道写入路径，它没有数据源，先不写空壳。
type Ability struct {
	GroupName string `gorm:"primaryKey;size:64"`
	ModelName string `gorm:"column:model_name;primaryKey;size:128"`
	ChannelID int    `gorm:"column:channel_id;primaryKey"`
	// Enabled 不写 default tag：三库对布尔默认值处理不一致，写入方显式赋值。
	Enabled  bool
	Priority int64
	Weight   uint
	Tag      string `gorm:"size:64"`
}
