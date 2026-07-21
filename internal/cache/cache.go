package cache

import (
	"sync"
	"time"

	"github.com/zcray0326/light-cache/internal/cache/eviction"
)

// cache 是 eviction.CacheStrategy 的并发安全封装(策略模式:加锁后多策略共享线程安全)。
// TTL 的并发保护也在这层:后台 cleanupLoop 持锁调 strategy.CleanUp,与 add/get 共享 mu 防 race。
type cache struct {
	mu              sync.Mutex
	strategy        eviction.CacheStrategy // 底层淘汰策略(非并发安全,由 mu 保护)
	ttl             time.Duration          // 全局 TTL,0=不启用
	cleanupInterval time.Duration          // 后台清理间隔,ttl>0 时 = ttl/2
	stopCh          chan struct{}          // 关闭后台 goroutine;nil=无 goroutine
}

// newCache 创建并发缓存。ttl>0 时起后台清理 goroutine。
func newCache(evictionType string, maxBytes int64, ttl time.Duration) *cache {
	s, err := eviction.New(evictionType, maxBytes, ttl, nil)
	if err != nil {
		panic(err) // 策略名写错,不静默降级
	}
	c := &cache{strategy: s, ttl: ttl}
	if ttl > 0 {
		c.cleanupInterval = ttl / 2
		c.stopCh = make(chan struct{})
		go c.cleanupLoop()
	}
	return c
}

// add 并发安全写入。
func (c *cache) add(key string, value ByteView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strategy.Add(key, value)
}

// get 并发安全查找。
func (c *cache) get(key string) (ByteView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.strategy.Get(key)
	if !ok {
		return ByteView{}, false
	}
	return v.(ByteView), true
}

// len 返回条目数。
func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.strategy.Len()
}

// cleanExpired 持锁删所有过期 entry。
func (c *cache) cleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strategy.CleanUp()
}

// cleanupLoop 后台定期清理,ticker 到点调 cleanExpired,stopCh 关闭退出。
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

// stop 关闭后台清理 goroutine。ttl=0 时空操作安全。
func (c *cache) stop() {
	if c.stopCh != nil {
		close(c.stopCh)
	}
}
