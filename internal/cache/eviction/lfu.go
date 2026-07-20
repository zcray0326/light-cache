package eviction

import "container/heap"

// lfuCache 是基于 LFU(最不经常使用)的淘汰实现。
// 用 map 做 O(1) 查找,用最小堆(container/heap)按"访问次数"维护顺序,
// 淘汰时取堆顶(访问次数最少的)。访问次数相同时,按插入序(先插入的先淘汰)。
//
// 非并发安全。对外只暴露接口 CacheStrategy,调用方通过工厂 New("lfu", ...) 获取。
type lfuCache struct {
	maxBytes  int64
	nbytes    int64
	cache     map[string]*lfuEntry // key → 堆元素指针,O(1) 查找
	pq        *priorityQueue       // 最小堆,按访问次数排序
	OnEvicted func(key string, value Value)
}

// lfuEntry 是 LFU 堆里的元素:在 entry 基础上加访问次数 count 和堆索引 index。
type lfuEntry struct {
	key   string
	value Value
	count int // 访问次数,堆排序的主键
	index int // 该元素在堆中的位置(heap.Fix/Remove 需要更新)
}

// NewLFU 构造一个 LFU 缓存。maxBytes 为上限(0=不限),onEvicted 为淘汰回调(可 nil)。
func NewLFU(maxBytes int64, onEvicted func(string, Value)) *lfuCache {
	pq := make(priorityQueue, 0)
	return &lfuCache{
		maxBytes:  maxBytes,
		cache:     make(map[string]*lfuEntry),
		pq:        &pq,
		OnEvicted: onEvicted,
	}
}

// Get 查找 key。命中时访问次数 +1,并修正堆位置(次数变了要重新堆化)。
func (c *lfuCache) Get(key string) (Value, bool) {
	if e, ok := c.cache[key]; ok {
		e.count++
		heap.Fix(c.pq, e.index) // 次数变了,Fix 重新堆化(上浮/下沉)
		return e.value, true
	}
	return nil, false
}

// Add 新增或更新一条记录。已存在则更新值并 +1 次数;否则插入堆。之后超限则淘汰堆顶。
func (c *lfuCache) Add(key string, value Value) {
	if e, ok := c.cache[key]; ok {
		// 已存在:更新值、+1 次数、修正堆
		c.nbytes += int64(value.Len()) - int64(e.value.Len())
		e.value = value
		e.count++
		heap.Fix(c.pq, e.index)
	} else {
		// 新增:建元素、压入堆、写 map、加内存
		e := &lfuEntry{key: key, value: value, count: 1}
		heap.Push(c.pq, e)
		c.cache[key] = e
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeMin()
	}
}

// removeMin 淘汰访问次数最少的元素(堆顶)。次数相同时堆顶是先插入的(见 Less)。
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

// Len 返回缓存条目数。
func (c *lfuCache) Len() int {
	return c.pq.Len()
}

// priorityQueue 是 lfuEntry 的最小堆,实现 heap.Interface。
type priorityQueue []*lfuEntry

// Less 排序规则:访问次数少的在前(小顶堆);次数相同时,先插入的(index 小)在前。
// "次数相同按插入序"用 index 比较 —— 因为 heap.Push 顺序就是插入顺序,index 越小越早插入。
func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].count == pq[j].count {
		return pq[i].index < pq[j].index
	}
	return pq[i].count < pq[j].count
}

func (pq priorityQueue) Len() int { return len(pq) }

// Swap 交换两个元素时,必须同步更新它们的 index 字段,保持堆索引一致。
func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push 把元素压入堆。heap 包调用,需设置元素 index。
func (pq *priorityQueue) Push(x any) {
	e := x.(*lfuEntry)
	e.index = len(*pq)
	*pq = append(*pq, e)
}

// Pop 弹出堆末元素。heap 包调用,清理引用避免内存泄漏。
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	e := old[n-1]
	old[n-1] = nil // 释放引用,避免内存泄漏
	e.index = -1   // 标记已出堆
	*pq = old[:n-1]
	return e
}
