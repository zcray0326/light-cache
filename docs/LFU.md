# LFU 淘汰策略扩展详解

> 本文档讲清 light-cache 的 LFU(最不经常使用)淘汰策略:它怎么实现、与 LRU/FIFO 的本质差异、以及如何作为 ggcache 扩展点接入。
> 配套代码:`internal/cache/eviction/lfu.go` / `lfu_test.go` / `strategy.go`(改)。
> 参考:ggcache 的 LFU 实现。

---

## 一、LFU 是什么,和 LRU/FIFO 有何不同

| 策略 | 判据 | 口号 | 本实现数据结构 |
| --- | --- | --- | --- |
| **FIFO** | 谁先进来删谁 | 不看访问,只看进入顺序 | map + 双向链表 |
| **LRU** | 谁最久没被碰删谁 | 关心"时间近不近" | map + 双向链表 |
| **LFU** | 谁访问次数最少删谁 | 关心"频率高不高" | map + **最小堆** |

### LFU vs LRU 的哲学差异(核心)

两者都"访问时动一下",但动的依据不同:

- **LRU**:`Get` 命中 → 节点挪到"最新"位置(链表尾)。判据是**最后一次访问的时间**。
- **LFU**:`Get` 命中 → 访问次数 +1,堆里重新排序。判据是**访问总次数**。

举个反例场景就能看出差异:

```
容量 2。操作:Add(k1) → 反复访问 k1(6次)→ Add(k2)(只这一次)→ Add(k3)超限

LRU: k3 加入前最后访问的是 k2,k1 是最久未用的 → 删 k1
LFU: k1 被访问 6 次(高频),k2 只 1 次(低频)→ 删 k2
```

**同一个场景,LRU 和 LFU 删的不是同一个 key**。这就是两者的本质区别:
- LRU 认为"刚用过的值更重要"(时间局部性)
- LFU 认为"用得多的值更重要"(频率局部性)

LFU 对**稳定热点**(长期高频访问的 key)更友好——不会被偶然的冷访问挤掉。代价:对**访问模式变化**反应慢(旧热点变冷后,它的高 count 还在,迟迟不被淘汰)。

---

## 二、为什么 LFU 用最小堆

LRU/FIFO 用双向链表(维护"访问时间顺序"),LFU 要维护"访问次数顺序",链表也能做,但有个痛点:**访问次数变化时,要把节点挪到正确位置**。链表挪动是 O(n) 查找 + O(1) 移动;而**最小堆**用 `heap.Fix` 重新堆化是 O(log n),且天然能快速取"最小值"(即访问次数最少的,淘汰对象)。

```
LRU/FIFO:  map + 双向链表     → Get O(1)(挪链表节点是 O(1))
LFU:       map + 最小堆        → Get O(log n)(次数变了要堆化)
                             → 淘汰 O(log n)(pop 堆顶)
```

LFU 的 Get 比 LRU 慢(log n vs 1),这是 LFU 用堆的固有代价。ggcache 也是这套(map + heap)。换来的好处:淘汰判断精准(按频率而非时间)。

### Go 标准库 `container/heap`

Go 的 `container/heap` 是个**通用堆框架**,你实现 `heap.Interface` 的五个方法(`Len`/`Less`/`Swap`/`Push`/`Pop`),它帮你维护堆性质。`heap.Push`/`heap.Pop`/`heap.Fix` 是对外 API:

- `heap.Push(pq, x)`:压入元素并堆化
- `heap.Pop(pq)`:弹出堆顶(最小值)
- `heap.Fix(pq, i)`:第 i 个元素**值变了**后,重新堆化(上浮或下沉到正确位置)

LFU 的 `Get` 命中后 `e.count++`,然后 `heap.Fix(c.pq, e.index)`——因为 count 变了,该元素在堆里的位置可能要变,Fix 重新堆化。

---

## 三、实现讲解(`lfu.go`)

### 数据结构

