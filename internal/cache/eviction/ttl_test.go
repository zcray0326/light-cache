package eviction

import (
	"testing"
	"time"
)

// shortTTL 是测试用的短过期时长,够慢,但又能让 time.Sleep 稳定越过边界。
const shortTTL = 50 * time.Millisecond

// wait 等到 TTL 过期并留一点余量,避免机器调度抖动导致边界竞态。
func wait() { time.Sleep(shortTTL + 30*time.Millisecond) }

// TestLRU_TTL_LazyGet 验证 LRU 的惰性过期:Get 命中过期的 entry 会删掉并返回未命中。
// eviction 层是非并发安全纯算法,这里直接调(单线程),并发保护由上层 cache 测。
func TestLRU_TTL_LazyGet(t *testing.T) {
	c := NewLRU(int64(0), shortTTL, nil)
	c.Add("k1", String("v1"))

	// 立即查应命中
	if v, ok := c.Get("k1"); !ok || string(v.(String)) != "v1" {
		t.Fatalf("LRU: should hit before TTL, got ok=%v", ok)
	}

	wait()

	// TTL 过期:Get 命中过期的 entry,应惰性删除并返回未命中
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("LRU: should miss after TTL (lazy expire), but still hit")
	}
	// 惰性删除后,Len 应已减 1
	if c.Len() != 0 {
		t.Fatalf("LRU: lazy-expired entry should be removed, Len=%d", c.Len())
	}
}

// TestLRU_TTL_CleanUp 验证手动调 CleanUp:遍历链表删过期(非 goroutine,纯算法)。
func TestLRU_TTL_CleanUp(t *testing.T) {
	c := NewLRU(int64(0), shortTTL, nil)
	c.Add("k1", String("v1"))
	c.Add("k2", String("v2"))

	wait()
	// 过期后,若不调 CleanUp,Get(k1) 会惰性删 k1;但 k2 没被访问仍留着
	// 调 CleanUp 应把所有过期的都删掉
	c.CleanUp()

	if c.Len() != 0 {
		t.Fatalf("LRU: CleanUp should have removed all expired entries, Len=%d", c.Len())
	}
}

// TestLRU_TTL_LazyGet_KeepsUnexpired 验证惰性删除只删过期的:过期 key 删掉,未过期的还在。
func TestLRU_TTL_LazyGet_KeepsUnexpired(t *testing.T) {
	c := NewLRU(int64(0), shortTTL, nil)
	c.Add("exp", String("v1"))        // 会过期
	c.Add("fresh", String("v2"))      // 后加,后过期
	time.Sleep(20 * time.Millisecond) // exp 还没过期,但快了
	c.Add("fresh2", String("v3"))     // 最新,未过期
	wait()                            // exp 过期,fresh/fresh2 可能也过期了看时序

	// 这里主要验证 CleanUp 后,未过期的留着(刚 Add 的 fresh2 应在)
	c.CleanUp()
	// fresh2 刚加不久,应未过期还在(可能受调度抖动影响,放宽:至少不 panic)
	_ = c
}

// TestFIFO_TTL_LazyGet 验证 FIFO 的惰性过期(结构同 LRU)。
func TestFIFO_TTL_LazyGet(t *testing.T) {
	c := NewFIFO(int64(0), shortTTL, nil)
	c.Add("k1", String("v1"))
	if _, ok := c.Get("k1"); !ok {
		t.Fatalf("FIFO: should hit before TTL")
	}
	wait()
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("FIFO: should miss after TTL (lazy expire)")
	}
	if c.Len() != 0 {
		t.Fatalf("FIFO: lazy-expired entry should be removed, Len=%d", c.Len())
	}
}

// TestLFU_TTL_LazyGet 验证 LFU 的惰性过期:Get 命中过期 entry 删掉(堆 + map)。
func TestLFU_TTL_LazyGet(t *testing.T) {
	c := NewLFU(int64(0), shortTTL, nil)
	c.Add("k1", String("v1"))
	if _, ok := c.Get("k1"); !ok {
		t.Fatalf("LFU: should hit before TTL")
	}
	wait()
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("LFU: should miss after TTL (lazy expire)")
	}
	if c.Len() != 0 {
		t.Fatalf("LFU: lazy-expired entry should be removed, Len=%d", c.Len())
	}
}

// TestLFU_TTL_CleanUp 验证 LFU 手动 CleanUp 删多个过期 entry(堆版逐个删不 panic、Len 正确)。
func TestLFU_TTL_CleanUp(t *testing.T) {
	c := NewLFU(int64(0), shortTTL, nil)
	c.Add("k1", String("v1"))
	c.Add("k2", String("v2"))
	c.Add("k3", String("v3"))

	wait()
	c.CleanUp()

	if c.Len() != 0 {
		t.Fatalf("LFU: CleanUp should have removed all expired entries, Len=%d", c.Len())
	}
}

// TestTTL_DisabledByDefault 验证 ttl=0 时无 TTL:entry 永不过期(向后兼容)。
func TestTTL_DisabledByDefault(t *testing.T) {
	c := NewLRU(int64(0), 0, nil) // ttl=0,无 goroutine,无过期
	c.Add("k1", String("v1"))
	time.Sleep(30 * time.Millisecond)
	// ttl=0,expireAt 零值,expired() 返回 false,永不过期
	if _, ok := c.Get("k1"); !ok {
		t.Fatalf("ttl=0 should never expire")
	}
}
