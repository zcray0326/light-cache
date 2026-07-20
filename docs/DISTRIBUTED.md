# Day5:分布式节点实现详解

> 本文档讲清 light-cache Day5 的设计:把前四天串成分布式缓存——`HTTPPool` 变身客户端+服务端、`Group.load` 先选远程节点取数、失败回退本地。
> 配套代码:`internal/cache/peers.go`(新)/ `internal/cache/http.go`(改)/ `internal/cache/group.go`(改)/ `cmd/server/main.go`(改)。
> 参考:GeeCache Day5 博客。

---

## 一、Day5 把前面四天串起来

前四天各管一摊:

```
Day1 eviction        ─ 怎么淘汰
Day2 cache/Group     ─ 单机并发 + 未命中回源
Day3 HTTPPool(服务端)─ HTTP 暴露缓存
Day4 consistenthash  ─ key 该去哪个节点(独立算法)
```

Day5 用两个接口 + 几行接线,把它们连成一个**分布式缓存**:

```
客户端请求 key
    │
    ▼
Group.Get(key)
    │ 本地未命中
    ▼
Group.load(key)  ◄── Day5 改这里:先远程,失败回退本地
    │
    ├─ peers != nil?
    │     ├─ PickPeer(key)  ◄── 一致性哈希(Day4)选节点
    │     │     ├─ 选到远程节点(不是自己)
    │     │     └─ 返回 httpGetter(指向该节点的 HTTP 客户端)
    │     ├─ getFromPeer → httpGetter.Get(group, key)
    │     │     └─ HTTP GET 远程节点 /_lightcache/<group>/<key>   ◄── Day3 服务端响应
    │     │     ├─ 成功 → 返回(不写本地缓存)
    │     │     └─ 失败 → 回退 ↓
    │     └─ (无 peers 或全部失败)
    ▼
getLocally → getter.Get(key)  ◄── Day2 回源
    └─ 写回本地缓存
```

核心思想:**未命中时,先看这个 key 是不是该归别的节点管,是就去别的节点取;取不到再自己回源。** 这样 N 个节点分担 N 份数据,缓存总容量随节点数线性扩展。

---

## 二、两个核心接口(`peers.go`)

Day5 的设计精髓是定义两个小接口,把"选节点"和"取数据"解耦:

```go
// PeerPicker:给定 key,选出该 key 归属的远程节点(返回它的取数客户端)。
type PeerPicker interface {
    PickPeer(key string) (peer PeerGetter, ok bool)
}

// PeerGetter:从某个远程节点取回指定 group 的 key 对应值。
type PeerGetter interface {
    Get(group string, key string) ([]byte, error)
}
```

### 为什么是接口,不直接用具体类型?

因为"选节点的方式"和"取数据的方式"可能有多种实现:

- 选节点:今天用一致性哈希(Day4),以后可用 etcd 服务发现(ggcache)
- 取数据:今天用 HTTP,以后可加 gRPC(ggcache)

`Group` 只依赖 `PeerPicker`/`PeerGetter` **接口**,不关心背后是 HTTP 还是 gRPC、是一致性哈希还是 etcd。这是依赖倒置——核心逻辑(回源、缓存)稳定,通信方式可替换。ggcache 正是靠这两个接口扩展出双协议。

### 接口对应谁实现

| 接口 | 实现者 | 怎么实现 |
| --- | --- | --- |
| `PeerPicker` | `*HTTPPool` | `PickPeer` 用一致性哈希选节点,返回该节点的 `httpGetter` |
| `PeerGetter` | `*httpGetter` | `Get` 拼 URL 发 HTTP 请求取字节 |

`HTTPPool` 一个人实现了 `PeerPicker`(选)+ 内含 `httpGetter` 实现 `PeerGetter`(取)。这是"一个结构体 + 一个辅助结构体"的组合。

---

## 三、HTTPPool 升级:服务端 + 客户端(`http.go`)

Day3 的 `HTTPPool` 只实现 `ServeHTTP`(服务端)。Day5 加 `Set` + `PickPeer`(客户端)+ `httpGetter`。

### 新增字段

```go
type HTTPPool struct {
    self        string                     // 本节点地址
    basePath    string
    mu          sync.Mutex                 // 保护 peers/httpGetters
    peers       *consistenthash.Map        // 一致性哈希环(Day4)
    httpGetters map[string]*httpGetter     // 节点地址 → 其 HTTP 客户端
}
```

### `Set(peers...)`:注册节点,建哈希环

