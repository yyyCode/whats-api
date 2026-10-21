package constant

// 上游类型（channels.type）。数值持久化在数据库里，属于 ABI：
// 只能追加，不能修改已有值。M1 只有 OpenAI 兼容一种。
const (
	APITypeOpenAI = 1
)
