package eviction

import (
	"reflect"
	"testing"
)

// String 是实现了 Value 接口的测试辅助类型。
type String string

func (d String) Len() int {
	return len(d)
}

// TestLRUGet 测试 LRU 基本查找:命中返回值并前移,未命中返回 ok=false。
func TestLRUGet(t *testing.T) {
	c := NewLRU(int64(0), 0, nil)
	c.Add("key1", String("1234"))
	if v, ok := c.Get("key1"); !ok || string(v.(String)) != "1234" {
		t.Fatalf("cache hit key1=1234 failed")
	}
	if _, ok := c.Get("key2"); ok {
		t.Fatalf("cache miss key2 failed")
	}
}

// TestLRURemoveOldest 测试内存超限时淘汰:容量只能装两条,加第三条后最老的 key1 被淘汰。
func TestLRURemoveOldest(t *testing.T) {
	k1, k2, k3 := "key1", "key2", "k3"
	v1, v2, v3 := "value1", "value2", "v3"
	maxBytes := len(k1 + k2 + v1 + v2)
	c := NewLRU(int64(maxBytes), 0, nil)
	c.Add(k1, String(v1))
	c.Add(k2, String(v2))
	c.Add(k3, String(v3))

	if _, ok := c.Get("key1"); ok || c.Len() != 2 {
		t.Fatalf("RemoveOldest key1 failed")
	}
}

// TestLRUOnEvicted 测试淘汰回调:小容量下连续加入,验证 OnEvicted 按预期顺序被调用。
func TestLRUOnEvicted(t *testing.T) {
	keys := make([]string, 0)
	callback := func(key string, value Value) {
		keys = append(keys, key)
	}
	c := NewLRU(int64(10), 0, callback)
	c.Add("key1", String("123456"))
	c.Add("k2", String("k2"))
	c.Add("k3", String("k3"))
	c.Add("k4", String("k4"))

	expect := []string{"key1", "k2"}
	if !reflect.DeepEqual(expect, keys) {
		t.Fatalf("Call OnEvicted failed, expect keys equals to %s", expect)
	}
}

// TestFIFORemoveOldest 测试 FIFO:容量只能装两条,加第三条后最早进入的 key1 被淘汰。
// FIFO 不看访问,只看进入顺序。
func TestFIFORemoveOldest(t *testing.T) {
	k1, k2, k3 := "key1", "key2", "k3"
	v1, v2, v3 := "value1", "value2", "v3"
	maxBytes := len(k1 + k2 + v1 + v2)
	c := NewFIFO(int64(maxBytes), 0, nil)
	c.Add(k1, String(v1))
	c.Add(k2, String(v2))
	c.Add(k3, String(v3))

	if _, ok := c.Get("key1"); ok || c.Len() != 2 {
		t.Fatalf("FIFO RemoveOldest key1 failed")
	}
}

// TestNewSelectsStrategy 测试工厂能按名字选对策略,非法名字返回 error。
func TestNewSelectsStrategy(t *testing.T) {
	lru, err := New("lru", int64(0), 0, nil)
	if err != nil || lru == nil {
		t.Fatalf("New(lru) failed: %v", err)
	}
	fifo, err := New("fifo", int64(0), 0, nil)
	if err != nil || fifo == nil {
		t.Fatalf("New(fifo) failed: %v", err)
	}
	if _, err := New("nope", int64(0), 0, nil); err == nil {
		t.Fatalf("want error for invalid eviction name")
	}
}

// TestLRUvsFIFO 是整个 eviction 包最有讲解价值的测试:
// 同一组操作,LRU 和 FIFO 淘汰结果不同,直观体现两种策略的分野。
//
//   - 容量只够两条:k1+v1 与 k2+v2。
//   - 操作:加 k1,k2 → Get(k1) → 加 k3 触发淘汰。
//   - LRU:刚 Get 过 k1,k1 被标记为最近使用,应淘汰 k2(最久未用)。
//   - FIFO:Get 不影响顺序,最早进入的 k1 应被淘汰。
func TestLRUvsFIFO(t *testing.T) {
	k1, k2, k3 := "k1", "k2", "k3"
	v1, v2, v3 := "v1", "v2", "v3"
	maxBytes := int64(len(k1) + len(v1) + len(k2) + len(v2))

	mk := func(name string) CacheStrategy {
		c, err := New(name, maxBytes, 0, nil)
		if err != nil {
			t.Fatalf("New(%s) failed: %v", name, err)
		}
		return c
	}

	// LRU 分支
	lruC := mk("lru")
	lruC.Add(k1, String(v1))
	lruC.Add(k2, String(v2))
	lruC.Get(k1) // 标记 k1 为最近使用
	lruC.Add(k3, String(v3))
	if _, ok := lruC.Get(k1); !ok {
		t.Fatalf("LRU: k1 should survive (recently used), got evicted")
	}
	if _, ok := lruC.Get(k2); ok {
		t.Fatalf("LRU: k2 should be evicted (least recently used), but still exists")
	}

	// FIFO 分支
	fifoC := mk("fifo")
	fifoC.Add(k1, String(v1))
	fifoC.Add(k2, String(v2))
	fifoC.Get(k1) // FIFO:不影响顺序
	fifoC.Add(k3, String(v3))
	if _, ok := fifoC.Get(k1); ok {
		t.Fatalf("FIFO: k1 should be evicted (first in), but still exists")
	}
	if _, ok := fifoC.Get(k2); !ok {
		t.Fatalf("FIFO: k2 should survive (k1 was first in), got evicted")
	}
}
