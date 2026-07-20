# eviction 包实现详解

> 本文档讲清 light-cache `eviction/` 包的设计:它做什么、为什么这么设计、LRU/FIFO 怎么实现、彼此差异在哪。
> 配套代码:`internal/cache/eviction/strategy.go` / `lru.go` / `fifo.go` / `strategy_test.go`。

---

## 一、这块要解决什么问题

缓存的内存是有限的。塞满之后还想再塞,就得**删掉一些旧数据腾地方**。
问题是:**删谁?** —— 这个"按什么规则挑该删的"就叫**淘汰策略(eviction)**。

不同规则有不同取舍:

| 策略 | 判据 | 口号 |
| --- | --- | --- |
| **FIFO** | 谁先进来删谁 | 先进先出,不看访问 |
| **LRU** | 谁最久没被碰删谁 | 最近用过的留着,冷数据删掉 |
| *(后续)* LFU | 谁用得最少删谁 | 按访问频率 |
| *(后续)* ARC | 自适应 | LRU+LFU 动态平衡 |

**为什么不全用 LRU?** 因为不同场景最优策略不同:FIFO 最简单、开销小;LRU 命中率通常更好但要维护访问顺序;LFU 对稳定热点友好但对访问模式变化反应慢。一个**像样的缓存库该让用户选**,而不是写死一种。

所以本包的目标是:**用一套统一接口,支持多种可切换的淘汰策略。** —— 这就是**策略模式**。

---

## 二、策略模式:三个角色

```
            ┌──────────────────────────────────────────┐
            │  strategy.go                            │
            │                                         │
            │  ① 接口 CacheStrategy  ←─ "合同"        │
            │  ② 枚举 EvictionType  ←─ "有哪些策略"   │
            │  ③ 工厂 New(name,...) ←─ "按名字造一个" │
            └───────────────┬─────────────────────────┘
                            │ 实现接口
              ┌─────────────┴──────────────┐
              ▼                            ▼
     ┌─────────────────┐          ┌─────────────────┐
     │ lru.go          │          │ fifo.go         │
     │ lruCache        │          │ fifoCache       │
     │ (实现接口)      │          │ (实现接口)      │
     └─────────────────┘          └─────────────────┘
```

三个角色分工:

- **接口 `CacheStrategy`** —— 定义所有策略都得会做的事:`Get / Add / Len`。它是"合同",外部调用方只认这个接口,不关心背后是 LRU 还是 FIFO。
- **枚举 `EvictionType`** —— 列出"有哪几种策略可选"(用 `iota` 自动编号),外加 `String()`(枚举→字符串)和 `StringToEvictionType()`(字符串→枚举)两个转换函数。
- **工厂 `New(name, maxBytes, onEvicted)`** —— 给个名字 `"lru"`/`"fifo"`,造出对应实现。**新增策略只要在这里加一个 `case`。**

**调用方视角**(只认接口,不认具体类型):

```go
// 想要哪种策略,改个字符串就行,其余代码一行不动
c, _ := eviction.New("lru", 1<<20, nil)   // 1MB,LRU
// c 是 CacheStrategy 接口,不是 *lruCache —— 这就是"对扩展开放,对修改封闭"
c.Add("k", someValue)
v, ok := c.Get("k")
```

### 为什么要这么设计(关键)

> **把"做什么"(接口)和"怎么做"(各实现)分开,用工厂把两者接起来。**

好处:

1. **加策略不改老代码**。要加 LFU,只需新增 `lfu.go` + 工厂里加一个 `case "lfu"`,`strategy.go` 的接口和已有 LRU/FIFO 一行不碰。这就是设计模式里说的"开闭原则"。
2. **调用方解耦**。用缓存的人只面对 `CacheStrategy` 接口,换策略不影响业务代码,降低维护成本。
3. **可测试**。`TestLRUvsFIFO` 用同一个 `CacheStrategy` 接口、同一组操作跑两种实现,差异一目了然——因为两者都满足同一接口,测试代码能统一对待。

