package eviction

import (
	"container/heap"
	"time"
)

// lfuCache 是 LFU 实现(非并发安全,上层 cache 加锁)。
// map 做 O(1) 查找,最小堆按访问次数排序,淘汰取堆顶(次数最少)。次数相同按插入序。
type lfuCache struct {
	maxBytes  int64
	nbytes    int64
	cache     map[string]*lfuEntry // key → 堆元素
	pq        *priorityQueue       // 最小堆,按访问次数
	OnEvicted func(key string, value Value)
	ttl       time.Duration // 0=不启用 TTL
}

// lfuEntry 是 LFU 堆元素:在 entry 基础上加访问次数、堆索引、过期时刻。
type lfuEntry struct {
	key      string
	value    Value
	count    int       // 访问次数,堆排序主键
	index    int       // 在堆中的位置(heap.Fix/Remove 用)
	expireAt time.Time // 绝对过期时刻,零值=永不过期
}

// expired 判断是否过期(零值永不过期)。
func (e *lfuEntry) expired() bool {
	if e.expireAt.IsZero() {
		return false
	}
	return time.Now().After(e.expireAt)
}

// NewLFU 构造 LFU 缓存。并发保护和后台清理由上层 cache 负责。
func NewLFU(maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) *lfuCache {
	pq := make(priorityQueue, 0)
	return &lfuCache{
		maxBytes:  maxBytes,
		cache:     make(map[string]*lfuEntry),
		pq:        &pq,
		OnEvicted: onEvicted,
		ttl:       ttl,
	}
}

// Get 查找。命中先惰性判过期(过期删+未命中),未过期 count++ 并修正堆位置(不刷新 expireAt)。
func (c *lfuCache) Get(key string) (Value, bool) {
	if e, ok := c.cache[key]; ok {
		if e.expired() {
			c.removeEntry(e)
			return nil, false
		}
		e.count++
		heap.Fix(c.pq, e.index)
		return e.value, true
	}
	return nil, false
}

// Add 新增或更新。已存在更新值+次数+修正堆;否则插入堆。超限淘汰堆顶。
func (c *lfuCache) Add(key string, value Value) {
	if e, ok := c.cache[key]; ok {
		c.nbytes += int64(value.Len()) - int64(e.value.Len())
		e.value = value
		e.count++
		e.expireAt = c.deadline()
		heap.Fix(c.pq, e.index)
	} else {
		e := &lfuEntry{key: key, value: value, count: 1, expireAt: c.deadline()}
		heap.Push(c.pq, e)
		c.cache[key] = e
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeMin()
	}
}

// deadline 返回绝对过期时刻。ttl=0 返回零值。
func (c *lfuCache) deadline() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeEntry 删指定堆元素(heap.Remove + map 删 + 扣内存 + 回调),给 Get/CleanUp 复用。
func (c *lfuCache) removeEntry(e *lfuEntry) {
	heap.Remove(c.pq, e.index)
	delete(c.cache, e.key)
	c.nbytes -= int64(len(e.key)) + int64(e.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(e.key, e.value)
	}
}

// removeMin 淘汰堆顶(次数最少,相同次数先插入的)。
func (c *lfuCache) removeMin() {
	if c.pq.Len() == 0 {
		return
	}
	e := heap.Pop(c.pq).(*lfuEntry)
	delete(c.cache, e.key)
	c.nbytes -= int64(len(e.key)) + int64(e.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(e.key, e.value)
	}
}

// CleanUp 遍历堆删所有过期 entry。先收集待删指针再逐个删(堆删除会打乱 index,但每个 entry 的 index 始终由 Swap 维护为最新,逐个删安全)。
func (c *lfuCache) CleanUp() {
	var expired []*lfuEntry
	for _, e := range *c.pq {
		if e.expired() {
			expired = append(expired, e)
		}
	}
	for _, e := range expired {
		c.removeEntry(e)
	}
}

// Len 返回条目数。
func (c *lfuCache) Len() int {
	return c.pq.Len()
}

// priorityQueue 是 lfuEntry 的最小堆,实现 heap.Interface。
type priorityQueue []*lfuEntry

// Less:访问次数少的在前(小顶堆);次数相同先插入的(index 小)在前。
func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].count == pq[j].count {
		return pq[i].index < pq[j].index
	}
	return pq[i].count < pq[j].count
}

func (pq priorityQueue) Len() int { return len(pq) }

// Swap 交换时同步更新 index 字段,保持堆索引一致。
func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push 压入元素,设置 index。
func (pq *priorityQueue) Push(x any) {
	e := x.(*lfuEntry)
	e.index = len(*pq)
	*pq = append(*pq, e)
}

// Pop 弹出堆末元素,清引用防泄漏。
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*pq = old[:n-1]
	return e
}
