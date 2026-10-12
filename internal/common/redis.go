package common

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/yyyCode/whats-api/internal/logger"
)

// ErrRedisDisabled 表示 Redis 未启用（未配置，或配置了但连不上）。
// 调用方据此走进程内缓存——这正是"拿掉 Redis 也能启动"的实现方式。
var ErrRedisDisabled = errors.New("redis 未启用")

var (
	// RedisEnabled 为 true 时 RDB 可用。所有使用方都必须先看这个标志。
	RedisEnabled bool
	// RDB 在未启用时为 nil。
	RDB *redis.Client
)

// InitRedisClient 初始化 Redis。**非致命**：
//
//	未配置 REDIS_CONN_STRING → 直接用进程内缓存
//	配置了但解析失败/连不上 → 告警并降级为进程内缓存
//
// 两种失败都不阻断启动：缓存是加速手段，不该成为可用性的单点。
func InitRedisClient() {
	RedisEnabled = false
	if RDB != nil {
		_ = RDB.Close()
		RDB = nil
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

	RDB = client
	RedisEnabled = true
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

func CloseRedis() error {
	if RDB == nil {
		return nil
	}
	return RDB.Close()
}

func RedisGet(key string) (string, error) {
	if !RedisEnabled || RDB == nil {
		return "", ErrRedisDisabled
	}
	return RDB.Get(context.Background(), key).Result()
}

// RedisSet 写入键值并设 TTL。ttl <= 0 表示不过期。
func RedisSet(key, value string, ttl time.Duration) error {
	if !RedisEnabled || RDB == nil {
		return ErrRedisDisabled
	}
	return RDB.Set(context.Background(), key, value, ttl).Err()
}

func RedisDel(key string) error {
	if !RedisEnabled || RDB == nil {
		return ErrRedisDisabled
	}
	return RDB.Del(context.Background(), key).Err()
}

// RedisIncr 原子自增，并刷新 TTL（滑动窗口）。
// 走 pipeline 是为了让 INCR 与 EXPIRE 一次往返完成，避免"自增了但没设过期"的中间态。
func RedisIncr(key string, ttl time.Duration) (int64, error) {
	if !RedisEnabled || RDB == nil {
		return 0, ErrRedisDisabled
	}
	ctx := context.Background()
	pipe := RDB.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
