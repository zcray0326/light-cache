# TTL 过期清理

> 扩展点:在已有 eviction(满了删)之上,加 TTL(到点删),保证数据新鲜度。设计对齐 Redis 的 lazy + active expiration。

## 为什么要 TTL

缓存有两套**正交**的删除机制,缺一不可:

| 机制 | 触发条件 | 目的 |
| --- | --- | --- |
| **eviction(LRU/FIFO/LFU)** | 缓存**满了** | 释放内存,挑该删的策略删 |
| **TTL** | 缓存**到点了** | 保证新鲜度,不管满不满,过期就清 |

只有 eviction 没有 TTL 的问题:一个 key 只要一直被访问(命中),eviction 永远不会删它,但它持有的可能是很久以前 DB 写入的旧值——数据陈旧,正确性 bug。TTL 保证"这份数据 N 秒后必失效,必须重新回源拿最新的"。

## 绝对过期(不是滑动)

本项目的 TTL 是**绝对过期**:

- entry 上挂 `expireAt time.Time`,**写入时定死**(`now + ttl`)
- **Get 命中不刷新** `expireAt` —— 到点必删,不管访问多频繁
- 过期判断:entry 自包含,`expired() bool` 只看自己的 `expireAt`,**无参,不需外部喂 ttl**

对比**滑动过期**(ggcache 的做法):entry 挂 `UpdateAt`,Get/Put 命中会 `Touch()` 刷新,热点 key 一直不过期。滑动模式的问题:

1. entry 自己判断不了过期(`Expired(ttl)` 得带参数),ttl 被迫存在 cache 层,entry 和 cache **两份状态联动**
2. 热点 key 永不过期 → 上游数据变了,缓存永远拿不到新值
3. `CleanUp(ttl)` 签名带参,接口不干净

绝对式的好处:每个 entry 自包含"我什么时候过期",`CleanUp()` 无参,无跨层状态。**与 Redis 一致**——Redis 存的是绝对 `expiretime`,GET 命中先检查过期(惰性),后台定期 active expire。

## 惰性 + 后台双管齐下

| 路径 | 何时 | 作用 |
| --- | --- | --- |
| **惰性(Get 时检查)** | Get 命中 → 若 `expired()` 则删 + 返回未命中 | 保证不返回脏数据,即使后台还没扫到 |
| **后台(goroutine 定期)** | ticker 到点调 `CleanUp()` 扫全表删过期 | 释放内存,不依赖访问触发 |

只有惰性:过期 key 不访问就永远占内存(直到被 eviction 淘汰)。
只有后台:goroutine 没跑到的过期 entry,Get 会返回脏数据(ggcache 的缺陷)。
**两者都有**,才既不占内存也不返脏数据。

## 方案 B:过期状态挂在 entry 上,无跨层状态

TTL 的**过期判断**放在 eviction 层(每个策略的 entry 上挂 `expireAt`),但**并发保护和后台 goroutine 放在 cache 层**。这是关键分工:

- **eviction 层**:纯算法,非并发安全(和 Get/Add 一样)。每个策略自己实现 `CleanUp()`(遍历删过期),entry 自带 `expireAt` 自判。
- **cache 层**:用 `sync.Mutex` 包 strategy,所有方法(add/get/`cleanExpired`)都在锁内调 strategy;后台 `cleanupLoop` 也在 cache 层起,通过 `cleanExpired()` 加锁后调 `strategy.CleanUp()`——**和业务读写共享同一把 `mu`,避免 race**。

为什么不在 cache 层维护独立的 `deadlines map`?那会导致两份"key 集合"状态(eviction 的 map + cache 的 deadlines map),必须时刻同步(淘汰时靠 OnEvicted 回调同步、TTL 主动删又要给策略加 Delete),任何一条路径漏同步就 bug。方案 B 把 `expireAt` 挂在 entry 上,**只有一份状态**:淘汰时 entry 整个没,expireAt 跟着没;过期时从同一个数据结构删。无跨层同步债。

代价:三个策略各写一遍 TTL 算法(Add 算 expireAt、Get 惰性检查、CleanUp)。但每个策略自包含、自洽,是合理代价。这也是 ggcache 选的路。**与 ggcache 不同的是:ggcache 把后台 goroutine 也放在 strategy 层,导致非并发安全的 strategy 跨 goroutine 直接操作,本方案把 goroutine 上移到 cache 层持锁,修正了这个 race。**

## 接口与实现

`CacheStrategy` 接口因 TTL 加一个方法(`CleanUp`,纯算法):

```go
type CacheStrategy interface {
	Get(key string) (value Value, ok bool)
	Add(key string, value Value)
	Len() int
	CleanUp() // 无参:entry 自带 expireAt 能自判过期。非并发安全,由上层持锁调用
}
```