```go
func (p *HTTPPool) Set(peers ...string) {
    p.peers = consistenthash.New(defaultReplicas, nil)  // 50 虚拟节点
    p.peers.Add(peers...)
    p.httpGetters = make(map[string]*httpGetter, len(peers))
    for _, peer := range peers {
        p.httpGetters[peer] = &httpGetter{baseURL: peer + p.basePath}
    }
}
```

- 用 `consistenthash.New(50, nil)` 建环,`Add` 把所有节点撒上去(Day4)。
- 为每个节点建一个 `httpGetter`(HTTP 客户端),缓存起来复用(避免每次请求新建客户端)。
- `baseURL = peer + basePath`,如 `http://localhost:8001/_lightcache/`。

### `PickPeer(key)`:选节点,实现 PeerPicker

```go
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
    if peer := p.peers.Get(key); peer != "" && peer != p.self {
        return p.httpGetters[peer], true
    }
    return nil, false
}
```

- `p.peers.Get(key)` —— Day4 的一致性哈希,返回 key 该去的真实节点。
- `peer != p.self` —— **排除自己**:如果选出来是自己,说明这个 key 该本地处理,不远程(否则自己 HTTP 请求自己,死循环)。这是关键防呆。
- 返回该节点的 `httpGetter`(实现 PeerGetter)。

### `httpGetter.Get`:HTTP 客户端,实现 PeerGetter

```go
func (h *httpGetter) Get(group string, key string) ([]byte, error) {
    u := fmt.Sprintf("%v%v/%v", h.baseURL, url.QueryEscape(group), url.QueryEscape(key))
    res, err := http.Get(u)
    ...
    return io.ReadAll(res.Body)
}
```

- 拼 URL:`baseURL/group/key`,group/key 用 `url.QueryEscape` 转义(防含特殊字符如 `/`、空格破坏 URL)。
- `http.Get` 发请求,读响应体返回字节。

### 两个编译期断言

```go
var _ PeerPicker = (*HTTPPool)(nil)
var _ PeerGetter = (*httpGetter)(nil)
```

这是 Go 惯用法:**编译期检查某类型实现了某接口**。`var _ I = (*T)(nil)` 声明一个不用的接口变量,赋 `*T` 的 nil。如果 `T` 没实现 `I`,编译就报错。比运行时发现"方法签名对不上"早得多。改接口/方法时,这个断言会立刻提示所有实现者。

---

## 四、Group 接入分布式(`group.go`)

### 新增 `peers` 字段 + `RegisterPeers`

```go
type Group struct {
    ...
    peers PeerPicker  // 为 nil 表示单机模式;非 nil 走分布式
}

func (g *Group) RegisterPeers(peers PeerPicker) {
    if g.peers != nil {
        panic("RegisterPeerPicker called more than once")
    }
    g.peers = peers
}
```

- `peers` 为 nil 时,`load` 直接本地回源——**兼容 Day2 单机模式**,不注册就是单机。
- `RegisterPeers` 只许注册一次(重复 panic),防止运行中换拓扑导致状态混乱。

### `load` 改造:先远程,失败回退本地(核心)

```go
func (g *Group) load(key string) (value ByteView, err error) {
    if g.peers != nil {
        if peer, ok := g.peers.PickPeer(key); ok {
            if value, err = g.getFromPeer(peer, key); err == nil {
                return value, nil
            }
            log.Println("[light-cache] Failed to get from peer", err)
        }
    }
    return g.getLocally(key)  // 远程不可用 → 本地回源
}
```

- 有 peers 且选到了远程节点 → `getFromPeer` 去取。
- 取**成功**直接返回;取**失败**(节点挂了、500 等)→ **回退本地** `getLocally`。
- 这就是 failover 雏形:远程挂了不影响功能,只是退化为本地回源。ggcache 的 failover 更完善(多节点重试),但骨架在这里。

### `getFromPeer`:不写本地缓存

```go
func (g *Group) getFromPeer(peer PeerGetter, key string) (ByteView, error) {
    bytes, err := peer.Get(g.name, key)
    ...
    return ByteView{b: bytes}, nil  // 注意:没调 populateCache
}
```

**远程取回的值不写本地缓存**(对比 `getLocally` 会 `populateCache`)。这是 GeeCache 的设计选择:避免每个节点都缓存同一份数据(冗余扩散)。代价是同一 key 反复远程请求。ggcache 的 singleflight 结果缓存会优化这点(Day6)。

---

## 五、多节点示例(`cmd/server/main.go`)

```go
func main() {
    // -port 选节点,-api 启前端 API
    gee := createGroup()  // "scores" 组,LRU,回源 db
    if api { go startAPIServer("http://localhost:9999", gee) }
    startCacheServer(addrMap[port], addrs, gee)
}
```

