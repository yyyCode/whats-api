package helper

import "testing"

func TestMapModel(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		mapping string
		want    string
		mapped  bool
	}{
		{"命中", "gpt-alias", `{"gpt-alias":"gpt-4o"}`, "gpt-4o", true},
		{"未命中", "gpt-4o", `{"gpt-alias":"gpt-4o"}`, "gpt-4o", false},
		{"空映射", "gpt-4o", "", "gpt-4o", false},
		{"非法 JSON 回退原值", "gpt-4o", `{oops`, "gpt-4o", false},
		{"映射到空串视为未命中", "gpt-alias", `{"gpt-alias":""}`, "gpt-alias", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, mapped := MapModel(c.origin, c.mapping)
			if got != c.want || mapped != c.mapped {
				t.Errorf("MapModel(%q, %q) = (%q, %v)，期望 (%q, %v)",
					c.origin, c.mapping, got, mapped, c.want, c.mapped)
			}
		})
	}
}
