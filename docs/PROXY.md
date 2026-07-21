# Proxy/Store 角色分离

> 架构升级:把对等节点拆成接入层(Proxy)+ 存储层(Store),防护逻辑收敛在 Proxy,store 纯存数据。

## 为什么要分离(对等架构的硬伤)

之前 light-cache 是对等架构:每个节点既是对外入口(`/api` 转发)又是存储(带 db 回源),靠 `-api` flag 软区分。问题:

- **防穿透资源重复**:每个节点各自一套空值缓存 + sentinel error。N 个节点空值标记存 N 份
- **被恶意 key 打穿的风险落在分片节点**:攻击者用大量不存在的 key 轰炸,一致性哈希撒到各 store,单分片被针对时影响该分片真实数据读写
- **监控分散**:miss/空占位/击穿次数散在各节点,没统一视图
- **API 节点也进环、带 db、能回源**,语义不纯(被路由到会 404 靠回退兜底)

分离后:防护逻辑全在 Proxy,store 纯存数据,职责清晰。

## 角色定义

| | Proxy(接入层 `-mode=proxy`) | Store(存储层 `-mode=store`) |
| --- | --- | --- |
| 对外接口 | `/api?key=xxx`(用户入口,固定 9999) | `/_lightcache/<group>/<key>`(供 Proxy 远程取) |
| 带 db | 不带(不回源) | 带(本地回源 DB) |
| 防穿透 | **空值缓存 + sentinel error,在这层** | 不做(纯存真实数据) |
| 防击穿 | singleflight,在这层 | (store 间互取也用) |
| 进 etcd 环 | **不进**(不背数据分片) | 进(背数据分片,被一致性哈希路由) |
| 一致性哈希 | 持环(从 etcd watch store 列表)算 PickPeer 转发 | 也持环(store 间互取用) |
| 远程失败 | **直接返回 error**(不本地回源) | 回退本地回源 DB |

## 工作流

```
用户 → Proxy(9999,不进环)
  ├─ Group.Get 命中本地缓存(含空值占位符)→ 直接返回
  └─ 未命中 → 一致性哈希 PickPeer 选 store
       ├─ store 命中 → 返回值
       ├─ store 404(ErrKeyNotFound)→ Proxy 塞空值占位符 + 返回 not-found(防穿透)
       └─ store 500(故障)→ Proxy 返回 error(不回源)
```

**关键**:Proxy 不回源(不带 db)。store not-found 时,Proxy 塞空值占位符到**自己的本地缓存**,下次同 key 命中占位符挡住,不再调 store。空值缓存只存在于 Proxy 层,store 的缓存不沾空值。

## 实现

### Group 角色感知(allowLocalFallback)

```go
// group.go
type Group struct {
    ...
    allowLocalFallback bool  // 远程失败是否回退本地回源:Store=true,Proxy=false
}

func WithProxyMode() GroupOption {
    return func(g *Group) {
        g.allowLocalFallback = false
        g.getter = noopGetter{}  // 兜底,实际不调(不回源)
    }
}

// load:远程失败按角色分流
if peer, ok := g.peers.PickPeer(key); ok {
    if value, err = g.getFromPeer(peer, key); err == nil {
        return value, nil
    }
    if !g.allowLocalFallback {
        // Proxy:不回源。远程确认 not-found → 塞空值占位符(防穿透在 Proxy 层)
        if errors.Is(err, ErrKeyNotFound) {
            g.populateCache(key, ByteView{b: []byte(nullPlaceholder)})
        }
        return ByteView{}, err
    }
    // Store:回退本地回源
}
return g.getLocally(key)
```

### 空值缓存归属变化

- **之前**(对等):空值缓存在 `getLocally`(每个节点都做,包括 store)
- **现在**(分离):空值缓存在 `load` 的 **Proxy 分支**(store 返回 ErrKeyNotFound 时 Proxy 塞);store 的 getLocally **不做空值缓存**,not-found 透传给 Proxy

### HTTP 状态码语义(store ↔ Proxy 通信)

store 的 ServeHTTP 和 Proxy 的 getFromPeer 要对 not-found 达成一致:
- **store ServeHTTP**:`ErrKeyNotFound → 404`,其他 error → 500
- **Proxy getFromPeer**:404 → `fmt.Errorf("%w: %v", ErrKeyNotFound, ...)`(用 `%w` 包,让 `errors.Is` 识别);500 → 普通 error
- 这样 Proxy 的 load 能用 `errors.Is(err, ErrKeyNotFound)` 识别 not-found,塞占位符防穿透

### etcd 注册(只 store 进环)

- **Store**:调 `discovery.Register` 注册自己进环,被一致性哈希路由
- **Proxy**:**不调 Register**,只 List+Watch 拿 store 列表建环(用于 PickPeer 转发)。Proxy 不背数据,不该被路由

## 启动与验证

### 一键起

```bash
docker-compose up -d
# 3 节点 etcd + 3 store + 1 proxy
```

### 验证角色分离

```bash
# 1. 命中:Proxy 转发 store,store 回源
curl "http://localhost:9999/api?key=Tom"   # → 200, 630

# 2. not-found:store 404,Proxy 塞占位符
curl -o /dev/null -s -w "%{http_code}\n" "http://localhost:9999/api?key=kkk"  # → 404
curl -o /dev/null -s -w "%{http_code}\n" "http://localhost:9999/api?key=kkk"  # → 404(第二次命中占位符,不调 store)

# 3. etcd 只注册 store(proxy 不进环)
docker exec light-cache-etcd0-1 etcdctl get --prefix /light-cache/nodes/
# 只看到 node1/2/3,没有 proxy

# 4. proxy 日志:第二次 kkk 无 "Pick peer"(占位符挡住)
docker logs light-cache-api-1 | grep "Pick peer"
```

## 对比 ggcache

ggcache 是对等架构(每个节点都能接客户端 + 存数据)。light-cache 选 Proxy/Store 分离的好处:
- 防穿透收敛 Proxy 层,store 不被无效请求拖累真实数据
- 防护逻辑集中,易于统一管控和监控
- 空值缓存的 TTL 缺陷影响面缩小(只在 Proxy 层,store 不受牵连)

代价:Proxy→store 多一跳转发延迟。对秋招项目,架构思考的体现 > 一跳延迟。

## 相关文件

- `internal/cache/group.go` — `allowLocalFallback` 字段、`WithProxyMode` 选项、`noopGetter`、load 远程失败按角色分流、空值缓存挪到 load 的 Proxy 分支
- `internal/cache/http.go` — ServeHTTP 把 ErrKeyNotFound 映射 404、getFromPeer 把 404 转成包 ErrKeyNotFound 的 error
- `cmd/server/main.go` — `-mode=proxy|store` flag、startProxyServer(不 Register/不回源)、startStoreServer(注册进环/带 db)
- `docker-compose.yml` — node1/2/3 `-mode=store`,api `-mode=proxy`
- `internal/cache/group_test.go` — mockPeerPicker/mockPeerGetter、TestProxyMode_NoLocalFallback、TestStoreMode_NoNullCache
