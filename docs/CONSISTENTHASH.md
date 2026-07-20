# Day4:一致性哈希实现详解

> 本文档讲清 light-cache Day4 的设计:一致性哈希算法,解决分布式缓存"节点增减时缓存大面积失效"问题。
> 配套代码:`internal/cache/consistenthash/consistenthash.go` / `consistenthash_test.go`。
> 参考:GeeCache Day4 博客。

---

## 一、要解决什么问题

分布式缓存有多个节点。一个 key 该存哪个节点?最朴素的做法是**普通哈希**:

```
节点编号 = hash(key) % 节点数
```

问题:**节点数变化时,几乎所有 key 的映射都变了**。

```
3 个节点时: hash("k1") % 3 = 1  → 节点1
加到 4 个节点: hash("k1") % 4 = 1?0?2?  → 几乎一定变

→ 几乎所有 key 都要重新迁移 → 缓存大面积失效,瞬间全部回源,DB 被打爆
```

| 节点数 | hash=10 的 key |
|---|---|
| 3 节点 | 10 % 3 = 1 → 节点1 |
| 4 节点 | 10 % 4 = 2 → 节点2 ← 变了 |
| 5 节点 | 10 % 5 = 0 → 节点0 ← 又变了 |

每加/减一个节点,**几乎 100% 的 key 要重新分配**。这就是普通哈希的致命伤。

**一致性哈希的目标**:节点增减时,**只影响相邻小区间的 key**,其余不动。

---

## 二、核心思路:哈希环

把"节点"和"key"都映射到同一个 `0 ~ 2^32-1` 的**环**上(对它们的哈希值取 2^32,自然成环)。key 在环上**顺时针**找最近的节点。

```
            环(0 ~ 2^32-1)

        节点A ●
      ╱          ╲
    ╱              ● 节点B
   │   key1→顺时针 │
   │     找到 B    │
    ╲              │
      ╲          ● 节点C
        ● key2→顺时针找到 A(绕回环首)
```

### 为什么这能减少迁移?

新增节点 D,只**插进环上一个位置**,它只"接管"顺时针方向、从上一个节点到 D 之间的那段 key:

```
加节点 D 前:   ... ─ A ────(key在这段)──→ B ─ ...
加节点 D 后:   ... ─ A ─ D ─(key归D)──→ B ─ ...

→ 只有 A 到 D 之间的 key 从"归 B"变成"归 D"
   其余所有 key 的映射不变!
```

这就是"一致性"——节点变化只影响一个小区间,而非全局。相比普通哈希的"几乎全变",迁移量从 O(全部) 降到 O(1/节点数)。

---

## 三、虚拟节点:解决数据倾斜

光有环还不够。节点少时(比如就 2-3 个),它们在环上的位置很可能**扎堆**或不均匀,导致某些节点扛了绝大部分 key(数据倾斜)。

```
不好:  ●A  ●B              ●C        ← A、B 挤一起,大部分 key 顺时针都归 C
好的:  每个真实节点 → 扩展成 N 个"虚拟节点"散布在环上 → 自然均匀
```

**虚拟节点**:一个真实节点对应环上的 N 个虚拟节点(用编号区分)。节点多了,环上分布自然趋均匀。key 顺时针找到虚拟节点后,再映射回它对应的真实节点。

```
真实节点 "2" → 虚拟节点 "02","12","22" → 各自 hash 后散落环上
                              ↓ key 命中某个虚拟节点
                          映射回真实节点 "2"
```

---

## 四、实现逐块讲解

### 数据结构

```go
type Map struct {
    mu       sync.RWMutex
    hash     Hash           // 哈希函数,可替换,默认 crc32
    replicas int            // 每个真实节点的虚拟节点数
    keys     []int          // 已排序的哈希环(虚拟节点哈希值)
    hashMap  map[int]string // 虚拟节点哈希值 → 真实节点名
}
```

- `keys`:环。把所有虚拟节点的哈希值排好序存在切片里,`Get` 时二分查找。
- `hashMap`:虚拟节点哈希值 → 真实节点。因为环上存的是虚拟节点(很多),但最终要返回真实节点(少),所以要用这个映射"翻译"回去。
- `replicas`:虚拟节点倍数。越大越均匀,内存越大。

### `New(replicas, fn)`

```go
func New(replicas int, fn Hash) *Map {
    m := &Map{
        replicas: replicas,
        hash:     crc32.ChecksumIEEE,  // 默认哈希
        hashMap:  make(map[int]string),
    }
    if fn != nil {
        m.hash = fn  // 允许自定义,便于测试用确定性哈希
    }
    return m
}
```

允许传自定义哈希函数 `fn`,是**为测试**:测试用 `testHash`(数字字符串→uint32)能让结果可预测、好写断言。生产用默认 crc32。

### `Add(nodes...)`:把节点扩展成虚拟节点撒到环上

```go
func (m *Map) Add(nodes ...string) {
    for _, node := range nodes {
        for i := 0; i < m.replicas; i++ {
            // 虚拟节点名 = strconv.Itoa(i) + node,如节点"2"的第3个虚拟节点 = "22"
            hash := int(m.hash([]byte(strconv.Itoa(i) + node)))
            m.keys = append(m.keys, hash)
            m.hashMap[hash] = node  // 虚拟节点 → 真实节点
        }
    }
    sort.Ints(m.keys)  // 排序,Get 才能 sort.Search 二分
}
```

