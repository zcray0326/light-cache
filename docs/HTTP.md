# Day3:HTTP 服务端实现详解

> 本文档讲清 light-cache Day3 的设计:`HTTPPool` 实现 `http.Handler`,把缓存通过 HTTP 暴露出去。
> 配套代码:`internal/cache/http.go` / `cmd/server/main.go`。
> 参考:GeeCache Day3 博客。

---

## 一、Day3 在做什么

Day2 做出了"单机缓存 + 回源"的 `Group`,但只能**进程内调用**(`gee.Get(key)`)。Day3 要让它能**跨进程/跨节点访问**——给缓存套一个 HTTP 服务端,别的节点(或外部客户端)能通过 HTTP 请求拿到本节点的缓存数据。

```
            Day2 已有                          Day3 新增
┌────────────────────────┐         ┌─────────────────────────────┐
│  Group.Get(key)        │  ←进程内 │  HTTPPool.ServeHTTP        │  ←跨进程
│  (库调用,同进程)      │         │  解析 URL → 调 Group.Get    │
└────────────────────────┘         │  写 HTTP 响应               │
                                   └──────────────┬──────────────┘
                                                  │ HTTP
                                          外部客户端 / 其他节点
                                       curl http://node/_lightcache/scores/Tom
```

`HTTPPool` 是个**适配层**:HTTP 请求进来 → 解析出 group 名和 key → 调 Day2 的 `Group.Get` → 把结果写成 HTTP 响应。它不碰缓存逻辑本身,只做"协议转换"。

---

## 二、核心:HTTPPool 实现 http.Handler

### Go 的 `http.Handler` 接口

Go 标准库 `net/http` 定义:

```go
type Handler interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request)
}
```

任何实现了 `ServeHTTP` 方法的类型,都能当 HTTP 处理器。`http.ListenAndServe(addr, handler)` 会把每个请求交给 `handler.ServeHTTP`。这是 Go 的"隐式接口"——`HTTPPool` 加个 `ServeHTTP` 方法就自动满足 `Handler`,不用 `implements` 声明。

```go
type HTTPPool struct {
    self     string // 本节点地址,如 "localhost:9999"
    basePath string // 统一前缀,默认 /_lightcache/
}

func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }
```

### ServeHTTP 的工作流程

约定 URL 格式:`/<basePath>/<groupname>/<key>`,例如 `/_lightcache/scores/Tom`。

```
请求: GET /_lightcache/scores/Tom

ServeHTTP 处理:
  1. 校验前缀:Path 必须以 basePath 开头,否则 panic(配置错误,该早暴露)
  2. 去前缀 → "scores/Tom"
  3. SplitN("/", 2) → ["scores", "Tom"]   → groupName="scores", key="Tom"
  4. GetGroup("scores") → 查全局表拿 Group,不存在返回 404
  5. group.Get("Tom") → ByteView(命中或回源)
  6. w.Write(view.ByteSlice()) → 以二进制流返回
```

```go
func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !strings.HasPrefix(r.URL.Path, p.basePath) {
        panic("HTTPPool serving unexpected path: " + r.URL.Path)
    }
    p.Log("%s %s", r.Method, r.URL.Path)

    parts := strings.SplitN(r.URL.Path[len(p.basePath):], "/", 2)
    if len(parts) != 2 {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    groupName, key := parts[0], parts[1]

    group := GetGroup(groupName)
    if group == nil {
        http.Error(w, "no such group: "+groupName, http.StatusNotFound)
        return
    }

    view, err := group.Get(key)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Write(view.ByteSlice())
}
```

---

## 三、几个值得讲清的点

### 1. 路径解析:`SplitN` 为什么用 2

```go
parts := strings.SplitN(r.URL.Path[len(p.basePath):], "/", 2)
```

`SplitN(s, sep, 2)` 表示**最多切 2 段**:

- `"scores/Tom"` → `["scores", "Tom"]` ✓
- `"scores/a/b/c"` → `["scores", "a/b/c"]` ✓(key 允许含 `/`,第二段整体当 key)

如果用 `strings.Split`(全切),`"scores/a/b"` 会被切成 `["scores","a","b"]` 三段,key 含 `/` 就解析不了。`SplitN(..., 2)` 保证第二段以后的整体性——**key 可以含 `/`**。

### 2. `[len(p.basePath):]` 切片去掉前缀

```go
r.URL.Path[len(p.basePath):]
// "/_lightcache/scores/Tom"[len("/_lightcache/"):] = "scores/Tom"
```

用前缀长度做切片起点,把前缀砍掉,只留后面的 `group/key` 部分。这是处理固定前缀的惯用法。

### 3. 响应类型:`application/octet-stream`

```go
w.Header().Set("Content-Type", "application/octet-stream")
w.Write(view.ByteSlice())
```

`octet-stream` 是"任意二进制流"的 MIME 类型。缓存值不一定是文本(可能是图片、序列化数据),用二进制流最通用。**Day7 会把这里换成 protobuf**,提升节点间传输效率——届时 body 是 protobuf 编码的 `Response`,而非裸字节。

