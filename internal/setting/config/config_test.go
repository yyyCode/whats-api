package config

import (
	"reflect"
	"strings"
	"testing"
)

type testSetting struct {
	Level  string  `json:"level"`
	Enable bool    `json:"enable"`
	Count  int     `json:"count"`
	Total  int64   `json:"total"`
	Factor float64 `json:"factor"`
}

func newTestSetting() testSetting {
	return testSetting{Level: "info", Enable: true, Count: 3, Total: 99, Factor: 1.5}
}

func TestRegisterAndExportDefaults(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	got := m.ExportAllConfigs()
	want := map[string]string{
		"test_setting.level":  `"info"`,
		"test_setting.enable": `true`,
		"test_setting.count":  `3`,
		"test_setting.total":  `99`,
		"test_setting.factor": `1.5`,
	}
	if len(got) != len(want) {
		t.Errorf("配置项数量 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ExportAllConfigs()[%q] = %q，期望 %q", k, got[k], v)
		}
	}

	if mods := m.Modules(); len(mods) != 1 || mods[0] != "test_setting" {
		t.Errorf("Modules() = %v，期望 [test_setting]", mods)
	}
	if n := m.FieldCount(); n != len(want) {
		t.Errorf("FieldCount() = %d，期望 %d", n, len(want))
	}
}

func TestUpdateConfigWritesAllSupportedTypes(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	rebuilds := 0
	m.OnRebuild("test_setting", func() { rebuilds++ })

	cases := []struct {
		key string
		raw string
	}{
		{"test_setting.level", `"debug"`},
		{"test_setting.enable", `false`},
		{"test_setting.count", `7`},
		{"test_setting.total", `123456789012`},
		{"test_setting.factor", `2.25`},
	}
	for _, c := range cases {
		if err := m.UpdateConfig(c.key, c.raw); err != nil {
			t.Fatalf("UpdateConfig(%q, %q) 失败: %v", c.key, c.raw, err)
		}
	}

	if s.Level != "debug" {
		t.Errorf("Level = %q，期望 debug", s.Level)
	}
	if s.Enable {
		t.Error("Enable 应为 false")
	}
	if s.Count != 7 {
		t.Errorf("Count = %d，期望 7", s.Count)
	}
	if s.Total != 123456789012 {
		t.Errorf("Total = %d，期望 123456789012", s.Total)
	}
	if s.Factor != 2.25 {
		t.Errorf("Factor = %v，期望 2.25", s.Factor)
	}
	// 每次成功写入都必须让快照重建，否则热路径读到的还是旧值。
	if rebuilds != len(cases) {
		t.Errorf("快照重建次数 = %d，期望 %d", rebuilds, len(cases))
	}

	// GetConfig 应能读回刚写入的值。
	if v, ok := m.GetConfig("test_setting.level"); !ok || v != `"debug"` {
		t.Errorf("GetConfig(level) = (%q, %v)，期望 (\"debug\", true)", v, ok)
	}
}

// 非法写入必须报错且不留副作用：管理端请求不能把运行中的配置改坏。
func TestUpdateConfigRejectsBadInputWithoutSideEffects(t *testing.T) {
	m := NewManager()
	s := newTestSetting()
	m.Register("test_setting", &s)

	rebuilds := 0
	m.OnRebuild("test_setting", func() { rebuilds++ })

	cases := []struct {
		name string
		key  string
		raw  string
	}{
		{"未注册的模块", "nope.level", `"x"`},
		{"未注册的字段", "test_setting.nope", `"x"`},
		{"key 缺少模块前缀", "level", `"x"`},
		{"key 为空", "", `"x"`},
		{"整数项给了字符串", "test_setting.count", `"not-a-number"`},
		{"字符串项给了数字", "test_setting.level", `123`},
		{"布尔项给了对象", "test_setting.enable", `{}`},
		{"浮点项给了数组", "test_setting.factor", `[]`},
		{"整数项给了 null", "test_setting.count", `null`},
		{"字符串项给了 null", "test_setting.level", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := m.UpdateConfig(c.key, c.raw); err == nil {
				t.Fatalf("UpdateConfig(%q, %q) 应该报错", c.key, c.raw)
			}
		})
	}

	if s != newTestSetting() {
		t.Errorf("失败的写入改变了配置值：%+v", s)
	}
	if rebuilds != 0 {
		t.Errorf("失败的写入不应触发快照重建，得到 %d 次", rebuilds)
	}
}

