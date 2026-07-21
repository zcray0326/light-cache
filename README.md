# light-cache

> 从零实现的分布式缓存。学习自 [GeeCache](https://geektutu.com/post/geecache-day1.html),目标是逐步向 [ggcache](https://github.com/1055373165/ggcache) 扩展。秋招项目。

## 7 天路线(GeeCache)

| Day | 主题 | 关键技术 | 状态 |
| --- | --- | --- | --- |
| 1 | LRU 缓存淘汰 | map + 双向链表,O(1) 增删改查 | ✅ |
| 2 | 单机并发缓存 | sync.Mutex 封装 + ByteView 只读 + Group + Getter 回调(底层策略可配 LRU/FIFO) | ✅ |
| 3 | HTTP 服务端 | HTTPPool 实现 Handler,路径解析 `/_lightcache/<group>/<key>` | ✅ |
| 4 | 一致性哈希 | 哈希环 + 虚拟节点 + sort.Search 二分 | ✅ |
| 5 | 分布式节点 | PeerPicker/PeerGetter 接口 + PickPeer 选节点 + 远程获取 + 回退本地 | ✅ |
| 6 | 防缓存击穿 | singleflight,同 key 并发只回源一次 | ✅ |
| 7 | Protobuf 通信 | ⏭️ 跳过:此场景 protobuf 收益微乎其微,改在 ggcache 阶段用 gRPC 实践 | ⏭️ |

## ggcache 扩展点(7 天之后)

- HTTP + gRPC 双协议
- etcd 服务注册发现 + 动态节点 + failover(3 节点 etcd 集群,lease+watch,对齐 ggcache)—— ✅
- Proxy/Store 角色分离(接入层收敛防护,存储层纯存数据,对齐生产级架构)—— ✅
- 多淘汰算法(LRU / LFU / FIFO / ARC,策略模式)—— LRU/FIFO/LFU ✅,ARC 待加
- singleflight 结果缓存
- TTL 自动清理(绝对过期 + 惰性 + 后台,对齐 Redis)—— ✅
- per-key TTL(每条数据自己的过期时长,对标 Redis EXPIRE;顺带修复空值缓存"没配全局 TTL 就永不过期"缺陷)—— 待做
- 缓存穿透防御(空值缓存,占位符字符串对齐 Redisson)—— ✅
- 请求重试与退避
- 缓存分段(细粒度锁)
- Prometheus + Grafana 监控
- CI/CD

## 本地运行

```bash
go test ./...          # 跑所有测试
docker-compose up -d   # 起 3 节点 etcd + 3 缓存节点 + 1 API 节点(详见 docs/ETCD.md)
curl "http://localhost:9999/api?key=Tom"   # → 630
```

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/EVICTION.md](docs/EVICTION.md) | Day1 淘汰策略:策略模式、map+双向链表、LRU vs FIFO |
| [docs/CACHE.md](docs/CACHE.md) | Day2 单机并发:ByteView 只读、Mutex 并发封装、Group 回源门面 |
| [docs/HTTP.md](docs/HTTP.md) | Day3 HTTP 服务端:HTTPPool 实现 Handler、路径解析 |
| [docs/CONSISTENTHASH.md](docs/CONSISTENTHASH.md) | Day4 一致性哈希:哈希环、虚拟节点、sort.Search 二分 |
| [docs/DISTRIBUTED.md](docs/DISTRIBUTED.md) | Day5 分布式节点:PeerPicker/PeerGetter、选节点、先远程后回退 |
| [docs/SINGLEFLIGHT.md](docs/SINGLEFLIGHT.md) | Day6 防缓存击穿:singleflight 并发去重、WaitGroup 等待、去重时序特性 |
| [docs/LFU.md](docs/LFU.md) | 扩展:LFU 淘汰策略,最小堆实现,与 LRU 哲学差异,开闭原则兑现 |
| [docs/TTL.md](docs/TTL.md) | 扩展:TTL 过期清理,绝对过期(对齐 Redis lazy+active)、惰性+后台、方案 B 无跨层状态、WithTTL 函数式选项 |
| [docs/NULLCACHE.md](docs/NULLCACHE.md) | 扩展:缓存穿透防御,空值占位符(对齐 Redisson)、sentinel error、靠内容判别合法空值、防击穿+防穿透协作 |
| [docs/ETCD.md](docs/ETCD.md) | 扩展:etcd 服务发现,3 节点 etcd 集群(Raft)、lease+watch、watch 信号触发全量重建环、和 HTTPPool.Set 衔接、docker-compose demo |
| [docs/PROXY.md](docs/PROXY.md) | 扩展:Proxy/Store 角色分离,防护收敛接入层、存储层纯存数据、对等架构硬伤对比、allowLocalFallback、空值缓存归属 |

## 参考

- [GeeCache 7 天博客](https://geektutu.com/post/geecache-day1.html)
- [ggcache](https://github.com/1055373165/ggcache)
