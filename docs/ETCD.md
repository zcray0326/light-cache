# etcd 服务发现

> 扩展点:节点列表从硬编码改为 etcd 动态发现,light-cache 从"伪分布式"变"真分布式"。

## 为什么要服务发现

之前的分布式是"伪分布式":节点列表**硬编码**在 `cmd/server/main.go` 的 `addrMap`,加/删节点都得改代码重启。etcd 服务发现后,节点启动时往 etcd 注册自己,其他节点 watch etcd 拿实时列表,节点拓扑**动态变化、无需改代码**。

## 两种"节点"别混淆

| | 是什么 | 作用 | 数量怎么定 |
| --- | --- | --- | --- |
| **etcd 集群节点** | etcd 自己的存储实例 | 存"服务注册表"(谁在线),Raft 复制 | 3/5/7 奇数,保 etcd 自己高可用(容忍少数派挂) |
| **light-cache 缓存节点** | 你的缓存服务实例 | 存缓存数据、处理 Get | 任意多个,动态增减 |

etcd 的"分布式"指**它自己高可用**(Raft 复制数据到多实例),不是指缓存节点数量无限。缓存节点数量由"起了几个 light-cache 进程并注册"决定,etcd 只被动记录。

## 机制(对齐 ggcache 思路,裸 KV 实现)

```
节点启动:
  1. 注册自己到 etcd(lease TTL 10s + KeepAlive 续约)
  2. ListPeers 拉一次当前节点列表 → HTTPPool.Set 初始化一致性哈希环
  3. 后台 goroutine watch etcd:任何节点 put/delete → 发信号 → 重新 ListPeers → HTTPPool.Set 全量刷环

节点崩溃:
  - KeepAlive 停止 → lease 10s 过期 → etcd 自动删 key
  - 其他节点 watch 到 delete → 刷环 → 不再路由到挂的节点
```

**关键设计**:watch 只发"有变化"信号,**不传具体增删节点**;收到信号重新 ListPeers 拉全量,再 `HTTPPool.Set` **全量重建环**(不是增量 add/remove)。全量重建简单可靠,避免增量要处理"同节点先删后加"等边角。

### 裸 KV 而非 endpoints.Manager

ggcache 用 etcd 官方的 `endpoints.Manager`(gRPC 服务发现标准封装)。light-cache 不走 gRPC,用裸 KV 更直观:

- key 格式:`/light-cache/nodes/<addr>`,value 是 `<addr>`
- 注册:`cli.Put(key, addr, WithLease(leaseId))`
- 发现:`cli.Get(prefix, WithPrefix)` 拿所有节点
- 监听:`cli.Watch(prefix, WithPrefix)`,任何事件发信号

### 和 HTTPPool.Set 的衔接(架构契合)

`HTTPPool.Set`(http.go)本身就是**全量替换语义**——注释明说"便于支持动态增减节点":每次调用新建一致性哈希环 + 重建 httpGetters map,内部 `mu` 保护并发安全。所以 watch 信号触发重新 List + Set,**http.go 完全不用改**。`RegisterPeers` 只在启动时注入一次,之后靠 Set 刷环更新节点列表,Group 无感知。

## lease + KeepAlive 下线

```go
lease, _ := cli.Grant(ctx, 10)                              // TTL 10s
cli.Put(ctx, key, addr, clientv3.WithLease(lease.ID))       // 绑定 lease
keepRespCh, _ := cli.KeepAlive(ctx, lease.ID)               // 自动续约
// 进程活着就续约,崩溃了不续约 → 10s 后 lease 过期 → etcd 自动删 key
```

节点崩溃 → KeepAlive 停 → 10s 后 lease 过期 → 其他节点 watch 到 delete → 刷环。**5~10s 僵尸窗口**(lease 过期前其他节点还可能路由到挂的节点)靠 `group.load` 的回退逻辑兜底:远程取失败 → 回退本地回源,不致整个请求失败。后续可加优雅退出(主动 Delete key)消除窗口。

## 3 节点 etcd 集群(高可用)

docker-compose 起 3 个 etcd 实例(etcd0/etcd1/etcd2),Raft 协议:

- `initial-cluster` 三节点互联
- 写入要多数派(2/3)确认,容忍挂 1 个
- client 配 3 个 endpoint,自动故障转移(挂的少数派不影响)

demo 和生产同款配置(不区分),注册中心本身高可用。

## 启动与演示

### 一键起(docker-compose)

```bash
docker-compose up -d
# 起 3 节点 etcd 集群 + 3 个对等缓存节点(node1:8001 node2:8002 api:9999)
```

### 验证服务发现

```bash
# etcd 里看注册的节点
docker exec light-cache-etcd0-1 etcdctl --endpoints=http://localhost:2379 get --prefix /light-cache/nodes/
# /light-cache/nodes/http://node1:8001
# http://node1:8001
# ...node2、node3
```

### 验证分布式路由

```bash
curl "http://localhost:9999/api?key=Tom"   # → 630(对等节点,未命中按一致性哈希选远程节点取)
curl "http://localhost:9999/api?key=Jack"  # → 589
```

### 验证动态下线

```bash
docker stop light-cache-node1-1          # 下线一个节点
sleep 12                                  # 等 lease 10s 过期 + watch 刷环
curl "http://localhost:9999/api?key=Sam" # → 567(自动路由到 node2/3,不再路由到 node1)
docker logs light-cache-node2-1 | tail   # 看到 peers refreshed 少了 node1
```

### 验证恢复

```bash
docker start light-cache-node1-1          # 恢复
sleep 5                                    # watch 触发
docker logs light-cache-node2-1 | tail     # node1 重新进环
```

## 地址一致性(踩坑提醒)

`PickPeer` 靠 `peer != p.self` 排除自己(避免自己请求自己死循环)。所以:
- `HTTPPool.self`(本节点地址)和
- etcd 里注册的 addr

**必须格式完全一致**(都 `http://host:port`)。docker-compose 里用 service 名做 DNS(`http://node1:8001`),本节点 `selfAddr` 也用 `http://node1:8001`,两边对齐。若一边 `localhost` 一边容器名,排除不掉自己 → 死循环。

## 相关文件

- `internal/discovery/etcd.go` — Register(lease+keepalive)、ListPeers、WatchPeers
- `cmd/server/main.go` — 接 etcd:client 初始化、Register、ListPeers 初始化环、WatchPeers goroutine 刷环、`-etcd`/`-host` flag
- `docker-compose.yml` — 3 节点 etcd 集群 + 3 个对等缓存节点
- `Dockerfile` — 多阶段构建(builder 编译 + alpine 运行镜像)
- 不改:`internal/cache/http.go`(HTTPPool.Set 已是全量替换)、`group.go`(RegisterPeers 注入一次)、`consistenthash/`(全量重建)
