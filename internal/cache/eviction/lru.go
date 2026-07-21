package eviction

import (
	"container/list"
	"time"
)

// lruCache 是 LRU 实现(非并发安全,由上层 cache 加锁)。方向:Front=最旧淘汰,Back=最新。
// TTL:Add 算 expireAt 写 entry,Get 惰性判过期,CleanUp 遍历删。
type lruCache struct {
	maxBytes  int64
	nbytes    int64
	ll        *list.List
	cache     map[string]*list.Element      // key → 链表节点,O(1) 查找
	OnEvicted func(key string, value Value) // 淘汰回调(可 nil)
	ttl       time.Duration                 // 0=不启用 TTL
}

// NewLRU 构造 LRU 缓存。并发保护和后台清理由上层 cache 负责。
func NewLRU(maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) *lruCache {
	return &lruCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
		ttl:       ttl,
	}
}

// Get 查找。命中先惰性判过期(过期删+未命中),未过期移到队尾返回(不刷新 expireAt)。
func (c *lruCache) Get(key string) (Value, bool) {
	if ele, hit := c.cache[key]; hit {
		kv := ele.Value.(*entry)
		if kv.expired() {
			c.removeElement(ele)
			return nil, false
		}
		c.ll.MoveToBack(ele)
		return kv.value, true
	}
	return nil, false
}

// Add 新增或更新。已存在移到队尾更新值;否则插队尾。超限淘汰队首。
func (c *lruCache) Add(key string, value Value) {
	if ele, ok := c.cache[key]; ok {
		c.ll.MoveToBack(ele)
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

// deadline 返回绝对过期时刻。ttl=0 返回零值(永不过期)。
func (c *lruCache) deadline() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeOldest 淘汰队首(最旧)。
func (c *lruCache) removeOldest() {
	ele := c.ll.Front()
	if ele != nil {
		c.removeElement(ele)
	}
}

// removeElement 删指定节点(链表移除+map 删+扣内存+回调),给 removeOldest/Get/CleanUp 复用。
func (c *lruCache) removeElement(ele *list.Element) {
	c.ll.Remove(ele)
	kv := ele.Value.(*entry)
	delete(c.cache, kv.key)
	c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(kv.key, kv.value)
	}
}

// CleanUp 遍历删所有过期 entry。先存 next 再删(删后 ele.Next() 失效)。
func (c *lruCache) CleanUp() {
	var next *list.Element
	for ele := c.ll.Front(); ele != nil; ele = next {
		next = ele.Next()
		if ele.Value.(*entry).expired() {
			c.removeElement(ele)
		}
	}
}

// Len 返回记录数。
func (c *lruCache) Len() int {
	return c.ll.Len()
}
