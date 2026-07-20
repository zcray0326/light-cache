package eviction

import "container/list"

// fifoCache 先进先出。和 LRU 的唯一区别:Get 命中时不移动节点。
type fifoCache struct {
	maxBytes  int64                         // maxBytes 为允许使用的最大内存,0 表示不限制。
	nbytes    int64                         // nbytes 为当前已使用的内存。
	ll        *list.List                    // ll 为双向链表。
	cache     map[string]*list.Element      // cache 为 key 到链表节点的映射,用于 O(1) 查找。
	OnEvicted func(key string, value Value) // OnEvicted 是可选回调,在记录被淘汰时执行。
}

// NewFIFO 构造 FIFO 缓存。
func NewFIFO(maxBytes int64, onEvicted func(string, Value)) *fifoCache {
	return &fifoCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
	}
}

// Get 命中只返回值,不动链表顺序
func (c *fifoCache) Get(key string) (Value, bool) {
	if ele, hit := c.cache[key]; hit {
		return ele.Value.(*entry).value, true
	}
	return nil, false
}

// Add 新增到队尾(PushBack),超限淘汰队首(Front)。
func (c *fifoCache) Add(key string, value Value) {
	if ele, ok := c.cache[key]; ok {
		kv := ele.Value.(*entry)
		c.nbytes += int64(value.Len()) - int64(kv.value.Len())
		kv.value = value // 已存在只更新值,位置不动(FIFO 不因更新而插队)
	} else {
		ele := c.ll.PushBack(&entry{key, value})
		c.cache[key] = ele //cache map新增一对映射
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeOldest()
	}
}

// removeOldest FIFO 淘汰队首(最早进入的)
func (c *fifoCache) removeOldest() {
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
func (c *fifoCache) Len() int { return c.ll.Len() }
