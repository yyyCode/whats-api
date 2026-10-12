package cachex

import (
	"sync"
	"testing"
	"time"

	"github.com/yyyCode/whats-api/internal/common"
)

func TestCounterUsesMemoryBackendWhenRedisDisabled(t *testing.T) {
	common.RedisEnabled = false
	common.RDB = nil

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
	common.RedisEnabled = false
	common.RDB = nil

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
	common.RedisEnabled = false

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
