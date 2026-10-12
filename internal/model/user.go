package model

// 贯穿本包的五条约定（设计文档 §5.5）：
//  1. 时间戳统一 int64 unix 秒——三库行为一致、前端 JS 不丢精度，不用 time.Time；
//  2. 金额/额度统一 int64 整数，永不 float（钱包上限 2^53-1 超出 int32，必须 int64）；
//  3. JSON 型字段一律 TEXT，Go 侧以 string 承载、显式编解码，不碰数据库特有 JSON 类型；
//  4. 业务默认值写在代码里，不写进 GORM tag——gorm:"default:..." 会让 AutoMigrate
//     每次重启发 ALTER TABLE，且三库对布尔默认值处理不一致；
//  5. 主键交给 GORM（gorm:"primaryKey"），不手写 AUTO_INCREMENT / SERIAL。
//
// 列名是 ABI 的一部分：TestSchemaMatchesBlueprint 把它们钉住了。

// User 是账号主体。字段严格照 blueprint §4 的最小 DDL，不自行扩列
// （加列是 M3 管理面的事，AutoMigrate 会处理）。
type User struct {
	ID           int    `gorm:"primaryKey"`
	Username     string `gorm:"size:64;uniqueIndex"`
	PasswordHash string `gorm:"size:128"`
	Role         int    `gorm:"index"` // constant.RoleUser / RoleAdmin / RoleRoot
	Status       int    `gorm:"index"` // constant.UserStatusEnabled / Disabled
	Quota        int64  // 钱包余额（quota）
	UsedQuota    int64  // 累计消耗
	GroupName    string `gorm:"size:64;index"`
	Email        string `gorm:"size:128;index"`
	AuthVersion  int    // 改密码/登出时 +1，用来让旧会话立即失效
	CreatedAt    int64  `gorm:"autoCreateTime"`
}
