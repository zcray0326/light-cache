package eviction

import (
	"container/list"
	"time"
)

// fifoCache 先进先出。和 LRU 的唯一区别:Get 命中时不移动节点。
// 非并发安全 —— 所有方法(含 CleanUp)都假设单线程,由上层 cache 层用 Mutex 保护。
// TTL 同 LRU:ttl>0 时 Add 算 expireAt、Get 惰性检查;CleanUp 遍历删过期。并发保护和后台 goroutine 在上层。
type fifoCache struct {
	maxBytes  int64                         // maxBytes 为允许使用的最大内存,0 表示不限制。
	nbytes    int64                         // nbytes 为当前已使用的内存。
	ll        *list.List                    // ll 为双向链表。
	cache     map[string]*list.Element      // cache 为 key 到链表节点的映射,用于 O(1) 查找。
	OnEvicted func(key string, value Value) // OnEvicted 是可选回调,在记录被淘汰时执行。
	ttl       time.Duration                 // ttl 为全局过期时长,0=不启用 TTL。
}

// NewFIFO 构造 FIFO 缓存。maxBytes 为上限(0=不限),ttl 为过期时长(0=不启用 TTL),
// onEvicted 为淘汰回调(可 nil)。并发保护和后台清理 goroutine 由上层 cache 负责。
func NewFIFO(maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) *fifoCache {
	return &fifoCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
		ttl:       ttl,
	}
}

// Get 命中时先惰性检查过期:过期删掉返回未命中;未过期返回值。不动链表顺序(FIFO 特性)。
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

// Add 新增到队尾(PushBack),超限淘汰队首(Front)。ttl>0 时算 expireAt 写入。
func (c *fifoCache) Add(key string, value Value) {
	if ele, ok := c.cache[key]; ok {
		kv := ele.Value.(*entry)
		c.nbytes += int64(value.Len()) - int64(kv.value.Len())
		kv.value = value // 已存在只更新值,位置不动(FIFO 不因更新而插队)
		kv.expireAt = c.deadline()
	} else {
		ele := c.ll.PushBack(&entry{key: key, value: value, expireAt: c.deadline()})
		c.cache[key] = ele //cache map新增一对映射
		c.nbytes += int64(len(key)) + int64(value.Len())
	}
	for c.maxBytes != 0 && c.maxBytes < c.nbytes {
		c.removeOldest()
	}
}

// deadline 返回该 entry 的绝对过期时刻。ttl=0 返回零值(永不过期)。
func (c *fifoCache) deadline() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeOldest FIFO 淘汰队首(最早进入的)
func (c *fifoCache) removeOldest() {
	ele := c.ll.Front()
	if ele != nil {
		c.removeElement(ele)
	}
}

// removeElement 删掉指定节点:链表移除、map 删映射、扣内存、回调。复用给 removeOldest/Get/CleanUp。
func (c *fifoCache) removeElement(ele *list.Element) {
	c.ll.Remove(ele)
	kv := ele.Value.(*entry)
	delete(c.cache, kv.key)
	c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(kv.key, kv.value)
	}
}

// CleanUp 遍历链表,删掉所有过期 entry。无参 —— entry 自带 expireAt 能自判。非并发安全,由上层持锁调用。
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
