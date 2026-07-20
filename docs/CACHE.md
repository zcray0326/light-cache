# Day2:单机并发缓存实现详解

> 本文档讲清 light-cache Day2 的设计:`ByteView` 只读封装、`cache` 并发封装、`Group` 回源门面。
> 配套代码:`internal/cache/byteview.go` / `internal/cache/cache.go` / `internal/cache/group.go` / `byteview_test.go` / `group_test.go`。
> 参考:GeeCache Day2 博客。本实现相对原版的扩展:**底层淘汰策略可由 Group 配置**(LRU/FIFO),借助 Day1 的策略模式。

---

## 一、Day2 在做什么

Day1 做了"非并发安全的淘汰缓存"(`eviction` 包,LRU/FIFO)。Day2 在它上面再搭两层,得到一个**可用的单机缓存系统**:

```
┌──────────────────────────────────────────────┐
│  Group (group.go)  ← 外部唯一入口          │
│  命名空间 + 未命中回源                         │
│  Get: 命中直接返回,未命中调 getter 回源写回   │
└───────────────────┬──────────────────────────┘
                    │ 持有
                    ▼
┌──────────────────────────────────────────────┐
│  cache (cache.go)  ← 并发封装                 │
│  sync.Mutex 包住 eviction.CacheStrategy      │
│  让 LRU/FIFO 都线程安全                       │
└───────────────────┬──────────────────────────┘
                    │ 持有接口(不写死实现)
                    ▼
┌──────────────────────────────────────────────┐
│  eviction 包 (Day1)  ← 底层淘汰策略           │
│  lruCache / fifoCache,实现 CacheStrategy     │
└──────────────────────────────────────────────┘

另:ByteView 贯穿三层,作为缓存值的统一只读类型。
```

三层各司其职:**eviction** 管"怎么淘汰",**cache** 管"并发安全",**Group** 管"命名隔离 + 未命中回源"。外部只碰 Group。

---

## 二、ByteView:只读值封装(`byteview.go`)

### 问题:为什么需要它

缓存里存的值如果是裸 `[]byte`,外部拿到后改一下(`v[0]='x'`),**缓存内部的数据就被污染了**。所以要把值包一层,对外**只读**。

```go
type ByteView struct {
    b []byte // 小写,外部不可直接访问
}

func (v ByteView) Len() int { return len(v.b) }

// ByteSlice 返回拷贝 —— 只读的关键
func (v ByteView) ByteSlice() []byte { return cloneBytes(v.b) }

// String 直接转,无需拷贝
func (v ByteView) String() string { return string(v.b) }
```

### 关键点:`ByteSlice` 返回拷贝,`String` 不用拷贝

| 方法 | 是否拷贝 | 为什么 |
| --- | --- | --- |
| `ByteSlice()` | ✅ 拷贝 | `[]byte` 可变,直接返回 `b` 外部就能改 → 必须拷贝 |
| `String()` | ❌ 不拷贝 | `string` 在 Go 里**不可变**,`string(b)` 已产生新数据,天然安全 |

这是 Day2 一个经典面试点:**为什么 `ByteSlice` 要拷贝而 `String` 不用?** —— 因为 `[]byte` 可变、`string` 不可变。

### `Len()` 实现了 `eviction.Value` 接口

`eviction.Value` 接口只要 `Len() int`。`ByteView` 有 `Len()`,所以 `ByteView` **自动满足** `eviction.Value`,能直接塞进 LRU/FIFO。这就是接口的隐式实现——不需要 `implements` 关键字。

### `cloneBytes`

```go
func cloneBytes(b []byte) []byte {
    c := make([]byte, len(b))
    copy(c, b)
    return c
}
```

`make` 新建同长 slice,`copy` 把数据复制过去。返回的是全新一份,与原 `b` 互不影响。

测试 `TestByteView_ByteSliceImmutable` 验证了这点:拿到 `ByteSlice()` 返回值改第一个字节,内部 `v.b` 不变。

---

## 三、cache:并发封装(`cache.go`)

### 问题:Day1 的 eviction 不并发安全

`eviction` 注释写了 "not safe for concurrent access"。多协程同时 `Get`/`Add` 会数据竞争。加一层 Mutex 包它。

```go
type cache struct {
    mu       sync.Mutex
    strategy eviction.CacheStrategy // ← 持有接口,不写死 LRU
}
```

