package types

// Usage 是上游返回的 token 用量，也是计费的唯一依据。
// 零值不是"免费"，而通常意味着上游没有给用量（超时、连接被断开等）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Valid 判断 usage 是否可用于计费。
// nil 与全零都无效：零用量是异常信号（上游超时/未返回），不是免费服务。
func (u *Usage) Valid() bool {
	return u != nil && (u.PromptTokens > 0 || u.CompletionTokens > 0)
}

// MergeNonZero 用后到的非零字段覆盖当前值，后到的零值不覆盖。
// 某些兼容网关会把累积 usage 放在倒数第二个 SSE 帧，
// 最后一帧的 usage 往往是全零；若零值也覆盖，前面已采集到的用量就会被抹掉。
func (u *Usage) MergeNonZero(next *Usage) {
	if u == nil || next == nil {
		return
	}
	if next.PromptTokens != 0 {
		u.PromptTokens = next.PromptTokens
	}
	if next.CompletionTokens != 0 {
		u.CompletionTokens = next.CompletionTokens
	}
	if next.TotalTokens != 0 {
		u.TotalTokens = next.TotalTokens
	}
}
