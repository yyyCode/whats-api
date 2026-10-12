package common

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/yyyCode/whats-api/internal/logger"
)

// ErrRedisDisabled 表示 Redis 未启用（没配置，或启动期就没连上）。
// 调用方据此走进程内缓存——这正是"拿掉 Redis 也能启动"的实现方式。
//
// 注意它**不**覆盖「启动时正常、运行中掉线」：那种情况下 RedisGet/RedisIncr
// 返回的是 redis 驱动的原始错误（连接失败等），不是这个哨兵。需要在运行期
// 降级的调用方（如 cachex.Counter）应当对任何 error 都回退，而不是只认哨兵。
var ErrRedisDisabled = errors.New("redis 未启用")

// 句柄写成 atomic：可用性在每个请求上都被读到，而 Init/Close 是写方。
// 万一将来加入运行期重连，普通变量的并发读写就是数据竞争（-race 会报），
// 不是"读到旧值"这么轻。LoadRedis 一次 Load 同时判定可用性与取句柄，
// 分两次读可能跨越一次重连，拿到"已判定可用但句柄已换"的错配。
var (
	redisEnabled atomic.Bool
	rdb          atomic.Pointer[redis.Client]
)

// LoadRedis 返回当前客户端；未启用时返回 nil。
func LoadRedis() *redis.Client {
	if !redisEnabled.Load() {
		return nil
	}
	return rdb.Load()
}

// RedisEnabled 报告是否有可用的客户端。
func RedisEnabled() bool { return LoadRedis() != nil }

// InitRedisClient 初始化 Redis。**非致命**：
//
//	未配置 REDIS_CONN_STRING → 直接用进程内缓存
//	配置了但解析失败/连不上 → 告警并降级为进程内缓存
//
// 两种失败都不阻断启动：缓存是加速手段，不该成为可用性的单点。
func InitRedisClient() {
	// 先摘掉旧句柄再探测：探测期间 LoadRedis 返回 nil，调用方走进程内缓存，
	// 不会拿到一个马上要被关掉的客户端。
	redisEnabled.Store(false)
	if old := rdb.Swap(nil); old != nil {
		_ = old.Close()
	}

	if RedisConnString == "" {
		logger.Info("未配置 REDIS_CONN_STRING，使用进程内缓存")
		return
	}

	opt, err := redis.ParseURL(normalizeRedisURL(RedisConnString))
	if err != nil {
		logger.Warn("REDIS_CONN_STRING 解析失败，降级为进程内缓存", "error", err)
		return
	}

	client := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("Redis 连接失败，降级为进程内缓存", "error", err)
		_ = client.Close()
		return
	}

	rdb.Store(client)
	redisEnabled.Store(true)
	logger.Info("Redis 已启用")
}

// normalizeRedisURL 允许写 "127.0.0.1:6379" 这种简写：
// redis.ParseURL 只认 URL 形式，而运维习惯写 host:port。
func normalizeRedisURL(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return s
	}
	return "redis://" + s
}

// RedisKeyCacheSeconds 是缓存键的默认 TTL：与配置同步频率一致，
// 这样"多节点最终一致"的窗口只有一个概念，而不是两个不同的时间常数。
func RedisKeyCacheSeconds() int { return SyncFrequency }

// CloseRedis 关闭客户端并清空句柄：关闭后 RedisEnabled() 返回 false，
// 后续调用拿到的是 ErrRedisDisabled，而不是"client is closed"这种
// 没法用 errors.Is 识别的驱动错误。
func CloseRedis() error {
	redisEnabled.Store(false)
	old := rdb.Swap(nil)
	if old == nil {
		return nil
	}
	return old.Close()
}

func RedisGet(key string) (string, error) {
	client := LoadRedis()
	if client == nil {
		return "", ErrRedisDisabled
	}
	return client.Get(context.Background(), key).Result()
}

// RedisSet 写入键值并设 TTL。ttl <= 0 表示不过期。
func RedisSet(key, value string, ttl time.Duration) error {
	client := LoadRedis()
	if client == nil {
		return ErrRedisDisabled
	}
	return client.Set(context.Background(), key, value, ttl).Err()
}

func RedisDel(key string) error {
	client := LoadRedis()
	if client == nil {
		return ErrRedisDisabled
	}
	return client.Del(context.Background(), key).Err()
}

// RedisIncr 原子自增，并刷新 TTL（滑动窗口）。
// 走 pipeline 是为了让 INCR 与 EXPIRE 一次往返完成，避免"自增了但没设过期"的中间态。
func RedisIncr(key string, ttl time.Duration) (int64, error) {
	client := LoadRedis()
	if client == nil {
		return 0, ErrRedisDisabled
	}
	ctx := context.Background()
	pipe := client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
