package cache

import (
	"sync"

	"github.com/zcray0326/light-cache/internal/cache/eviction"
)

// cache 是 eviction.CacheStrategy 的并发安全封装。
// 注意:这里只持有接口,不关心底层是 LRU 还是 FIFO —— 策略模式的好处在这里兑现:
// 加了并发锁后,LRU 和 FIFO 同时获得线程安全,且策略可由 Group 配置。
type cache struct {
	mu       sync.Mutex
	strategy eviction.CacheStrategy // 底层淘汰策略(由 Group 在构造时注入)
}

// newCache 按淘汰策略类型和内存上限创建并发缓存。
// evictionType 取值如 "lru"/"fifo"/"lfu";maxBytes 为 0 表示不限。
func newCache(evictionType string, maxBytes int64) *cache {
	s, err := eviction.New(evictionType, maxBytes, nil)
	if err != nil {
		// 配置非法直接 panic:策略名是开发者写错的,不应在运行时静默降级。
		panic(err)
	}
	return &cache{strategy: s}
}

// add 并发安全地写入一条缓存。
func (c *cache) add(key string, value ByteView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// ByteView 实现了 eviction.Value 接口,可直接传给 strategy.Add。
	c.strategy.Add(key, value)
}

// get 并发安全地查找一条缓存。
func (c *cache) get(key string) (ByteView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.strategy.Get(key)
	if !ok {
		return ByteView{}, false
	}
	// 存进去的是 ByteView,这里断言回来(eviction.Get 返回的是 Value 接口/any)。
	return v.(ByteView), true
}

// len 返回缓存条目数。
func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.strategy.Len()
}
