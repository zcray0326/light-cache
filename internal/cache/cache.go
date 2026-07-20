package cache

import (
	"sync"
	"time"

	"github.com/zcray0326/light-cache/internal/cache/eviction"
)

// cache 是 eviction.CacheStrategy 的并发安全封装。
// 注意:这里只持有接口,不关心底层是 LRU 还是 FIFO —— 策略模式的好处在这里兑现:
// 加了并发锁后,LRU 和 FIFO 同时获得线程安全,且策略可由 Group 配置。
//
// TTL 的并发保护也归这里:后台 cleanupLoop 在 cache 层起,通过 cleanExpired() 加锁后
// 调 strategy.CleanUp() —— 和 add/get 共享同一把 mu,避免和业务读写 race。
type cache struct {
	mu              sync.Mutex
	strategy        eviction.CacheStrategy // 底层淘汰策略(非并发安全,由 mu 保护)
	ttl             time.Duration          // 全局过期时长,0=不启用 TTL
	cleanupInterval time.Duration          // 后台清理的扫描间隔,ttl>0 时 = ttl/2
	stopCh          chan struct{}          // 关闭时停止后台清理 goroutine;nil 表示无 goroutine
}

// newCache 按淘汰策略类型、内存上限和 TTL 创建并发缓存。
// evictionType 取值如 "lru"/"fifo"/"lfu";maxBytes 为 0 表示不限;ttl 为 0 表示不启用 TTL。
// ttl 透传给 strategy(Add 时算 expireAt);ttl>0 时在本层起后台清理 goroutine。
func newCache(evictionType string, maxBytes int64, ttl time.Duration) *cache {
	s, err := eviction.New(evictionType, maxBytes, ttl, nil)
	if err != nil {
		// 配置非法直接 panic:策略名是开发者写错的,不应在运行时静默降级。
		panic(err)
	}
	c := &cache{strategy: s, ttl: ttl}
	if ttl > 0 {
		c.cleanupInterval = ttl / 2 // 短 TTL 自动短间隔,长 TTL 自动长间隔
		c.stopCh = make(chan struct{})
		go c.cleanupLoop()
	}
	return c
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

// cleanExpired 并发安全地删掉所有过期 entry。
// 在持锁状态下调 strategy.CleanUp()(strategy 非并发安全,必须由本层 mu 保护)。
func (c *cache) cleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strategy.CleanUp()
}

// cleanupLoop 后台定期清理过期 entry。ticker 到点调 cleanExpired,stopCh 关闭即退出。
func (c *cache) cleanupLoop() {
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.cleanExpired()
		case <-c.stopCh:
			return
		}
	}
}

// stop 关闭后台清理 goroutine。无 goroutine 时(ttl=0)空操作安全。
// Group 不再使用时调用防 goroutine 泄漏。
func (c *cache) stop() {
	if c.stopCh != nil {
		close(c.stopCh)
	}
}