注意 `Stop` **不在接口里**——goroutine 在 cache 层起,Stop 也是 cache 层的事(`cache.stop()`),不该暴露到 strategy 接口。这修正了 ggcache "Stop 不在接口/拿具体类型才能停"的问题。

每个策略的改动(eviction 层):

- **LRU/FIFO**(链表):`entry` 结构体加 `expireAt` 字段;Add 算 `expireAt`;Get 命中先 `expired()` 检查(过期删+miss,未过期才返回);`CleanUp` 遍历链表删过期。
- **LFU**(堆):`lfuEntry` 加 `expireAt`;Get 命中先检查(过期则 `heap.Remove`+map 删+miss);`CleanUp` 先收集待删 entry 指针再逐个 `heap.Remove`(注意:堆删除会打乱 index,但每个 entry 的 index 字段始终由堆的 `Swap` 维护为最新,逐个删安全)。

cache 层的改动:

```go
type cache struct {
	mu              sync.Mutex
	strategy        eviction.CacheStrategy
	ttl             time.Duration
	cleanupInterval time.Duration
	stopCh          chan struct{}
}

// cleanExpired 并发安全地删过期:持锁后调 strategy.CleanUp()(strategy 非并发安全,必须由 mu 保护)
func (c *cache) cleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strategy.CleanUp()
}

// cleanupLoop 后台定期清理。和 add/get 共享 c.mu,无 race。
func (c *cache) cleanupLoop() {
	ticker := time.NewTicker(c.cleanupInterval) // 默认 ttl/2
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
```

清理间隔 `ttl/2`:短 TTL 自动短间隔(快过期快清),长 TTL 自动长间隔(省 CPU),无需人为下限。

**关键细节:`Group.mainCache` 是 `*cache` 指针,不是 `cache` 值。** 若用值类型,`newCache` 内起的 goroutine 捕获的是局部 `*cache`,而 Group 持有值拷贝,两者 `mu` 是不同锁——后台清理和业务读写用不同锁,必然 race。用指针后,goroutine 和 Group 操作的是同一个 cache、同一把锁。

## 配置:函数式选项 WithTTL

```go
// Group 全局一个 TTL(所有 key 同一过期时长)
g := cache.NewGroup("scores", 1<<20, "lru", getter, cache.WithTTL(10*time.Minute))
```

不传 `WithTTL` 则无 TTL(向后兼容,旧代码不用改)。`NewGroup` 用可变参数 `opts ...GroupOption` 接收,Group 持有 ttl 透传给 `newCache(evictionType, maxBytes, ttl)`。

```go
func NewGroup(name string, maxBytes int64, evictionType string, getter Getter, opts ...GroupOption) *Group
```

## 绝对过期 + singleflight 的组合

绝对过期"热点 key 到点必删"的最大担忧是回源压力:到期瞬间,所有并发请求一起回源。但这正是 singleflight 存在的意义——同 key 并发回源被去重成一次。所以 **绝对过期 + singleflight** 是生产标配:既保证新鲜度,又防击穿。两者在本项目都有。

## goroutine 生命周期

- 后台 goroutine 在 cache 层起(ttl>0 时),`stopCh chan struct{}`,`stop()` close 它退出
- ttl=0 时不起 goroutine,`stop()` 是空操作(安全)
- Group 通过 `Stop()` 方法暴露:`g.Stop()` 转调 `g.mainCache.stop()`
- Group 是全局长期对象,goroutine 随进程走,生产无影响
- 测试用完必调 `defer g.Stop()` / `defer c.stop()` 防 goroutine 泄漏(见 `ttl_test.go`)

## 相关文件

- `internal/cache/eviction/strategy.go` —— `entry` 加 `expireAt`/`expired()`、接口加 `CleanUp`、工厂加 ttl 参数
- `internal/cache/eviction/lru.go` / `fifo.go` / `lfu.go` —— 三策略 TTL 纯算法(Add expireAt、Get 惰性、CleanUp)。非并发安全,无 goroutine
- `internal/cache/cache.go` —— 并发安全封装:`cleanExpired()` 加锁调 `strategy.CleanUp()`、`cleanupLoop` 后台 goroutine、`stop()`、`mainCache` 用指针避免值拷贝分离锁
- `internal/cache/group.go` —— `WithTTL` 函数式选项、`NewGroup` 可变参数、`Stop()` 暴露给上层
- `internal/cache/eviction/ttl_test.go` —— 三策略 CleanUp 纯算法测试(惰性 Get、手动 CleanUp、ttl=0)
- `internal/cache/ttl_test.go` —— cache 层后台清理 + Stop + **并发无 race** 测试(`go test -race`)
- `internal/cache/group_test.go` —— Group 级 TTL(含"过期后回源",补 ggcache 空白)
