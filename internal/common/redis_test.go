package common

import (
	"context"
	"errors"
	"testing"

	"github.com/go-redis/redis/v8"
)

// resetRedis 把 Redis 状态清成"未启用"。测试与实现同包，直接用未导出的变量，
// 不为测试在生产代码里开后门。
func resetRedis(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = CloseRedis()
		RedisConnString = ""
	})
	_ = CloseRedis()
}

// Redis 未启用时，底层函数必须明确报错，而不是空指针崩掉或静默返回零值——
// 静默返回零值会让上层"降级"变成"数据丢失"。
func TestRedisFunctionsFailClearlyWhenDisabled(t *testing.T) {
	resetRedis(t)

	if RedisEnabled() || LoadRedis() != nil {
		t.Fatal("前置条件不成立：此刻句柄应为空")
	}

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
// 同时验证它会把上一轮的句柄**关掉**：否则反复 Init 会漏掉连接池。
func TestInitRedisClientWithoutConfigDegrades(t *testing.T) {
	t.Cleanup(func() { _ = CloseRedis(); RedisConnString = "" })

	// 制造"上一轮已启用"的脏状态。这里的 client 从不真正连出去，
	// 本用例只关心句柄有没有被摘掉、有没有被关闭。
	stale := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	rdb.Store(stale)
	redisEnabled.Store(true)

	RedisConnString = ""
	InitRedisClient()

	if RedisEnabled() {
		t.Error("未配置 REDIS_CONN_STRING 时 RedisEnabled 必须为 false")
	}
	if LoadRedis() != nil {
		t.Error("未配置时不应留下 client")
	}
	if err := stale.Ping(context.Background()).Err(); err == nil {
		t.Error("旧 client 必须被关闭，否则每次 Init 都漏一个连接池")
	}
}

// 配置了一个连不上的地址：必须降级而不是启动失败——这是第二条验收标准。
func TestInitRedisClientUnreachableDegrades(t *testing.T) {
	t.Cleanup(func() { _ = CloseRedis(); RedisConnString = "" })

	RedisConnString = "127.0.0.1:1" // 保留端口，必然连不上
	InitRedisClient()

	if RedisEnabled() {
		t.Error("连接失败时 RedisEnabled 必须为 false")
	}
	if LoadRedis() != nil {
		t.Error("连接失败时不应留下 client")
	}
}

// 关闭后状态必须一并清空：否则调用方拿到的是驱动层的 "client is closed"，
// 一个没法用 errors.Is 识别的错误。Close 必须幂等。
func TestCloseRedisClearsState(t *testing.T) {
	t.Cleanup(func() { RedisConnString = "" })

	rdb.Store(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}))
	redisEnabled.Store(true)

	if err := CloseRedis(); err != nil {
		t.Errorf("CloseRedis 应成功，得到 %v", err)
	}
	if RedisEnabled() || LoadRedis() != nil {
		t.Error("CloseRedis 之后必须报告未启用")
	}
	if _, err := RedisGet("k"); !errors.Is(err, ErrRedisDisabled) {
		t.Errorf("关闭后应得到 ErrRedisDisabled，得到 %v", err)
	}
	if err := CloseRedis(); err != nil {
		t.Errorf("CloseRedis 必须幂等，第二次调用报错 %v", err)
	}
}

func TestNormalizeRedisURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"redis://127.0.0.1:6379", "redis://127.0.0.1:6379"},
		{"rediss://host:6380", "rediss://host:6380"},
		{"  127.0.0.1:6379  ", "redis://127.0.0.1:6379"},
	}
	for _, c := range cases {
		if got := normalizeRedisURL(c.in); got != c.want {
			t.Errorf("normalizeRedisURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
