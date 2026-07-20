# Day6:singleflight 防缓存击穿

> 本文档讲清 light-cache Day6 的设计:`singleflight` 包实现"同 key 并发去重",并接入 `Group.load`。
> 配套代码:`internal/cache/singleflight/singleflight.go` / `singleflight_test.go` / `internal/cache/group.go`(改)。
> 参考:GeeCache Day6 博客,标准库 `golang.org/x/sync/singleflight`。

---

## 一、缓存击穿是什么

缓存有三大类故障,先分清(面试常考):

| 故障 | 现象 | 原因 |
| --- | --- | --- |
| **击穿** | 一个热点 key 过期,瞬间大量请求同时穿透到 DB | 并发回源同一 key,没去重 |
| **穿透** | 查一个**不存在**的 key,每次都穿透到 DB | 缓存存不存在的值也不缓存空结果 |
| **雪崩** | 大量 key**同时过期**,全部同时回源 | TTL 集中过期 |

Day6 专门解决**击穿**:

```
热点 key "Tom" 缓存失效瞬间,1000 个请求同时到达:
  请求1: 本地未命中 → 回源 DB
  请求2: 本地未命中 → 回源 DB   ← 没去重,各回各的
  请求3: 本地未命中 → 回源 DB
  ...
  → 1000 个请求全打到 DB,DB 可能被打挂

singleflight 后:
  请求1: 未命中 → 回源 DB(第一个)        ┐
  请求2: 未命中 → 等请求1完成,共享结果     │ 只回源一次
  请求3: 未命中 → 等请求1完成,共享结果     │ 其余 999 个等
  ...                                       ┘
  → 只有 1 个请求到 DB,其余共享
```

这就是"重复抑制"(duplicate suppression)——标准库 `x/sync/singleflight` 的同款机制。

---

## 二、singleflight 实现(`singleflight/singleflight.go`)

### 数据结构

```go
type call struct {
    wg  sync.WaitGroup // 让后来的重复请求等待第一个完成
    val interface{}    // fn 的返回值
    err error          // fn 的返回错误
}

type Group struct {
    mu sync.Mutex       // 保护 m
    m  map[string]*call // key → 正在进行的 call
}
```

- `call` 代表"一次正在进行(或已完成)的调用",持有 `WaitGroup`(去重的关键)。
- `Group.m` 记录"哪些 key 正在进行",这是去重的依据。**懒初始化**(首次用才建 map)。

### `Do`:核心方法

```go
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
    g.mu.Lock()
    if g.m == nil {
        g.m = make(map[string]*call)
    }
    // 1. 已有同 key 正在进行 → 重复请求,等待
    if c, ok := g.m[key]; ok {
        g.mu.Unlock()
        c.wg.Wait()       // 阻塞到第一个 wg.Done()
        return c.val, c.err
    }
    // 2. 没有 → 我是第一个,登记
    c := new(call)
    c.wg.Add(1)           // 标记"有人在等"
    g.m[key] = c
    g.mu.Unlock()

    // 3. 执行 fn(锁外,执行期间不持锁,让别的 key 能并行)
    c.val, c.err = fn()
    c.wg.Done()           // 唤醒所有等待者

    // 4. 删除,让后续同 key 请求能重新触发
    g.mu.Lock()
    delete(g.m, key)
    g.mu.Unlock()

    return c.val, c.err
}
```

### 关键点逐个讲

**1. `m[key]` 命中 → `wg.Wait()`**:这是去重的核心。重复请求发现"已有 call 在跑",不自己跑,而是等第一个的 `wg.Done()`,然后拿第一个的结果。**1000 个请求,只有第 1 个跑 fn,其余 999 个等**。

**2. `wg.Add(1)` 在 `fn()` 前**:必须在登记 `m[key]=c` 之后、`Unlock` 之前(或紧邻)就 `Add(1)`。否则若 `Add` 晚了,`wg.Wait()` 可能在 `Done` 之后才调用,`WaitGroup` 计数为负 → panic。`Add` 必须早于任何 `Wait`。

**3. fn 执行在锁外**:`c.val, c.err = fn()` 这行**不持锁**。否则 fn 慢(回源 DB 慢)会阻塞所有其他 key 的请求。把 fn 放锁外,让不同 key 的 call 能并行进行。

**4. `delete(g.m, key)` 在 `fn()` 后**:fn 完成后从 map 删,这样**后续**的同 key 请求(在第一个完成之后才来的)能触发新一轮 call。不删的话,map 永远留着这个 call,后续请求永远拿旧结果——缓存永远不更新,也不回源。删了才让"完成的事"过去,新请求重新判断"要不要回源"(此时缓存可能已写入,直接命中,不再进 Do)。

### 一个重要的时序特性

singleflight 去重的是**"同一时刻正在进行的调用"**,不是"永久只执行一次"。如果第一批请求的 fn 已完成并 `delete` 了 map,之后到达的请求会触发新一轮。所以:

- fn **有耗时**(真实回源 IO 毫秒级)→ 窗口长,重复请求能挤进来等 → 去重生效(测试验证 100 并发只执行 1 次)
- fn **瞬时完成** → 窗口短,重复请求可能都落在 fn 完成之后 → 各自成新第一个 → 去重不明显

这也是为什么测试里要 `time.Sleep` 模拟回源耗时(见下文)。真实场景回源都慢,去重自然生效。

---

## 三、接入 Group(`group.go`)

### 加 `loader` 字段

```go
type Group struct {
    ...
    loader *singleflight.Group // Day6 防击穿
}
```

`NewGroup` 里初始化 `loader: &singleflight.Group{}`。