三种角色:
- **缓存节点**(8001/8002/8003):`RegisterPeers` + `ListenAndServe`,既是服务端(响应 `_lightcache` 请求)也是客户端(可被 PickPeer 选出向别的节点取)。
- **API 节点**(8003 + `-api`):额外起 `:9999/api`,接收外部请求,走 `Group.Get` → 分布式选节点。

### 端到端验证(已跑通)

```
curl http://localhost:9999/api?key=Tom  → 630
curl http://localhost:9999/api?key=kkk → kkk not exist
```

日志实证分布式流程(API 节点 8003 的日志):
```
[Server http://localhost:8003] Pick peer http://localhost:8001   ← 一致性哈希选出 8001
                                                                       (8001 收到请求回源)
[SlowDB] search key Tom                                           ← 8001 回源(远程节点)
[light-cache] hit                                                 ← 第二次 Tom 命中
```

`kkk` 回源失败时:8001 返回 500 → 8003 日志 `Failed to get from peer` → **回退本地** `getLocally` → `[SlowDB] search key kkk` → 返回 `kkk not exist`。failback 生效。

---

## 六、几个值得讲的点

### 1. 为什么 API 节点不回源,缓存节点才回源?

API 节点(8003 带 `-api`)的 `Group.Get` 未命中时:`load` → `PickPeer` 选远程缓存节点 → 取。**API 节点自己不回源**(`getLocally` 只在远程全失败时才走)。这样回源压力集中在"应该管这个 key 的那个缓存节点",而非任意节点,数据归属清晰。

### 2. `peer != p.self` 排除自己的必要性

如果一致性哈希选出的是自己(完全可能,自己的虚拟节点就在环上),不排除就会 `httpGetter` HTTP 请求自己 → 自己又 `load` → 又选到自己 → ... 实际上会走本地但绕了不必要的 HTTP 圈。排除后直接返回 `ok=false`,`load` 回退本地 `getLocally`,干净。

### 3. `url.QueryEscape` 的作用

group/key 拼进 URL 路径,若 key 含 `/`、空格、`?` 等,会破坏 URL 解析。`QueryEscape` 把它们转义成 `%2F` 等。这是 HTTP 客户端构造 URL 的标准做法。

### 4. `defaultReplicas = 50`

每个真实节点 50 个虚拟节点。太少(如 1)分布不均,太多(如 10000)内存浪费且二分查找略慢。50 是经验值,GeeCache 选它平衡均匀性和开销。

---

## 七、Day5 在整个架构的位置

```
单机(Day1-2)─ HTTP 暴露(Day3)─ 选节点算法(Day4)─ 分布式(Day5)
   │              │                │                  │
   ▼              ▼                ▼                  ▼
 eviction      HTTPPool       consistenthash      peers.go 接口
 cache/Group   (服务端)       (Map.Get)          PickPeer/getFromPeer
                                  │                │
                                  └──── PickPeer 用它选节点 ──┘
                                          │
                                          ▼
                                  Group.load 先远程后本地
```

Day5 之后,light-cache 已是个**可工作的分布式缓存**:多节点分担数据、一致性哈希选节点、HTTP 通信、回源 + 回退。

---

## 八、当前边界

| 缺的东西 | 归属 | 说明 |
| --- | --- | --- |
| 并发回源去重 | Day6 | singleflight:同 key 并发只回源一次,防缓存击穿 |
| protobuf 编码 | Day7 | 现在 HTTP body 是裸字节,可换 protobuf 提升效率 |
| 动态节点增减 | ggcache 扩展 | `Set` 是全量替换,非增量;etcd 服务发现可做动态 |
| gRPC 协议 | ggcache 扩展 | `PeerGetter` 已是接口,加 gRPC 实现即可 |
| 节点健康检查/failover 重试 | ggcache 扩展 | 现在只回退本地,可改为重试其他节点 |
| 缓存穿透/击穿/雪崩防御 | ggcache 扩展 | 空值缓存、singleflight、随机 TTL |

---

## 附:文件清单

| 文件 | 改动 | 职责 |
| --- | --- | --- |
| `peers.go` | 新增 | `PeerPicker`/`PeerGetter` 两个接口 |
| `http.go` | 改 | `HTTPPool` 加 `Set`/`PickPeer`(客户端),`httpGetter`(HTTP 客户端) |
| `group.go` | 改 | `Group` 加 `peers` + `RegisterPeers`,`load` 先远程后回退 + `getFromPeer` |
| `cmd/server/main.go` | 改 | 多节点示例:缓存节点 + 前端 API 节点,`-port`/`-api` flag |