```go
type lfuCache struct {
    maxBytes  int64
    nbytes    int64
    cache     map[string]*lfuEntry // key → 堆元素,O(1) 查找
    pq        *priorityQueue        // 最小堆
    OnEvicted func(key string, value Value)
}

type lfuEntry struct {
    key   string
    value Value
    count int    // 访问次数,堆排序主键
    index int    // 该元素在堆中的位置(heap.Fix 需要)
}
```

- `lfuEntry` 比 LRU 的 `entry` 多了 `count`(访问次数)和 `index`(堆位置)。`index` 是堆操作的关键——`heap.Fix(pq, e.index)` 要知道元素在哪。
- `cache` map 负责 O(1) 查找(拿到 `lfuEntry` 指针,含 `index`)。
- `pq` 是最小堆,堆顶是 count 最小的(淘汰对象)。

### Get:命中 +1 次数 + 堆化

```go
func (c *lfuCache) Get(key string) (Value, bool) {
    if e, ok := c.cache[key]; ok {
        e.count++
        heap.Fix(c.pq, e.index) // 次数变了,重新堆化(上浮/下沉)
        return e.value, true
    }
    return nil, false
}
```

count 变了,该元素在堆里可能要往上移(次数变大,离堆顶更远)或不动。`heap.Fix` 自动处理。

### Add:新增/更新 + 超限淘汰

```go
func (c *lfuCache) Add(key string, value Value) {
    if e, ok := c.cache[key]; ok {
        // 已存在:更新值、+1、堆化
        c.nbytes += int64(value.Len()) - int64(e.value.Len())
        e.value = value
        e.count++
        heap.Fix(c.pq, e.index)
    } else {
        // 新增:建元素、压堆、写 map、加内存
        e := &lfuEntry{key: key, value: value, count: 1}
        heap.Push(c.pq, e)
        c.cache[key] = e
        c.nbytes += int64(len(key)) + int64(value.Len())
    }
    for c.maxBytes != 0 && c.maxBytes < c.nbytes {
        c.removeMin()
    }
}
```

新增元素的 `count` 初始为 1(刚进来算访问过一次)。

### removeMin:淘汰堆顶

```go
func (c *lfuCache) removeMin() {
    if c.pq.Len() == 0 { return }
    e := heap.Pop(c.pq).(*lfuEntry)
    delete(c.cache, e.key)
    c.nbytes -= int64(len(e.key)) + int64(e.value.Len())
    if c.OnEvicted != nil { c.OnEvicted(e.key, e.value) }
}
```

`heap.Pop` 弹出堆顶(最小值,count 最低的)。同步从 map 删、扣内存、回调。

### priorityQueue:实现 heap.Interface

```go
type priorityQueue []*lfuEntry

func (pq priorityQueue) Less(i, j int) bool {
    if pq[i].count == pq[j].count {
        return pq[i].index < pq[j].index // 次数相同,先插入的(index 小)在前
    }
    return pq[i].count < pq[j].count // count 小的在前(小顶堆)
}

func (pq priorityQueue) Swap(i, j int) {
    pq[i], pq[j] = pq[j], pq[i]
    pq[i].index = i // 交换时同步更新 index!
    pq[j].index = j
}
// Push/Pop 略,见 lfu.go
```

**两个关键点**:

1. **`Less` 的次级排序**:次数相同时,用 `index` 比较——`heap.Push` 是按顺序追加的,index 小的是先插入的。所以"次数相同,先插入的先淘汰"——这避免了同次数元素的无序随机淘汰,行为确定。

2. **`Swap` 必须同步更新 index**:堆内部交换元素时,元素的物理位置变了,`index` 字段必须跟着改,否则下次 `heap.Fix(pq, e.index)` 会找错位置。这是用 `container/heap` 的"维护成本"。

---

## 四、接入工厂:开闭原则兑现

接入只改了 `strategy.go` 三处,**LRU/FIFO 的代码一行没碰**:

1. 枚举加一项:`EvictionLFU`
2. `String()` / `StringToEvictionType()` 各加一个 `case "lfu"`
3. 工厂 `New()` 加一个 `case EvictionLFU: return NewLFU(...)`

新增 LFU 实现(`lfu.go`)+ 工厂接线,老代码零改动。这就是 `EVICTION.md` 第八节"开闭原则"的实操兑现——**对扩展开放,对修改封闭**。

```go
// 现在工厂支持三种
func New(name string, maxBytes int64, onEvicted func(string, Value)) (CacheStrategy, error) {
    switch t {
    case EvictionLRU:  return NewLRU(maxBytes, onEvicted), nil
    case EvictionFIFO: return NewFIFO(maxBytes, onEvicted), nil
    case EvictionLFU:  return NewLFU(maxBytes, onEvicted), nil  // 新增,只这一行
    ...
    }
}
```

调用方:`eviction.New("lfu", ...)` 即可用,`NewGroup(..., "lfu", ...)` 也能配 LFU 底层。上层(group.go/cache.go)无需改动——它们依赖的是 `CacheStrategy` 接口,LFU 实现了接口就自动接入。

---

## 五、测试验证

### `TestLFUDiffersFromLRU`(最有价值)

同一组操作,LFU 和 LRU 删不同的 key:

```
操作: Add(k1) → 反复访问 k1(6次)→ Add(k2)(1次)→ Add(k3)超限

LFU: k1 count=6(高),k2 count=1(低)→ 删 k2(频率低)
LRU: k3 前最后访问 k2,k1 最久未用 → 删 k1(时间远)
```

测试断言:LFU 删 k2、LRU 删 k1。**同一个场景两种策略结果相反**,清晰演示哲学差异。

### 其他测试

- `TestLFUGet`:基本命中/未命中
- `TestLFURemoveMin`:容量超限时删 count 最低的
- `TestLFUvsLRU`:高频访问 k1 后加 k3,LFU 删低频 k2
- `TestLFUFactory`:工厂按 "lfu" 名创建,验证策略模式接入

---

## 六、LFU 的优缺点(面试可讲)

**优点**:
- 对稳定热点友好(高频 key 不会被偶然冷访问挤掉)
- 淘汰判断比 LRU 更"全局"(看总频率而非单次时间)

**缺点**:
- Get 是 O(log n)(堆化),比 LRU 的 O(1) 慢
- 对访问模式变化反应慢:旧热点变冷后,它的高 count 迟迟不降,占用缓存(历史包袱)
- 新 key 初始 count=1,容易被早期淘汰(新数据"冷启动"问题)

**改进方向**(ARC 解决):ARC(Adaptive Replacement Cache)在 LRU 和 LFU 间动态平衡,兼具两者优点——这是后续可加的扩展点。

---

## 七、当前淘汰策略全家桶

| 策略 | 状态 | 数据结构 | Get 复杂度 |
| --- | --- | --- | --- |
| LRU | ✅ | map + 双向链表 | O(1) |
| FIFO | ✅ | map + 双向链表 | O(1) |
| LFU | ✅(本次) | map + 最小堆 | O(log n) |
| ARC | ⬜ 后续 | LRU+LFU 双链表 | O(1) |

三种策略共用 `CacheStrategy` 接口 + 工厂,调用方一行换策略名即可。

---

## 附:文件清单

| 文件 | 改动 | 职责 |
| --- | --- | --- |
| `internal/cache/eviction/lfu.go` | 新增 | `lfuCache` + `lfuEntry` + `priorityQueue`(最小堆),LFU 实现 |
| `internal/cache/eviction/lfu_test.go` | 新增 | LFU 基础 + 与 LRU 差异 + 工厂测试 |
| `internal/cache/eviction/strategy.go` | 改 | 枚举/String/StringToEvictionType/工厂 各加 LFU case |
