package eviction

import "container/list"

// lruCache 是基于 LRU(最近最少使用)的淘汰实现。
// 非并发安全。对外只暴露接口 CacheStrategy,调用方通过工厂 New("lru", ...) 获取。
//
// 方向约定:Front()=队首=最旧(淘汰对象),Back()=队尾=最新。
type lruCache struct {
	maxBytes  int64                         // maxBytes 为允许使用的最大内存,0 表示不限制。
	nbytes    int64                         // nbytes 为当前已使用的内存。
	ll        *list.List                    // ll 为双向链表。
	cache     map[string]*list.Element      // cache 为 key 到链表节点的映射,用于 O(1) 查找。
	OnEvicted func(key string, value Value) // OnEvicted 是可选回调,在记录被淘汰时执行。
}

// NewLRU 构造一个 LRU 缓存。maxBytes 为上限(0=不限),onEvicted 为淘汰回调(可 nil)。
func NewLRU(maxBytes int64, onEvicted func(string, Value)) *lruCache {
	return &lruCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
	}
}

// Get 查找 key。命中时把节点移到队尾 Back(标记为最近访问),并返回值。
func (c *lruCache) Get(key string) (Value, bool) {
	if ele, hit := c.cache[key]; hit {
		c.ll.MoveToBack(ele)
		return ele.Value.(*entry).value, true
	}
	return nil, false
}

// Add 新增或更新一条记录。已存在则移到队尾并更新值;否则插入队尾。之后超限则淘汰队首。
func (c *lruCache) Add(key string, value Value) {
	if ele, ok := c.cache[key]; ok {
		c.ll.MoveToBack(ele)
		kv := ele.Value.(*entry)
		c.nbytes += int64(value.Len()) - int64(kv.value.Len())
		kv.value = value
	} else {
		ele := c.ll.PushBack(&entry{key, value})
		c.cache[key] = ele
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeOldest()
	}
}

// removeOldest 淘汰最久未访问的节点(队首,即 Front())。
func (c *lruCache) removeOldest() {
	ele := c.ll.Front()
	if ele != nil {
		c.ll.Remove(ele)
		kv := ele.Value.(*entry)
		delete(c.cache, kv.key)
		c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())
		if c.OnEvicted != nil {
			c.OnEvicted(kv.key, kv.value)
		}
	}
}

// Len 返回缓存中的记录数量。
func (c *lruCache) Len() int {
	return c.ll.Len()
}
