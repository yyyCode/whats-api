package model

// Option 是运行时配置的持久化形态：key → JSON 编码的值。
//
// 表里只存"管理员显式改过"的项，默认值来自代码里的注册结构体
// （见 internal/setting/config）。这样将来调默认值能影响所有未显式修改的实例。
type Option struct {
	// key 是三库的关键字，列名必须显式指定；GORM 在生成的 DDL 里会给它加引号。
	Key   string `gorm:"column:key;primaryKey;size:128"`
	Value string `gorm:"type:text"`
}

// OptionMap 是内存中的配置镜像；初始化路径见 Task 3 的 InitOptionMap。
var OptionMap = map[string]string{}
