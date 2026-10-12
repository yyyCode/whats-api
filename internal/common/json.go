package common

import "encoding/json"

// JSON 编解码的统一入口。
//
// 不要直接用 encoding/json：全项目只经过这几个函数进出，
// 将来换成 sonic 之类的实现时才只需要改这一个文件。

func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func MarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// UnmarshalString 是 Unmarshal 的字符串版本：库里大量 JSON 存在 TEXT 字段里。
func UnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// MustMarshalString 用于"几乎不会失败"的场景（基础类型构成的 map/结构体）。
// 失败时返回空字符串而不是 panic：日志与配置序列化不该把进程带走。
func MustMarshalString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
