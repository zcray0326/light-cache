package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestCache_TTL_BackgroundCleanup 验证 cache 层后台 goroutine 定期清理:
// 不手动调 CleanUp,只靠后台,过期 entry 应被清掉(len 减到 0)。
func TestCache_TTL_BackgroundCleanup(t *testing.T) {
	const ttl = 50 * time.Millisecond
	c := newCache("lru", int64(0), ttl)
	defer c.stop()

	c.add("k1", ByteView{b: []byte("v1")})
	c.add("k2", ByteView{b: []byte("v2")})

	// 等过期 + 等后台跑一轮(间隔 = ttl/2 = 25ms)
	time.Sleep(ttl + 80*time.Millisecond)

	if c.len() != 0 {
		t.Fatalf("background cleanup should have removed expired entries, len=%d", c.len())
	}
}

// TestCache_TTL_Stop 验证 stop() 后后台 goroutine 不再清理。
func TestCache_TTL_Stop(t *testing.T) {
	const ttl = 50 * time.Millisecond
	c := newCache("lru", int64(0), ttl)
	c.add("k1", ByteView{b: []byte("v1")})
	c.stop() // 停 goroutine

	time.Sleep(ttl + 80*time.Millisecond) // 等够多个清理周期,但因 Stop 不会清

	// Stop 后没有后台清理,k1 应还在(没被主动删)
	if c.len() != 1 {
		t.Fatalf("after stop, expired entry should still be present (no cleanup), len=%d", c.len())
	}
	// 但惰性 get 仍能识别过期并删掉
	if _, ok := c.get("k1"); ok {
		t.Fatalf("lazy expire should still work after stop")
	}
}

// TestCache_TTL_NoGoroutineWhenDisabled 验证 ttl=0 不起 goroutine,stop() 空操作安全。
func TestCache_TTL_NoGoroutineWhenDisabled(t *testing.T) {
	c := newCache("lru", int64(0), 0) // ttl=0
	defer c.stop()                    // 空操作,不应 panic
	c.add("k1", ByteView{b: []byte("v1")})
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.get("k1"); !ok {
		t.Fatalf("ttl=0 should never expire")
	}
}

// TestCache_TTL_ConcurrentNoRace 验证后台清理与并发业务读写不 race:
// 后台 cleanupLoop 持锁调 CleanUp,业务 add/get 也持锁,共享同一把 mu。
// go test -race 下通过即说明无数据竞争。
func TestCache_TTL_ConcurrentNoRace(t *testing.T) {
	const ttl = 20 * time.Millisecond   // 短 TTL,让后台频繁清理,放大 race 概率
	c := newCache("lfu", int64(0), ttl) // LFU 堆版最容易暴露 race
	defer c.stop()

	var wg sync.WaitGroup
	// 并发写:不停 add 同一批 key(覆盖更新,触发堆 Fix)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.add("k1", ByteView{b: []byte(fmt.Sprintf("v%d", i))})
			c.add("k2", ByteView{b: []byte(fmt.Sprintf("v%d", i))})
		}
	}()
	// 并发读
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.get("k1")
			c.get("k2")
		}
	}()
	wg.Wait()
	// 跑完后若 -race 没报错,说明后台清理和业务读写无竞争
}
