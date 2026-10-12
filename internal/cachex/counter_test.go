package cachex

import (
	"sync"
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

// useMemoryBackend 让 Counter 走内存路径。
//
// Counter 的判断依据是 common.RedisEnabled()，而 Redis 的开关状态是 common
// 的内部状态，cachex 够不着——只能通过公共 API 把它关掉。CloseRedis 正是
// 语义上最贴切的那个：关闭之后系统本来就该退化到内存。
func useMemoryBackend(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = common.CloseRedis() })
	_ = common.CloseRedis()
}

func TestCounterUsesMemoryBackendWhenRedisDisabled(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_requests", time.Minute)
	if c.Backend() != "memory" {
		t.Fatalf("Backend() = %q，期望 memory（这是第二条验收标准的观察点）", c.Backend())
	}

	if _, ok := c.Get("total"); ok {
		t.Fatal("未自增过就不该有值")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Incr("total"); err != nil {
			t.Fatalf("Incr 失败: %v", err)
		}
	}
	got, ok := c.Get("total")
	if !ok || got != 3 {
		t.Errorf("Get(total) = (%d, %v)，期望 (3, true)", got, ok)
	}
}

// 计数器必须是原子的：并发自增不能丢计数。
func TestCounterIncrIsAtomic(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_concurrent", time.Minute)
	const n = 100

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Incr("total"); err != nil {
				t.Errorf("Incr 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	got, _ := c.Get("total")
	if got != n {
		t.Errorf("并发自增 %d 次后 Get = %d，说明丢了计数", n, got)
	}
}

// 不同 key 互不干扰。
func TestCounterKeysAreIndependent(t *testing.T) {
	useMemoryBackend(t)

	c := NewCounter("test_keys", time.Minute)
	if _, err := c.Incr("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Incr("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Incr("b"); err != nil {
		t.Fatal(err)
	}

	if v, _ := c.Get("a"); v != 2 {
		t.Errorf("a = %d，期望 2", v)
	}
	if v, _ := c.Get("b"); v != 1 {
		t.Errorf("b = %d，期望 1", v)
	}
}

// TTL 是滑动窗口：每次自增都把过期时间往后推，与 Redis 的 INCR+EXPIRE 一致。
func TestCounterTTLIsSlidingWindow(t *testing.T) {
	useMemoryBackend(t)

	const ttl = 400 * time.Millisecond
	c := NewCounter("test_ttl", ttl)
	if _, err := c.Incr("k"); err != nil {
		t.Fatal(err)
	}

	// 在第一次的 TTL 到期前再自增一次，把过期时间往后推。
	time.Sleep(350 * time.Millisecond)
	if _, err := c.Incr("k"); err != nil {
		t.Fatal(err)
	}

	// t≈500ms：已越过"不滑动时"的 400ms 截止点，但第二次自增把窗口推到了 750ms。
	// 留了 ±100ms 余量，避免机器负载高时误报。
	time.Sleep(150 * time.Millisecond)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Errorf("滑动窗口未生效：Get = (%d, %v)，期望 (2, true)", v, ok)
	}

	time.Sleep(400 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Error("TTL 到期后应过期")
	}
}