- 一个真实节点 `node` 生成 `replicas` 个虚拟节点,名字是 `i+node`(用编号区分,避免不同真实节点的虚拟节点撞哈希)。
- 每个虚拟节点哈希值进 `keys`,同时记 `hashMap[hash]=node`。
- 最后 `sort.Ints` 排序——**环必须有序**,才能在 `Get` 里二分。

> 一个易混点:`strconv.Itoa(i)+node` 是**编号在前、节点名在后**(如 `"12"`=i=1,node="2")。所以虚拟节点 "12" 属于节点 "2",不是节点 "6"!测试里我一开始算错就是这个——`hashMap[12]="2"`。

### `Get(key)`:顺时针找最近节点

```go
func (m *Map) Get(key string) string {
    if m.IsEmpty() { return "" }
    hash := int(m.hash([]byte(key)))
    // 二分:环上第一个 >= hash 的虚拟节点
    idx := sort.Search(len(m.keys), func(i int) bool {
        return m.keys[i] >= hash
    })
    if idx == len(m.keys) {
        idx = 0  // 比所有都大,绕回环首(环的"顺时针绕回")
    }
    return m.hashMap[m.keys[idx]]
}
```

**`sort.Search` 是这里的关键**。它是 Go 标准库的二分查找:

```go
sort.Search(n, func(i int) bool { ... })
// 在 [0, n) 二分,返回满足 f(i)==true 的最小 i
// 若都不满足,返回 n
```

我们要"环上第一个 >= hash 的位置"——定义 `f(i) = keys[i] >= hash`,这是单调的(环已排序),`sort.Search` 二分 O(log n) 找到。

- 正常情况:`idx` 指向顺时针最近虚拟节点。
- `idx == len(keys)`:hash 比环上所有都大,顺时针绕回环首 `keys[0]`(环首尾相接,取模语义)。

最后 `hashMap[keys[idx]]` 把虚拟节点翻译回真实节点返回。

---

## 五、测试验证的核心点

### `TestHashing`:节点增减影响小范围

```
环: 2,4,6,12,14,16,22,24,26 (节点 2,4,6 各 3 虚拟节点)
Get("27") → 27 比所有大,绕回 2 → 节点 "2"

加节点 "8" → 环多了 8,18,28
Get("27") → 顺时针最近 28 → 节点 "8"   ← 迁到 8
Get("11") → 顺时针最近 12 → 节点 "2"   ← 不变!
```

这验证了一致性哈希的核心价值:**加节点只接管相邻区间(27→8),其余 key(11)映射不变**。对照普通哈希"几乎全变",差异巨大。

### `TestReplicate`:单节点多虚拟节点不崩

单真实节点+50虚拟节点,所有 key 都映射到它——验证虚拟节点机制本身能正确工作,不会因环上只有虚拟节点而错乱。

### `TestEmpty`:空环返回空串

防御性:环上没节点时 `Get` 返回 `""`,不 panic。

---

## 六、几个值得讲的点

### 1. 为什么用 `sort.Search` 而非 `map` 直接查

环上要找"顺时针最近",不是精确等于 hash 的虚拟节点(大概率没有精确等于的)。这是**范围查询**(>=hash 的最小值),有序切片 + 二分是标准解法,O(log n)。`map` 只能精确匹配,做不到。

### 2. `hashMap` 为什么必要

环上 `keys` 存的是**虚拟节点**哈希(数量 = 节点数×replicas),但 `Get` 要返回**真实节点**。虚拟节点很多、真实节点少,需要 `hashMap[虚拟哈希]=真实节点` 做翻译。没有它,环上全是虚拟节点,没法还原成真实节点名。

### 3. 虚拟节点名拼接 `strconv.Itoa(i)+node`

为什么不是 `node+strconv.Itoa(i)`?都行,只要保证不同(虚拟节点名)/(真实节点,编号)对能哈希到不同值。GeeCache 用 `i+node`。**重点是编号要参与哈希**,否则同一个节点的 replicas 个虚拟节点会哈希成同一个值(全撞),虚拟节点就没意义了。

### 4. 并发:`sync.RWMutex`

`Add` 写(`keys` 追加+排序、`hashMap` 写)用写锁,`Get`/`IsEmpty` 读用读锁。让多读并行。这是本包自带的并发保护,不依赖上层。

---

## 七、Day4 在整个架构的位置

```
Day1 eviction ── Day2 cache/Group ── Day3 HTTPPool ── Day4 一致性哈希
   淘汰           单机门面            HTTP服务端      决定 key 去哪个节点
                                                      │
                                                      ▼
   Day5 分布式节点:Group.load → PickPeer(用一致性哈希选节点) → HTTP 取
```

Day4 的 `consistenthash.Map` 是 Day5 的**选节点工具**:Day5 的 `HTTPPool.Set(peers)` 把节点列表喂给一个 `Map`,`PickPeer(key)` 用 `Map.Get(key)` 选出该 key 该去哪个节点。所以 Day4 是独立算法包,Day5 把它接入主流程。

---

## 八、当前边界

| 缺的东西 | 归属 | 说明 |
| --- | --- | --- |
| 接入主流程 | Day5 | 本包是独立算法,还没连到 Group/HTTPPool |
| 节点下线删除 | 可选扩展 | `Add` 有,`Remove` 没实现;ggcache 动态节点需要 |
| 数据倾斜量化 | 可选 | 没做"统计各节点 key 占比"的测试,可补 |

---

## 附:文件清单

| 文件 | 职责 |
| --- | --- |
| `consistenthash/consistenthash.go` | `Map` 一致性哈希环:Add 撒虚拟节点、Get 二分找顺时针最近节点 |
| `consistenthash_test.go` | 节点增减影响范围、虚拟节点、空环测试 |
