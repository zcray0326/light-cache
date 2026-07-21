package eviction

import (
	"container/list"
	"time"
)

// fifoCache 是 FIFO 实现(非并发安全,上层 cache 加锁)。和 LRU 唯一区别:Get 命中不移动节点。
type fifoCache struct {
	maxBytes  int64
	nbytes    int64
	ll        *list.List
	cache     map[string]*list.Element      // key → 链表节点
	OnEvicted func(key string, value Value) // 淘汰回调(可 nil)
	ttl       time.Duration                 // 0=不启用 TTL
}

// NewFIFO 构造 FIFO 缓存。并发保护和后台清理由上层 cache 负责。
func NewFIFO(maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) *fifoCache {
	return &fifoCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
		ttl:       ttl,
	}
}

// Get 命中先惰性判过期(过期删+未命中),未过期返回值(不动链表顺序,FIFO 特性)。
func (c *fifoCache) Get(key string) (Value, bool) {
	if ele, hit := c.cache[key]; hit {
		kv := ele.Value.(*entry)
		if kv.expired() {
			c.removeElement(ele)
			return nil, false
		}
		return kv.value, true
	}
	return nil, false
}

// Add 新增到队尾。已存在只更新值不移动(FIFO 不因更新插队)。超限淘汰队首。
func (c *fifoCache) Add(key string, value Value) {
	if ele, ok := c.cache[key]; ok {
		kv := ele.Value.(*entry)
		c.nbytes += int64(value.Len()) - int64(kv.value.Len())
		kv.value = value
		kv.expireAt = c.deadline()
	} else {
		ele := c.ll.PushBack(&entry{key: key, value: value, expireAt: c.deadline()})
		c.cache[key] = ele
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeOldest()
	}
}

// deadline 返回绝对过期时刻。ttl=0 返回零值。
func (c *fifoCache) deadline() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeOldest 淘汰队首(最早进入)。
func (c *fifoCache) removeOldest() {
	ele := c.ll.Front()
	if ele != nil {
		c.removeElement(ele)
	}
}

// removeElement 删指定节点(链表移除+map 删+扣内存+回调)。
func (c *fifoCache) removeElement(ele *list.Element) {
	c.ll.Remove(ele)
	kv := ele.Value.(*entry)
	delete(c.cache, kv.key)
	c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(kv.key, kv.value)
	}
}

// CleanUp 遍历删所有过期 entry。
func (c *fifoCache) CleanUp() {
	var next *list.Element
	for ele := c.ll.Front(); ele != nil; ele = next {
		next = ele.Next()
		if ele.Value.(*entry).expired() {
			c.removeElement(ele)
		}
	}
}

func (c *fifoCache) Len() int { return c.ll.Len() }
