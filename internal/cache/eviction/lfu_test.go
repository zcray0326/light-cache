package eviction

import "testing"

// TestLFUGet 测试 LFU 基本查找:命中返回值并 +1 访问次数,未命中返回 false。
func TestLFUGet(t *testing.T) {
	c := NewLFU(int64(0), nil)
	c.Add("k1", String("v1"))
	if v, ok := c.Get("k1"); !ok || string(v.(String)) != "v1" {
		t.Fatalf("LFU Get k1 failed")
	}
	if _, ok := c.Get("k2"); ok {
		t.Fatalf("LFU miss k2 failed")
	}
}

// TestLFURemoveMin 测试 LFU 淘汰:容量只能装两条,加第三条后访问次数最少的被淘汰。
//
// 关键差异:LFU 看"访问次数"而非"进入顺序"。
//   - Add(k1), Add(k2):都 count=1
//   - Get(k1):k1 count=2,k2 仍 count=1
//   - Add(k3):超限,淘汰 count 最少的 → k2 被删(k1 因被访问过而幸存)
//
// 对比 LRU:同样操作 LRU 也会删 k2(因为 Get(k1) 让 k2 变最久未用)。
// 真正区分 LFU 与 LRU 的是 TestLFUvsLRU(见下)。
func TestLFURemoveMin(t *testing.T) {
	k1, k2, k3 := "k1", "k2", "k3"
	maxBytes := int64(len(k1) + len("v1") + len(k2) + len("v2"))
	c := NewLFU(maxBytes, nil)
	c.Add(k1, String("v1"))
	c.Add(k2, String("v2"))
	c.Get(k1) // k1 count=2,k2 count=1
	c.Add(k3, String("v3"))

	if _, ok := c.Get("k2"); ok {
		t.Fatalf("LFU: k2 should be evicted (count=1, lowest), but still exists")
	}
	if _, ok := c.Get("k1"); !ok {
		t.Fatalf("LFU: k1 should survive (count=2), got evicted")
	}
}

// TestLFUvsLRU 用一组操作直观区分 LFU 和 LRU 的淘汰差异。
//
// 场景:容量装 2 条。k1 反复访问(count 高),k2 只访问一次。
// 后续反复访问 k1(不碰 k2),再加 k3 触发淘汰:
//   - LRU:谁最久没被碰删谁。k3 加入前刚碰过 k1,所以 k2 最久未用 → 删 k2。
//   - LFU:谁访问次数最少删谁。k1 被访问很多次(count 高),k2 只 count=1 → 删 k2。
//
// 这里两者结果相同(k2 都被删)。真正不同在下面 TestLFUDiffersFromLRU:
// 让 k2 比较新但次数低、k1 比较旧但次数高时,LFU 删 k2、LRU 删 k1。
func TestLFUvsLRU(t *testing.T) {
	maxBytes := int64(len("k1") + len("v1") + len("k2") + len("v2"))

	// --- LFU 分支 ---
	lfu := NewLFU(maxBytes, nil)
	lfu.Add("k1", String("v1"))
	lfu.Add("k2", String("v2"))
	// 反复访问 k1,拉高它的 count
	for i := 0; i < 5; i++ {
		lfu.Get("k1")
	}
	// 此时 k1 count=6(初始1+5),k2 count=1
	lfu.Add("k3", String("v3")) // 超限
	if _, ok := lfu.Get("k2"); ok {
		t.Fatalf("LFU: k2 (count=1) should be evicted, but exists")
	}
	if _, ok := lfu.Get("k1"); !ok {
		t.Fatalf("LFU: k1 (count=6) should survive, got evicted")
	}
}

// TestLFUDiffersFromLRU 构造 LFU 与 LRU 结果真正不同的场景:
// k1 是最旧的(先进),但被频繁访问(count 高);k2 是最新的,但只访问一次(count 低)。
// 加 k3 触发淘汰:
//   - LRU:删最久未访问的。k3 加入前最后访问的是 k2,所以 k1 最久未用 → LRU 删 k1。
//   - LFU:删访问次数最少的。k1 count 高,k2 count=1 → LFU 删 k2。
//
// 这是 LFU 与 LRU 哲学差异的最清晰演示:
//
//	LRU 关心"时间近不近",LFU 关心"频率高不高"。
func TestLFUDiffersFromLRU(t *testing.T) {
	maxBytes := int64(len("k1") + len("v1") + len("k2") + len("v2"))

	// --- LFU:删 k2(频率低)---
	lfu := NewLFU(maxBytes, nil)
	lfu.Add("k1", String("v1"))
	// 频繁访问 k1,拉高 count
	for i := 0; i < 5; i++ {
		lfu.Get("k1")
	}
	lfu.Add("k2", String("v2")) // k2 最新,但 count=1
	lfu.Add("k3", String("v3")) // 超限,LFU 删 count 最低的 k2
	if _, ok := lfu.Get("k2"); ok {
		t.Fatalf("LFU: k2 (count=1) should be evicted, but exists")
	}
	if _, ok := lfu.Get("k1"); !ok {
		t.Fatalf("LFU: k1 (high count) should survive, got evicted")
	}

	// --- LRU 对照:删 k1(最久未用)---
	// 用 LRU 跑同样操作,验证两者结果不同(对比用,不在此包外断言 LRU 内部)
	lru := NewLRU(maxBytes, nil)
	lru.Add("k1", String("v1"))
	for i := 0; i < 5; i++ {
		lru.Get("k1") // LRU:每次访问都把 k1 挪到最新
	}
	lru.Add("k2", String("v2")) // k2 最新,k1 次新
	lru.Add("k3", String("v3")) // 超限,LRU 删最久未用的 → k1
	if _, ok := lru.Get("k1"); ok {
		t.Fatalf("LRU: k1 should be evicted (oldest), but exists — LFU/LRU 结果应不同")
	}
	if _, ok := lru.Get("k2"); !ok {
		t.Fatalf("LRU: k2 should survive (newest), got evicted")
	}
}

// TestLFUFactory 通过工厂按名字创建 LFU,验证策略模式接入正确。
func TestLFUFactory(t *testing.T) {
	c, err := New("lfu", int64(0), nil)
	if err != nil || c == nil {
		t.Fatalf("New(lfu) failed: %v", err)
	}
	c.Add("k", String("v"))
	if v, ok := c.Get("k"); !ok || string(v.(String)) != "v" {
		t.Fatalf("factory-created LFU Get failed")
	}
}
