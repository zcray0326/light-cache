package eviction

import (
	"container/list"
	"time"
)

// lruCache 是基于 LRU(最近最少使用)的淘汰实现。
// 非并发安全 —— 所有方法(含 CleanUp)都假设单线程,由上层 cache 层用 Mutex 保护。
// 对外只暴露接口 CacheStrategy,调用方通过工厂 New("lru", ...) 获取。
//
// 方向约定:Front()=队首=最旧(淘汰对象),Back()=队尾=最新。
//
// TTL:ttl>0 时 Add 算 expireAt=now+ttl 写进 entry;Get 命中先惰性判过期;
// CleanUp 遍历删过期。但并发保护(锁)和后台 goroutine 都在上层 cache,
// 这里只是非并发安全的纯算法 —— 和 Get/Add 一样。
type lruCache struct {
	maxBytes  int64                         // maxBytes 为允许使用的最大内存,0 表示不限制。
	nbytes    int64                         // nbytes 为当前已使用的内存。
	ll        *list.List                    // ll 为双向链表。
	cache     map[string]*list.Element      // cache 为 key 到链表节点的映射,用于 O(1) 查找。
	OnEvicted func(key string, value Value) // OnEvicted 是可选回调,在记录被淘汰时执行。
	ttl       time.Duration                 // ttl 为全局过期时长,0=不启用 TTL(Add 时用)。
}

// NewLRU 构造一个 LRU 缓存。maxBytes 为上限(0=不限),ttl 为过期时长(0=不启用 TTL),
// onEvicted 为淘汰回调(可 nil)。注意:并发保护和后台清理 goroutine 由上层 cache 负责。
func NewLRU(maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) *lruCache {
	return &lruCache{
		maxBytes:  maxBytes,
		ll:        list.New(),
		cache:     make(map[string]*list.Element),
		OnEvicted: onEvicted,
		ttl:       ttl,
	}
}

// Get 查找 key。命中时先做惰性过期检查:过期则删掉并返回未命中(对齐 Redis lazy expiration),
// 未过期才把节点移到队尾 Back(标记为最近访问)并返回值。绝对语义:Get 不刷新 expireAt。
func (c *lruCache) Get(key string) (Value, bool) {
	if ele, hit := c.cache[key]; hit {
		kv := ele.Value.(*entry)
		if kv.expired() {
			// 过期:删掉,当作未命中
			c.removeElement(ele)
			return nil, false
		}
		c.ll.MoveToBack(ele)
		return kv.value, true
	}
	return nil, false
}

// Add 新增或更新一条记录。已存在则移到队尾并更新值;否则插入队尾。之后超限则淘汰队首。
// ttl>0 时算好 expireAt 写入(绝对过期,定死);ttl=0 时 expireAt 零值(永不过期)。
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

// deadline 返回该 entry 的绝对过期时刻。ttl=0 返回零值(永不过期)。
func (c *lruCache) deadline() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeOldest 淘汰最久未访问的节点(队首,即 Front())。
func (c *lruCache) removeOldest() {
	ele := c.ll.Front()
	if ele != nil {
		c.removeElement(ele)
	}
}

// removeElement 删掉指定节点:从链表移除、map 删映射、扣内存、回调。
// 抽出来给 removeOldest / Get 惰性删除 / CleanUp 复用。
func (c *lruCache) removeElement(ele *list.Element) {
	c.ll.Remove(ele)
	kv := ele.Value.(*entry)
	delete(c.cache, kv.key)
	c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())
	if c.OnEvicted != nil {
		c.OnEvicted(kv.key, kv.value)
	}
}

// CleanUp 遍历链表,删掉所有过期 entry。无参 —— entry 自带 expireAt 能自判。
// 非并发安全:由上层 cache 在持锁时调用。先存 next 再删当前节点(删后 ele.Next() 失效)。
func (c *lruCache) CleanUp() {
	var next *list.Element
	for ele := c.ll.Front(); ele != nil; ele = next {
		next = ele.Next()
		if ele.Value.(*entry).expired() {
			c.removeElement(ele)
		}
	}
}

// Len 返回缓存中的记录数量。
func (c *lruCache) Len() int {
	return c.ll.Len()
}