### 4. 错误码语义

| 情况 | 状态码 | 来源 |
| --- | --- | --- |
| URL 格式错(非 2 段) | 400 Bad Request | `http.Error(...StatusBadRequest)` |
| group 不存在 | 404 Not Found | group 没注册 |
| group.Get 内部错(回源失败等) | 500 Internal Server Error | 透传 `err.Error()` |
| key 为空 | 500(走 Get 的 "key is required") | 空串能被 SplitN 切成 `["group",""]`,key="",Get 报错 |

> 注:Day3 这版空 key 会走 500(因为 `SplitN` 把 `scores/` 切成 `["scores",""]`,key="",`Group.Get("")` 返回 "key is required")。这与 GeeCache 原版行为一致。若要更严谨,可在 ServeHTTP 里对空 key 提前返回 400——属于可选优化。

### 5. `panic` 而非 `http.Error`(前缀不匹配)

```go
if !strings.HasPrefix(r.URL.Path, p.basePath) {
    panic("HTTPPool serving unexpected path: " + r.URL.Path)
}
```

前缀不匹配说明**路由配置错了**(HTTPPool 被挂到了不该挂的路径下),这是开发者 bug,不该在运行时"温柔降级"返回 404——直接 panic 让问题尽早暴露。和 Day2 `newCache` 里 `panic(err)` 同理:配置类错误 fail-fast,运行时数据错误才用 HTTP 错误码。

---

## 四、示例服务端 `cmd/server/main.go`

```go
func main() {
    cache.NewGroup("scores", 2<<10, "lru", cache.GetterFunc(
        func(key string) ([]byte, error) {
            if v, ok := db[key]; ok { return []byte(v), nil }
            return nil, fmt.Errorf("%s not exist", key)
        }))

    peers := cache.NewHTTPPool("localhost:9999")
    log.Fatal(http.ListenAndServe("localhost:9999", peers))
}
```

- 注册 `scores` group(底层用 LRU,Day2 的多策略在此生效)。
- `NewHTTPPool` 造处理器,`http.ListenAndServe` 起服务。

### 为什么 `main.go` 放 `cmd/server/` 而非根目录

根目录的 `.go` 文件是 `package cache`(库)。如果再在根目录放 `main.go`(`package main`),**同目录两个不同包名,Go 编译器拒绝**。所以可执行入口放 `cmd/server/` 子目录,和库代码隔离。这也是 Go 项目工程化惯例(`cmd/` 放可执行入口)。

### 端到端验证(已跑通)

```
curl http://localhost:9999/_lightcache/scores/Tom    → 630         (回源 db 命中)
curl http://localhost:9999/_lightcache/scores/kkk   → kkk not exist (回源失败,500)
curl http://localhost:9999/_lightcache/nope/Tom     → HTTP 404    (group 不存在)
curl http://localhost:9999/_lightcache/scores/      → HTTP 500    (空 key)
```

服务端日志:`GET /_lightcache/scores/Tom` → `[SlowDB] search key Tom`(回源),验证请求确实走到 Day2 的 Group。

---

## 五、Day3 在整个架构的位置

```
Day1 eviction  ── Day2 cache(并发)+ Group(回源) ── Day3 HTTPPool(HTTP 暴露)
   淘汰            单机缓存门面               跨进程访问的服务端

后续:
  Day4 一致性哈希 ── 决定 key 该去哪个节点
  Day5 分布式节点 ── HTTPPool 加 Set/PickPeer,变服务端+客户端;Group.load 先试远程
  Day7 protobuf  ── HTTP body 换 protobuf 编码
```

Day3 的 `HTTPPool` 现在**只是服务端**(只响应请求)。Day5 会给它加 `Set(节点列表)` 和 `PickPeer(选节点)`,让它同时是**客户端**(主动向别的节点发请求取数据)。届时 `Group.load` 的流程会变成:先 `PickPeer` 选远程节点 → HTTP 取 → 失败再回退本地 `getLocally`。

---

## 六、当前边界

| 缺的东西 | 归属 | 说明 |
| --- | --- | --- |
| 客户端能力 | Day5 | HTTPPool 只能"被访问",不能"主动访问别人";Day5 加 PickPeer |
| 节点选择 | Day4-5 | 还不知道 key 该去哪个节点;一致性哈希决定 |
| 并发回源去重 | Day6 | singleflight |
| protobuf 编码 | Day7 | 现在是裸字节,传输效率一般 |
| 测试 | — | Day3 逻辑简单,靠端到端 curl 验证;后续可补 `http_test.go` |

---

## 附:文件清单

| 文件 | 职责 |
| --- | --- |
| `http.go` | `HTTPPool` 实现 `http.Handler`,路径解析 + 调 Group.Get,返回字节流 |
| `cmd/server/main.go` | 示例服务端入口,注册 Group + `ListenAndServe` |