如果不用策略模式,而是写一个 `Cache` 结构体里塞个 `mode string`、每个方法里 `if mode=="lru" {...} else if mode=="fifo" {...}`,**每加一个策略所有方法都要改**,而且分支越堆越多,这就是典型的反例。

---

## 三、核心数据结构:map + 双向链表

LRU 和 FIFO 用的是**同一套底层数据结构**,只是用法不同:

```go
type lruCache struct {
    maxBytes  int64                      // 内存上限,0=不限
    nbytes    int64                      // 当前已用内存
    ll        *list.List                 // 双向链表:维护"顺序"
    cache     map[string]*list.Element   // map:key→链表节点,O(1)查找
    OnEvicted func(key string, value Value) // 淘汰回调(可选)
}

type entry struct {  // 链表节点里存的内容(两种策略共用)
    key   string
    value Value       // Value 接口:任何能 Len() 的值
}
```

### 为什么是 map + 双向链表,而不是只用其中一个?

```
单用 map:   查找 O(1) ✗  但没有顺序,没法知道"谁最该删"
单用链表:   有顺序 ✗     但查找 O(n),太慢
map + 链表: 查找 O(1) ✓  且链表节点移动/插入/删除 O(1) ✓
```

两者**互补**:

- **map** 负责"按 key 快速找到节点":`cache[key]` 直接拿到链表节点指针,O(1)。
- **双向链表** 负责"维护顺序":能把任意节点 O(1) 地挪到队首/队尾、O(1) 地删队首或队尾。靠的是双向链表每个节点持有前驱(`Prev`)和后继(`Next`)指针,改两个指针就能摘除/插入节点,不用遍历。

### 链表节点里为什么还要存 key?(容易忽略的细节)

```
淘汰时:取链表尾节点 → 拿到 entry → 还要从 map 里删掉这个 key 的映射
                    └──── 但链表节点只有 value 的话,怎么知道删 map 里的哪个 key?
```

所以 `entry` 里**必须存 key**,淘汰链表节点时才能 `delete(c.cache, kv.key)` 把 map 对应映射也清掉。这是个不存就出 bug 的小设计。

### `Value` 接口:为什么值要能 `Len()`

缓存按**字节**算内存用量(`maxBytes`/`nbytes`),所以得知道每个 value 占多大。但值可能是 string、可能是结构体、可能是图片字节……统一抽象成接口:

```go
type Value interface {
    Len() int   // 报告自己占多少字节
}
```

任何实现了 `Len() int` 的类型都能存进来。`nbytes` 累加 `len(key) + value.Len()`,淘汰时扣回去,保证内存计量准确。

---

## 四、LRU 怎么工作(带图)

**约定**:`Front()`=队首=最旧(淘汰对象),`Back()`=队尾=最新。新增/命中的记录往队尾放,淘汰从队首取。和 FIFO 方向一致,便于统一理解。

### 数据结构示意

```
   map                              双向链表 ll
  ┌──────────┐         ┌──────────┐ Front (最旧,被淘汰)
  │ "k1" → ●─┼────────►│ k1 │ v1 │⟷│ k2 │ v2 │⟷│ k3 │ v3 │
  │ "k2" → ●─┼───┐    └──────────┘                  └──────────┘ Back (最新,最近访问)
  │ "k3" → ●─┼─┐ └───────────────────────────▲
  └──────────┘ └─────────────────────────────┘
   map 的 value 是指向链表节点的指针 *list.Element
   所以 map 能 O(1) 直接定位到链表里的某个节点
```

### Get(命中)—— 把节点挪到队尾

访问 `k2`,它命中后,把它从中间挪到队尾(标记"我刚用过"):

```
  之前: [k1最旧]⟷[k2]⟷[k3最新]
  Get(k2) 命中 → MoveToBack(k2)
  之后: [k1最旧]⟷[k3]⟷[k2最新]   ← k2 升到队尾,k1 仍然最旧
```

```go
func (c *lruCache) Get(key string) (Value, bool) {
    if ele, hit := c.cache[key]; hit { // O(1) 查 map 拿节点
        c.ll.MoveToBack(ele)           // O(1) 挪到队尾 ← 这就是 LRU 的"标记最近用"
        return ele.Value.(*entry).value, true
    }
    return nil, false
}
```