### `load` 用 `loader.Do` 包住

```go
func (g *Group) load(key string) (value ByteView, err error) {
    viewi, err := g.loader.Do(key, func() (interface{}, error) {
        if g.peers != nil {
            if peer, ok := g.peers.PickPeer(key); ok {
                if value, err = g.getFromPeer(peer, key); err == nil {
                    return value, nil
                }
                log.Println("[light-cache] Failed to get from peer", err)
            }
        }
        return g.getLocally(key)
    })
    if err == nil {
        return viewi.(ByteView), nil
    }
    return
}
```

**关键**:把 Day5 的"远程取 + 本地回源"整段逻辑塞进 `loader.Do` 的 `fn`。这样:

- 1000 个并发请求同 key → 只有 1 个真正进 fn(或远程或本地),其余 999 个等。
- fn 内的"远程 + 回退本地"逻辑不变,只是被去重包裹。

注意 `Do` 返回 `interface{}`,要断言回 `ByteView`(`viewi.(ByteView)`)。

---

## 四、测试验证(`singleflight_test.go`)

### `TestDo`:去重生效

```
100 个 goroutine 同时对同一 key 调 Do:
  fn 里 time.Sleep(50ms) 模拟回源耗时
  atomic 计数 fn 执行次数

结果:fn executed 1/100 times  ← 只执行 1 次,去重生效
```

### 为什么测试要 `time.Sleep`

第一版测试没 sleep,fn 瞬时完成 → 结果 fn 执行了 100 次,一次没去重(第一版测试失败)。加上 50ms sleep 后窗口够长,99 个重复请求挤进来 `wg.Wait`,只执行 1 次。

这验证了上面"时序特性":**singleflight 去重依赖 fn 有一定耗时**,真实回源场景满足,去重生效。

### `TestDoDifferentKey`:不同 key 各自执行

10 个不同 key 并发 → fn 执行 10 次。证明不同 key 不互相阻塞(各自独立 call)。

### `TestDoErr`:错误也能共享

fn 返回 error,5 个等待者都拿到相同 error。证明去重不丢错误。

---

## 五、几个值得讲的点

### 1. 为什么用 `WaitGroup` 而非 channel

`WaitGroup` 是"一对多等待"的轻量原语:`Add(1)` + `Done()` 配 `Wait()`。比 channel 简单(不用管理 channel 生命周期、缓冲)。singleflight 里"第一个完成后唤醒所有等待者",正是一对多,`WaitGroup` 最贴合。

### 2. `interface{}` 的代价

`Do` 的 fn 返回 `interface{}`(任意类型),调用方断言回具体类型。这是 Go 1.18 前无泛型时的妥协——`singleflight` 要支持任意返回类型,只能用 `interface{}`。代价:类型安全丢失(断言错会 panic)。Go 泛型版可写成 `Do[T]`,但目前标准库仍是 `interface{}`。我们沿用。

### 3. 为什么 map 不用 `sync.Map`

`sync.Map` 适合"读多写少、key 集稳定"。singleflight 的 `m` 是高频增删(每个 call 完成就 delete),且要配合 `WaitGroup` 的复合操作(`查 map → Wait` 或 `查 map → Add → 登记`),这些要原子,`sync.Map` 给不了"查+操作"的原子性。所以用 `Mutex` + 普通 map,手动保证复合操作原子。

### 4. singleflight 不解决穿透/雪崩

- **穿透**(查不存在的 key):singleflight 会让"查不存在的 key"也只回源一次,但回源拿到 error 后,后续请求还是会再回源(因为 fn 返回 err 不写缓存,下次又 miss → 又进 Do)。要彻底防穿透,得**缓存空结果**(ggcache 扩展)。
- **雪崩**(大量 key 同时过期):singleflight 是按 key 去重的,对"很多不同 key 同时失效"帮助有限。防雪崩要**随机 TTL**(让过期时间分散)。

singleflight 主要治**击穿**(单热点 key 的并发回源)。这点面试要说清,别夸大成"解决所有缓存问题"。

---

## 六、Day6 在整个架构的位置

```
Group.Get(key)
  ├─ 命中 → 返回
  └─ 未命中 → load
              │
              ▼ loader.Do(key, fn)  ← Day6 在这里包一层:同 key 并发只执行一次
              │
              ├─ (fn 内)PickPeer → 远程取  ← Day5
              └─ (fn 内)本地回源 getter   ← Day2
```

Day6 是个**横切层**:不改 Day2-5 的回源/远程逻辑,只在 `load` 外面包一层去重。这是"装饰"性质的增强——核心流程不变,加并发安全网。

---

## 七、当前边界

| 缺的东西 | 归属 | 说明 |
| --- | --- | --- |
| 缓存穿透防御 | ggcache 扩展 | 空值缓存:singleflight 让回源只一次,但 err 不缓存,要单独做 |
| 缓存雪崩防御 | ggcache 扩展 | 随机 TTL,让过期分散 |
| 结果缓存 | ggcache 扩展 | singleflight 结果可短时缓存,延长去重窗口 |
| protobuf | Day7 | 远程取的 HTTP body 仍裸字节 |

---

## 附:文件清单

| 文件 | 改动 | 职责 |
| --- | --- | --- |
| `singleflight/singleflight.go` | 新增 | `Group`+`call`+`Do`,同 key 并发去重 |
| `singleflight/singleflight_test.go` | 新增 | 去重生效、不同 key 独立、错误共享 |
| `group.go` | 改 | `Group` 加 `loader`,`load` 用 `loader.Do` 包住远程+本地 |
