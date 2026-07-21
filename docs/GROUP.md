# Group 配置化 + 幂等

> Group 名单从 config.yml 读,启动时批量建;NewGroup 幂等(同名已存在返回旧的)。参考 ggcache 的 GroupManager + 双重检查锁。

## 为什么要配置化

之前 Group 是 main.go 代码写死(建一个 "scores"),且 `NewGroup` **没有幂等性**——`groups[name] = g` 静默覆盖,重复建同名 Group 会丢旧缓存 + 后台 goroutine 泄漏。

配置化后:
- **多 Group** 从配置读,加 Group 改配置重启(不用改代码)
- **NewGroup 幂等**:同名重复建返回同一实例,不覆盖
- **Proxy/Store 各自建**:按角色建该建的(Proxy 建空值缓存用、Store 建回源用)

## config.yml

```yaml
groups:
  - name: scores
    evictionType: lru
    maxBytes: 2048
    ttl: 0            # 不启用 TTL
  - name: users
    evictionType: lfu
    maxBytes: 4096
    ttl: 10m
etcd:
  endpoints:
    - http://etcd0:2379
    - http://etcd1:2379
    - http://etcd2:2379
```

`internal/config/config.go` 用 `gopkg.in/yaml.v3` 解析到全局 `Conf`。`-config` flag 指定路径(默认 `config.yml`)。

## NewGroup 幂等(双重检查锁)

照搬 ggcache 的模式,修正之前"静默覆盖"的缺陷:

```go
func NewGroup(name string, maxBytes int64, evictionType string, getter Getter, opts ...GroupOption) *Group {
    if getter == nil { panic("nil Getter") }
    // 第一次检查(读锁):已存在直接返回
    mu.RLock()
    if g, ok := groups[name]; ok {
        mu.RUnlock()
        return g
    }
    mu.RUnlock()

    mu.Lock()
    defer mu.Unlock()
    // double-check:防并发重复建(两个 goroutine 同时过了第一次检查)
    if g, ok := groups[name]; ok {
        return g
    }
    g := &Group{name: name, getter: getter, loader: &singleflight.Group{}}
    for _, opt := range opts { opt(g) }
    g.mainCache = newCache(evictionType, maxBytes, g.ttl)
    groups[name] = g
    return g
}
```

幂等保证:重复 `NewGroup("scores", ...)` 返回同一个 Group,缓存不丢、goroutine 不泄漏。

## GroupManager:按配置批量建

`buildGroups(role)` 在 main.go 里循环 `config.Conf.Groups`,按 role 建对应模式的 Group:

- `role="store"`:`NewGroup(name, maxBytes, evictionType, sharedGetter)` —— 带回源 getter
- `role="proxy"`:`NewGroup(..., noopGetter, WithProxyMode(), WithTTL(...))` —— 不回源,防穿透在 Proxy

TTL 通过 `WithTTL` 选项传入(NewGroup 构造时应用)。所有 Group 共用一个 `sharedGetter`(查同一个 db),跟 ggcache 一样(先简单;后续要按 group 路由不同数据源再改)。

## Proxy/Store 各自建 Group

- **Proxy 节点**:`buildGroups("proxy")` 建所有 Group(Proxy 模式),持有 GroupManager。Proxy 的 mainCache 只存空值占位符(防穿透)
- **Store 节点**:`buildGroups("store")` 建所有 Group(Store 模式),持有 GroupManager。Store 的 mainCache 存真实数据 + 回源

两者都**对每个 Group 调 RegisterPeers**(避免 ggcache "只给 scores 注册"的 bug——所有 Group 都参与分布式路由)。

## 一致性保证

所有节点跑同一个二进制 + 读同一份 config.yml(挂载进容器),Group 名单天然一致。没有跨节点同步协议(靠配置一致)。请求的 group 名在 URL 里(`/api?group=scores`),Proxy/Store 的 `GetGroup(groupName)` 本地查,查不到返回 nil → 404("no such group")。

## 启动与验证

```bash
# config.yml 配 scores + users 两 Group
docker-compose up -d

# 默认 group=scores
curl "http://localhost:9999/api?key=Tom"                  # → 630

# 显式 group
curl "http://localhost:9999/api?key=Tom&group=scores"      # → 630
curl "http://localhost:9999/api?key=Tom&group=users"       # → 200(独立 group,共用 getter 查同 db)

# 不存在的 group
curl -o /dev/null -s -w "%{http_code}\n" "http://localhost:9999/api?key=Tom&group=nope"  # → 404
```

## 相关文件

- `internal/config/config.go` + `config.yml` —— 配置结构 + yaml 解析 + 示例
- `internal/cache/group.go` —— `NewGroup` 幂等化(双重检查锁)
- `cmd/server/main.go` —— `buildGroups(role)` 按配置批量建 + `-config` flag + 每个_group_RegisterPeers
- `docker-compose.yml` —— 挂 config.yml + 去掉 -etcd(从 config 读)
- `internal/cache/group_test.go` —— `TestNewGroup_Idempotent`、`TestGetGroup_NotFound`