### 关键点:持有接口而非具体类型

原版 GeeCache 写的是 `lru *lru.Cache`(写死 LRU)。我们写成 `strategy eviction.CacheStrategy`——**只认接口**。好处:

- LRU 和 FIFO **同时**获得并发安全(加锁这层对接口实现,两种策略通用)。
- 策略可由 `Group` 配置,换策略不用改 `cache.go`。

这是 Day1 策略模式在 Day2 的**直接兑现**:并发封装一次,所有策略受益。

```go
func newCache(evictionType string, maxBytes int64) *cache {
    s, err := eviction.New(evictionType, maxBytes, nil) // 工厂造策略
    if err != nil {
        panic(err) // 策略名写错是开发期 bug,直接 panic
    }
    return &cache{strategy: s}
}
```

### 每个方法 `Lock`/`defer Unlock`

```go
func (c *cache) add(key string, value ByteView) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.strategy.Add(key, value)
}
```

`Get`/`Add`/`Len` 全部加锁。

### 为什么用 `sync.Mutex` 而非 `sync.RWMutex`?

这是 Day2 另一个面试点。直觉上缓存"读多写少"该用读写锁,但这里不行:

- **LRU 的 `Get` 命中会 `MoveToBack` 改链表**——这是**写**操作,读锁护不住。
- 所以 LRU 缓存用 `Mutex`(独占锁)最简单正确。

那 FIFO 的 `Get` 不改链表,能用 `RWMutex` 吗?理论上可以。但统一用 `Mutex` 最简单,且淘汰时(LRU 也会改链表)本来就要写锁。**权衡:简单正确 > 微小性能**。

### `Get` 的类型断言

```go
v, ok := c.strategy.Get(key)        // 返回 eviction.Value(即 any)
...
return v.(ByteView), true           // 断言回 ByteView
```

`eviction.Get` 返回接口 `Value`(`any`),存进去的是 `ByteView`,取出来断言回来。和 Day1 讲过的 `ele.Value.(*entry)` 同理——`any` 装箱,取出要手动拆箱断言。

---

## 四、Group:回源门面(`group.go`)

`Group` 是整个系统的入口,做三件事:**命名隔离**、**未命中回源**、**注册可查**。

### 1. `Getter` 接口 + `GetterFunc`(接口型函数)

```go
type Getter interface {
    Get(key string) ([]byte, error)
}

type GetterFunc func(key string) ([]byte, error)
func (f GetterFunc) Get(key string) ([]byte, error) { return f(key) }
```

**问题**:回源逻辑因业务而异,DB、API、文件都可能。定义成接口 `Getter`,用户各自实现。但每次都写个结构体实现接口太啰嗦——所以加 `GetterFunc`:

- `GetterFunc` 是个**函数类型**。
- 给它实现 `Get` 方法(里面就调自己 `f(key)`),于是 `GetterFunc` 满足了 `Getter` 接口。
- 用户传一个普通 `func` 就行:`GetterFunc(func(key string) ([]byte, error) {...})`。

这叫**接口型函数**(适配器模式),和标准库 `http.HandlerFunc`、`http.HandlerFunc` 同理。Day2 第三个面试点。

用法见测试:

```go
gee := NewGroup("scores", 2<<10, "lru", GetterFunc(func(key string) ([]byte, error) {
    return []byte(db[key]), nil // 模拟从 DB 回源
}))
```

### 2. `Group` 结构与全局表

```go
type Group struct {
    name      string // 命名空间,隔离不同业务
    getter    Getter // 未命中回源
    mainCache cache  // 本地并发缓存
}

var (
    mu     sync.RWMutex
    groups = make(map[string]*Group)
)
```

- `name` 隔离命名空间:不同业务的缓存互不干扰(如 "scores" 和 "users")。
- `groups` 全局表:`NewGroup` 注册,`GetGroup` 按名查。为后续 Day5 分布式节点按 group 名路由做准备。
- 全局表用 `RWMutex`:`GetGroup` 只读用 `RLock`,`NewGroup` 写用 `Lock`。

### 3. `Get`:命中或回源(核心)

```go
func (g *Group) Get(key string) (ByteView, error) {
    if key == "" {
        return ByteView{}, fmt.Errorf("key is required")
    }
    // 1. 先查本地缓存
    if v, ok := g.mainCache.get(key); ok {
        log.Println("[light-cache] hit")
        return v, nil
    }
    // 2. 未命中 → 回源
    return g.load(key)
}
```

