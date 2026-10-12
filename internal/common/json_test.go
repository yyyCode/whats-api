package common

import "testing"

type jsonSample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	in := jsonSample{Name: "渠道", Count: 3}
	raw, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}

	var out jsonSample
	if err := Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if out != in {
		t.Errorf("往返后不一致：%+v vs %+v", out, in)
	}
}

// 库里有大量 JSON 存在 TEXT 字段里，读的时候是字符串。
func TestUnmarshalString(t *testing.T) {
	var out jsonSample
	if err := UnmarshalString(`{"name":"a","count":1}`, &out); err != nil {
		t.Fatalf("UnmarshalString 失败: %v", err)
	}
	if out.Name != "a" || out.Count != 1 {
		t.Errorf("解析结果错误：%+v", out)
	}
}

func TestUnmarshalInvalidReturnsError(t *testing.T) {
	var out jsonSample
	if err := UnmarshalString(`{"name":`, &out); err == nil {
		t.Fatal("非法 JSON 应该返回错误")
	}
	if err := Unmarshal([]byte("not json"), &out); err == nil {
		t.Fatal("非法 JSON 应该返回错误")
	}
}

func TestMarshalIndent(t *testing.T) {
	raw, err := MarshalIndent(jsonSample{Name: "a", Count: 1})
	if err != nil {
		t.Fatalf("MarshalIndent 失败: %v", err)
	}
	want := "{\n  \"name\": \"a\",\n  \"count\": 1\n}"
	if string(raw) != want {
		t.Errorf("MarshalIndent = %q，期望 %q", raw, want)
	}
}

// 序列化失败不能让进程崩：日志与配置序列化不该把服务带走。
func TestMustMarshalStringReturnsEmptyOnFailure(t *testing.T) {
	if got := MustMarshalString(make(chan int)); got != "" {
		t.Errorf("不可序列化的值应返回空字符串，得到 %q", got)
	}
	if got := MustMarshalString(jsonSample{Name: "a", Count: 1}); got != `{"name":"a","count":1}` {
		t.Errorf("正常序列化结果错误: %q", got)
	}
}