### Add —— 新增/更新,超限淘汰

```
  加 k4:PushBack 插到队尾
  之前: [k1旧]⟷[k2]⟷[k3新]
  之后: [k1旧]⟷[k2]⟷[k3]⟷[k4新]
  若超 maxBytes → 反复 removeOldest(取 Front=最旧的 k1) 删掉,直到回到限额内
```

```go
func (c *lruCache) Add(key string, value Value) {
    if ele, ok := c.cache[key]; ok {
        // 已存在:移到队尾 + 更新值 + 修正内存占用
        c.ll.MoveToBack(ele)
        kv := ele.Value.(*entry)
        c.nbytes += int64(value.Len()) - int64(kv.value.Len())
        kv.value = value
    } else {
        // 新增:插队尾 + 写 map + 加内存
        ele := c.ll.PushBack(&entry{key, value})
        c.cache[key] = ele
        c.nbytes += int64(len(key)) + int64(value.Len())
    }
    for c.maxBytes != 0 && c.maxBytes < c.nbytes {  // maxBytes==0 表示不限
        c.removeOldest()
    }
}
```

### removeOldest —— 淘汰 Front

```go
func (c *lruCache) removeOldest() {
    ele := c.ll.Front()          // 取最旧节点(队首)
    if ele != nil {
        c.ll.Remove(ele)         // 从链表摘除
        kv := ele.Value.(*entry)
        delete(c.cache, kv.key)   // 从 map 删映射 ← 这里需要 entry 里的 key
        c.nbytes -= int64(len(kv.key)) + int64(kv.value.Len())  // 扣内存
        if c.OnEvicted != nil {   // 回调(先判空,否则 nil 调用 panic)
            c.OnEvicted(kv.key, kv.value)
        }
    }
}
```

---

## 五、FIFO 怎么工作(和 LRU 的唯一差异)

FIFO 的约定(本实现):`PushBack` 入队(新值进队尾)、淘汰 `Front`(队首=最早进入)。**和 LRU 方向完全一致**(都是队尾=最新、队首淘汰)。
**和 LRU 唯一的区别:`Get` 命中时不挪动节点。** —— FIFO 不在乎你访没访问过,只按进入顺序排队。

```
  FIFO/LRU 链表: [k1队首(最旧,淘汰)]⟷[k2]⟷[k3队尾(最新)]
  Get(k1) 命中 → 直接返回,节点位置不动
  LRU 的 Get 则会 MoveToBack —— 这就是两种策略的分水岭
```

```go
// FIFO Get:命中只返回,不 Move —— 这是它与 LRU 的全部差异
func (c *fifoCache) Get(key string) (Value, bool) {
    if ele, hit := c.cache[key]; hit {
        return ele.Value.(*entry).value, true   // 注意:没有 MoveToBack
    }
    return nil, false
}
```

### 方向问题(说明)

本包里 LRU 和 FIFO **方向完全一致**:`Front`=队首=最旧(淘汰),`Back`=队尾=最新。

> 注:GeeCache 原版 LRU 用的是 `Front`=最新、淘汰 `Back`,名字和语义拧着(反直觉)。本实现把 LRU 方向统一成 `Front`=队首淘汰、`Back`=队尾最新,与 FIFO 对齐。两者唯一差异就剩 LRU 的 `Get` 里多一个 `MoveToBack`。

---

## 六、一张图看懂 LRU vs FIFO 的行为差异

**同一组操作,两种策略淘汰结果不同** —— 这正是 `TestLRUvsFIFO` 要验证的:

```
初始容量:只装得下 2 条(k1+v1, k2+v2)

操作:  Add(k1) → Add(k2) → Get(k1) → Add(k3)   [Add(k3) 必然触发淘汰]

        (队首=最旧/淘汰 ←────────────── 队尾=最新)
┌─────────────┬───────────────────────┬───────────────────────┐
│             │         LRU            │         FIFO           │
├─────────────┼───────────────────────┼───────────────────────┤
│ Add(k1)     │ [k1]                   │ [k1]                   │
│ Add(k2)     │ [k1旧,k2新]            │ [k1早,k2晚]            │
│ Get(k1)     │ [k2旧,k1新]  ←挪动!   │ [k1早,k2晚] ←不挪      │
│ Add(k3)超限 │ 淘汰 k2(最旧)         │ 淘汰 k1(最早进)        │
└─────────────┴───────────────────────┴───────────────────────┘
              ↑ k1 幸存(刚用过)         ↑ k1 被删(不管你用没用)
```