// ValidateConfig 是"先校验、后落库"的前置检查（model.UpdateOption 依赖它）。
// 它必须同时满足两件事：(a) 不改变任何状态；(b) 接受集合与 UpdateConfig 完全一致。
func TestValidateConfigDoesNotWriteAndAgreesWithUpdate(t *testing.T) {
	newManager := func(rebuilds *int) (*Manager, *testSetting) {
		m := NewManager()
		s := newTestSetting()
		m.Register("test_setting", &s)
		m.OnRebuild("test_setting", func() { *rebuilds++ })
		return m, &s
	}

	valid := []struct{ key, raw string }{
		{"test_setting.level", `"debug"`},
		{"test_setting.enable", `false`},
		{"test_setting.count", `42`},
		{"test_setting.total", `9007199254740991`},
		{"test_setting.factor", `1.25`},
	}
	invalid := []struct{ key, raw string }{
		{"test_setting.nope", `"x"`},
		{"nope.level", `"x"`},
		{"level", `"x"`},
		{"", `"x"`},
		{"test_setting.count", `"not-a-number"`},
		{"test_setting.level", `123`},
		{"test_setting.enable", `{}`},
		{"test_setting.factor", `[]`},
		{"test_setting.count", `null`},
	}

	// (a) 校验不写任何东西：值不变、快照不重建
	rebuilds := 0
	m, s := newManager(&rebuilds)
	before := *s

	for _, c := range valid {
		if err := m.ValidateConfig(c.key, c.raw); err != nil {
			t.Errorf("ValidateConfig(%q, %q) 意外报错: %v", c.key, c.raw, err)
		}
	}
	for _, c := range invalid {
		if err := m.ValidateConfig(c.key, c.raw); err == nil {
			t.Errorf("ValidateConfig(%q, %q) 应该报错", c.key, c.raw)
		}
	}

	if *s != before {
		t.Errorf("ValidateConfig 改变了配置值：%+v → %+v", before, *s)
	}
	if rebuilds != 0 {
		t.Errorf("ValidateConfig 触发了快照重建 %d 次（它不该有任何副作用）", rebuilds)
	}

	// (b) 接受集合一致。这条断言挡住的是"校验通过但写入失败"的裂缝——
	// 那个裂缝会让 model.UpdateOption 把非法值先落进库，再被别的实例同步出去。
	rebuilds2 := 0
	m2, _ := newManager(&rebuilds2)

	for _, c := range valid {
		if err := m2.UpdateConfig(c.key, c.raw); err != nil {
			t.Errorf("ValidateConfig 放行但 UpdateConfig 拒绝: (%q, %q): %v", c.key, c.raw, err)
		}
	}
	for _, c := range invalid {
		if err := m2.UpdateConfig(c.key, c.raw); err == nil {
			t.Errorf("ValidateConfig 拒绝但 UpdateConfig 接受: (%q, %q)", c.key, c.raw)
		}
	}

	if rebuilds2 != len(valid) {
		t.Errorf("成功的写入应每次重建快照：得到 %d 次，期望 %d 次", rebuilds2, len(valid))
	}
}

// 注册期的用法错误一律 panic（fail-fast），而不是留到运行时。
func TestRegisterPanicsOnMisuse(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"重复注册", func() {
			m := NewManager()
			s := newTestSetting()
			m.Register("dup", &s)
			m.Register("dup", &s)
		}},
		{"传了结构体值而不是指针", func() {
			NewManager().Register("x", newTestSetting())
		}},
		{"传了 nil", func() {
			NewManager().Register("x", nil)
		}},
		{"模块名为空", func() {
			s := newTestSetting()
			NewManager().Register("", &s)
		}},
		{"字段缺少 json tag", func() {
			s := struct{ Level string }{}
			NewManager().Register("x", &s)
		}},
		// 用 reflect.StructOf 动态构造带重复 json tag 的类型：go vet 的 structtag
		// 检查会拒绝源码里静态写死的重复 tag，而这里要测的是 Register 的运行时行为。
		{"json tag 重复", func() {
			typ := reflect.StructOf([]reflect.StructField{
				{Name: "A", Type: reflect.TypeOf(""), Tag: reflect.StructTag(`json:"same"`)},
				{Name: "B", Type: reflect.TypeOf(""), Tag: reflect.StructTag(`json:"same"`)},
			})
			NewManager().Register("x", reflect.New(typ).Interface())
		}},
		{"不支持的字段类型", func() {
			s := struct {
				Tags []string `json:"tags"`
			}{}
			NewManager().Register("x", &s)
		}},
		{"OnRebuild 注册到不存在的模块", func() {
			NewManager().OnRebuild("nope", func() {})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("应该 panic")
				}
			}()
			c.fn()
		})
	}
}

// 多个模块各自的 key 不能互相污染。
func TestMultipleModulesAreIsolated(t *testing.T) {
	m := NewManager()
	a := struct {
		Level string `json:"level"`
	}{Level: "a"}
	b := struct {
		Level string `json:"level"`
	}{Level: "b"}
	m.Register("mod_a", &a)
	m.Register("mod_b", &b)

	if err := m.UpdateConfig("mod_a.level", `"changed"`); err != nil {
		t.Fatal(err)
	}
	if a.Level != "changed" {
		t.Errorf("mod_a.Level = %q，期望 changed", a.Level)
	}
	if b.Level != "b" {
		t.Errorf("mod_b 被误改：Level = %q", b.Level)
	}
	if v, _ := m.GetConfig("mod_b.level"); v != `"b"` {
		t.Errorf("GetConfig(mod_b.level) = %q，期望 \"b\"", v)
	}
	if v, ok := m.GetConfig("mod_missing.level"); ok {
		t.Errorf("未注册模块不该返回 ok，得到 %q", v)
	}
}

// 注册的名字出现在错误信息里，方便定位是谁注册错了。
func TestRegisterErrorMentionsField(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("应该 panic")
		}
		msg := r.(string)
		if !strings.Contains(msg, "json tag") {
			t.Errorf("panic 信息应说明缺少 json tag，实际：%s", msg)
		}
	}()
	s := struct{ Level string }{}
	NewManager().Register("x", &s)
}
