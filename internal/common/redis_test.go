package common

import (
	"errors"
	"testing"
)

// Redis 未启用时，底层函数必须明确报错，而不是空指针崩掉或静默返回零值——
// 静默返回零值会让上层"降级"变成"数据丢失"。
func TestRedisFunctionsFailClearlyWhenDisabled(t *testing.T) {
	RedisEnabled = false
	RDB = nil

	if _, err := RedisGet("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisGet 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if err := RedisSet("k", "v", 0); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisSet 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if err := RedisDel("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisDel 应返回 ErrRedisDisabled，得到 %v", err)
	}
	if _, err := RedisIncr("k", 0); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("RedisIncr 应返回 ErrRedisDisabled，得到 %v", err)
	}
}

// 未配置 REDIS_CONN_STRING 时 InitRedisClient 必须安静降级，不能 panic、不能卡住。
func TestInitRedisClientWithoutConfigDegrades(t *testing.T) {
	RedisEnabled = true // 故意留下脏状态，验证 Init 会把它清掉
	RedisConnString = ""
	t.Cleanup(func() { RedisEnabled = false; RDB = nil })

	InitRedisClient()

	if RedisEnabled {
		t.Error("未配置 REDIS_CONN_STRING 时 RedisEnabled 必须为 false")
	}
	if RDB != nil {
		t.Error("未配置时不应建立连接")
	}
}

// 配置了一个连不上的地址：必须降级而不是启动失败——这是第二条验收标准。
func TestInitRedisClientUnreachableDegrades(t *testing.T) {
	RedisConnString = "127.0.0.1:1" // 保留端口，必然连不上
	t.Cleanup(func() { RedisEnabled = false; RDB = nil; RedisConnString = "" })

	InitRedisClient()

	if RedisEnabled {
		t.Error("连接失败时 RedisEnabled 必须为 false")
	}
	if RDB != nil {
		t.Error("连接失败时不应留下 client")
	}
}

func TestNormalizeRedisURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"redis://127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"rediss://host:6380", "rediss://host:6380"},
	}
	for _, c := range cases {
		if got := normalizeRedisURL(c.in); got != c.want {
			t.Errorf("normalizeRedisURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
