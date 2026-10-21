package types

import "testing"

func TestUsageValid(t *testing.T) {
	cases := []struct {
		name  string
		usage *Usage
		want  bool
	}{
		{"nil", nil, false},
		{"全零", &Usage{}, false},
		{"只有 prompt", &Usage{PromptTokens: 7}, true},
		{"只有 completion", &Usage{CompletionTokens: 3}, true},
		{"prompt 与 completion 都有", &Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, true},
	}
	for _, c := range cases {
		if got := c.usage.Valid(); got != c.want {
			t.Errorf("%s: Valid() = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestUsageMergeNonZero(t *testing.T) {
	cases := []struct {
		name string
		base Usage
		next *Usage
		want Usage
	}{
		{
			name: "后到的非零字段覆盖",
			base: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			next: &Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33},
			want: Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33},
		},
		{
			name: "后到的零值不抹掉已有值",
			base: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			next: &Usage{},
			want: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name: "零值字段不覆盖、非零字段覆盖",
			base: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			next: &Usage{CompletionTokens: 25},
			want: Usage{PromptTokens: 10, CompletionTokens: 25, TotalTokens: 30},
		},
		{
			name: "合并 nil 不变",
			base: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			next: nil,
			want: Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name: "合并进零值即拷贝非零字段",
			base: Usage{},
			next: &Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11},
			want: Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11},
		},
	}
	for _, c := range cases {
		c.base.MergeNonZero(c.next)
		if c.base != c.want {
			t.Errorf("%s: 合并后 = %+v，期望 %+v", c.name, c.base, c.want)
		}
	}
}

func TestUsageMergeNonZeroOnNilReceiver(t *testing.T) {
	var u *Usage
	u.MergeNonZero(&Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	if u.Valid() {
		t.Error("nil 接收者合并后不应变成有效 usage")
	}
}