流程图:

```
Group.Get(key)
   │
   ├─ 查 mainCache.get(key)
   │     ├─ 命中 → 返回值 (log: hit)
   │     └─ 未命中 ↓
   │
   ▼
load(key) ──► getLocally(key)
                  │
                  ├─ getter.Get(key)  ← 回源(DB/API)
                  │     ├─ error → 返回 error
                  │     └─ ok   ↓
                  ├─ ByteView{b: cloneBytes(bytes)}  ← 拷贝防污染
                  ├─ populateCache(key, value)  ← 写回缓存
                  └─ 返回 value
```

为什么 `load`/`getLocally`/`populateCache` 拆成三个方法,而不是全塞进 `Get`?**为 Day5 预留**:`load` 以后会先尝试远程节点,失败再回退 `getLocally`。现在 `load` 只调本地,但接口已就位,Day5 扩展不破坏 `Get`。这是"为扩展留口子"的体现。

### 4. 回源结果必须 `cloneBytes`

```go
bytes, err := g.getter.Get(key)
...
value := ByteView{b: cloneBytes(bytes)}
```

回源函数返回的 `[]byte` 可能被调用方继续持有/修改。不拷贝直接存,缓存就和外部数据共享同一块内存,外部一改缓存就脏。所以**回源结果也要拷贝**——和 `ByteView` 的只读思想一脉相承。

---

## 五、相对 GeeCache 原版的扩展

| 点 | GeeCache 原版 | 本实现 |
| --- | --- | --- |
| 底层淘汰 | 写死 `lru.Cache` | `eviction.CacheStrategy` 接口,LRU/FIFO 可选 |
| `NewGroup` 签名 | `NewGroup(name, cacheBytes, getter)` | `NewGroup(name, maxBytes, evictionType, getter)` |
| 并发层 | 只保护 LRU | 保护接口 → 所有策略通用 |
| 测试 | 只测 LRU | 额外 `TestGet_FIFO` 验证多策略 |

扩展价值:**策略模式让并发封装和回源逻辑对底层策略无感**。加 LFU 时,Day1 加 `lfu.go`,`NewGroup(..., "lfu", ...)` 即可用,Day2 代码一行不改。

---

## 六、为什么这么分层(设计要点)

1. **单一职责**:eviction 管淘汰、cache 管并发、Group 管回源。每层只一件事,可独立测、独立换。
2. **依赖接口不依赖实现**:`cache` 依赖 `eviction.CacheStrategy` 接口而非 `*lruCache`,所以策略可替换。这是"依赖倒置"。
3. **为扩展留口子**:`Group.load` 拆分给 Day5 分布式预留;`Getter` 接口给不同数据源预留;`groups` 全局表给节点路由预留。
4. **只读贯穿始终**:`ByteView` 只读、`ByteSlice` 拷贝、回源结果拷贝——三层都防"外部污染缓存"。

---

## 七、当前边界(后续逐步补)

| 缺的东西 | 归属 | 说明 |
| --- | --- | --- |
| 并发回源去重 | Day6 | singleflight:同 key 并发只回源一次,防缓存击穿 |
| 远程节点访问 | Day5 | `load` 会先试远程节点,失败回退本地 |
| HTTP 服务端 | Day3 | 让其他节点能通过 HTTP 访问本节点缓存 |
| 一致性哈希 | Day4 | 决定 key 该去哪个节点 |
| 过期/穿透防御 | ggcache 扩展 | TTL、空值缓存,后续独立加 |

---

## 附:文件清单与职责

| 文件 | 职责 |
| --- | --- |
| `byteview.go` | `ByteView` 只读值封装(实现 `eviction.Value`),`cloneBytes` |
| `cache.go` | `cache` 并发封装:`sync.Mutex` 包 `eviction.CacheStrategy`,Add/Get/Len |
| `group.go` | `Getter`/`GetterFunc`、`Group` 命名空间 + 未命中回源 + 全局注册表 |
| `byteview_test.go` | `ByteView` 只读性测试(Len / 拷贝不可变 / String) |
| `group_test.go` | `Getter` 适配器测试 + `Group` 回源/命中测试(LRU & FIFO 两策略) |
