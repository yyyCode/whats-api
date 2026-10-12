// Package cachex 提供"Redis 可用则用 Redis、不可用则退化为进程内实现"的缓存原语。
//
// 单机部署与多实例部署共用同一份代码，代价是必须把两种后端的语义差异
// （TTL 精度、原子性）封在实现内部，不让调用方感知。
package cachex

import (
	"strconv"
	"sync"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

// Counter 是带 TTL 的原子计数器。
//
// 它与"键值缓存"分开建模，因为语义不同：计数器关心的是原子自增与滑动过期，
// 而键值缓存关心的是编解码与 TTL 精度。混成一个类型会让两边都别扭。
type Counter struct {
	namespace string
	ttl       time.Duration
	memory    *memoryCounter
}

func NewCounter(namespace string, ttl time.Duration) *Counter {
	return &Counter{
		namespace: common.CacheNamespacePrefix + ":" + namespace,
		ttl:       ttl,
		memory:    newMemoryCounter(),
	}
}

// useRedis 每次调用都重新判断，而不是在构造时固化：
// Redis 中途掉线后代码应继续工作（降级），而不是整片报错。
func (c *Counter) useRedis() bool { return common.RedisEnabled && common.RDB != nil }

func (c *Counter) key(key string) string { return c.namespace + ":" + key }

// Backend 报告当前实际后端，供 /api/status 与运维排查使用。
func (c *Counter) Backend() string {
	if c.useRedis() {
		return "redis"
	}
	return "memory"
}

// Incr 原子自增并返回自增后的值。Redis 路径失败时回落到内存路径，
// 保证调用方永远拿得到结果（计数精度不如可用性重要）。
func (c *Counter) Incr(key string) (int64, error) {
	if c.useRedis() {
		if v, err := common.RedisIncr(c.key(key), c.ttl); err == nil {
			return v, nil
		}
	}
	return c.memory.Incr(key, c.ttl), nil
}

// Get 读取当前值。内存后端的 TTL 是滑动窗口（与 Redis 路径的 EXPIRE 语义一致）。
func (c *Counter) Get(key string) (int64, bool) {
	if c.useRedis() {
		raw, err := common.RedisGet(c.key(key))
		if err != nil {
			return 0, false
		}
		v, err := parseCounter(raw)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return c.memory.Get(key)
}

// memoryCounter 是进程内实现。不需要精确的过期语义（惰性判断即可）：
// 多实例一致性靠定时同步，不靠缓存。
type memoryCounter struct {
	mu   sync.Mutex
	data map[string]counterEntry
}

type counterEntry struct {
	value     int64
	expiresAt time.Time
}

func newMemoryCounter() *memoryCounter {
	return &memoryCounter{data: map[string]counterEntry{}}
}

func (m *memoryCounter) Incr(key string, ttl time.Duration) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	e, ok := m.data[key]
	if !ok || (ttl > 0 && !e.expiresAt.IsZero() && now.After(e.expiresAt)) {
		e.value = 0 // 已过期：窗口重开
	}
	e.value++
	if ttl > 0 {
		e.expiresAt = now.Add(ttl) // 与 Redis 的 INCR+EXPIRE 一致：滑动窗口
	} else {
		e.expiresAt = time.Time{}
	}
	m.data[key] = e
	return e.value
}

func (m *memoryCounter) Get(key string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.data[key]
	if !ok {
		return 0, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(m.data, key)
		return 0, false
	}
	return e.value, true
}

func parseCounter(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}