**结论一句话**:LRU 看"最近用过没",FIFO 只看"进来早不早"。Get(k1) 这个动作,LRU 救了 k1,FIFO 里没救成。

---

## 七、整体调用链(工厂怎么把一切串起来)

```
   用户代码
      │  eviction.New("lru", maxBytes, onEvicted)
      ▼
   ┌──────────────────────────────────────┐
   │ factory New()                        │
   │  1. StringToEvictionType("lru")      │  字符串 → 枚举值
   │     → EvictionLRU                    │
   │  2. switch t {                       │
   │       case EvictionLRU:              │
   │         return NewLRU(...)  ─────┐   │
   │       case EvictionFIFO:         │   │
   │         return NewFIFO(...)      │   │
   │     }                            │   │
   └──────────────────────────────────┼───┘
                                      ▼
   返回的是 CacheStrategy 接口  ← 调用方拿到这个,不再关心具体类型
                                      │
   调用 c.Add(...) / c.Get(...)       │  接口方法 → 具体实现的方法
                                      ▼
                          *lruCache.Add / Get / Len
                          (或 *fifoCache,取决于工厂选的)
```

### 三个角色的协作关系

- **接口** 定调子("策略必须能 Get/Add/Len"),是稳定的。
- **枚举** 是"选项清单",要扩策略就加一项。
- **工厂** 是"接线员",把"用户要的名字"翻译成"对应实现"。它是唯一同时知道所有策略的地方,所以**新策略只需动工厂这一个 switch**——扩展点集中,这正是策略模式的精髓。

---

## 八、扩展:怎么加一个 LFU(演示开闭原则)

照三步走,不碰老代码:

1. `strategy.go` 枚举加一项:`EvictionLFU`(并在 `String()`、`StringToEvictionType()` 各加一个 `case`)。
2. 新增 `lfu.go`:实现 `lfuCache`,满足 `CacheStrategy` 接口。
3. 工厂 `New()` 加一个 `case EvictionLFU: return NewLFU(...)`。

`lru.go`、`fifo.go`、所有调用方代码**一行不改**。这就是策略模式带来的可维护性——也是面试讲"为什么这么设计"的核心论据。

---

## 九、当前实现的边界(后续逐步补)

为保持 Day1 主题清晰,本包**刻意没有**以下东西(避免一次塞太多,后续各自独立扩展):

| 缺的东西 | 归属哪一天/扩展点 | 说明 |
| --- | --- | --- |
| 并发安全 | Day2 | 用 `sync.Mutex` 封装,本包注释已声明非并发安全 |
| TTL 过期清理 | ggcache 扩展 | eviction 是"满了删",TTL 是"到点删",两件事 |
| 分段锁 | ggcache 扩展 | 减锁粒度的并发优化,先有并发再说 |
| LFU / ARC | 本包后续 | 照第八节三步走即可扩展 |

分清 **eviction(淘汰,满了删)** 和 **expiration(过期,到点删)** 两个概念——ggcache 把它们混在一起,我们刻意先只做前者,概念更干净。

---

## 附:文件清单与职责

| 文件 | 职责 |
| --- | --- |
| `strategy.go` | `Value` 接口、`entry` 共享类型、`EvictionType` 枚举、`CacheStrategy` 接口、工厂 `New` |
| `lru.go` | LRU 实现(map+双向链表,Get 命中 MoveToBack,淘汰 Front) |
| `fifo.go` | FIFO 实现(与 LRU 同结构、同方向,Get 不移动,淘汰 Front) |
| `strategy_test.go` | 单测 + 工厂切换测试 + `TestLRUvsFIFO` 行为差异测试 |
