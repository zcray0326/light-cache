# 缓存穿透防御(空值缓存)

> 扩展点:在已有防击穿(singleflight)+ TTL 基础上,加防穿透(空值缓存),凑齐缓存三件套。

## 为什么要防穿透

缓存三大经典问题:

| 问题 | 含义 | light-cache 对策 |
| --- | --- | --- |
| **击穿** | 热点 key 过期瞬间,大量并发请求一起打 DB | singleflight(已有):同 key 并发回源去重成一次 |
| **穿透** | 大量**不存在的 key** 反复打 DB,缓存永远挡不住 | **空值缓存(本文)**:not-found 也缓存一个占位符短时挡住 |
| **雪崩** | 大量 key 同时过期,瞬间全打 DB | TTL 加随机抖动(可后续) |

穿透的根因:不存在的 key,缓存里没有,每次 Get 都未命中 → 回源打 DB。攻击者用随机 key 轰炸就能压垮 DB。对策:not-found 时也往缓存塞个"占位符",后续同 key 命中占位符直接挡住,不再打 DB。

## 机制:占位符字符串(对齐 Redisson)

not-found 时,往缓存塞一个**固定占位符 ByteView**,内容是约定的 sentinel 字符串 `_LIGHTCACHE_NULL_`。命中时检查 value 内容是否等于占位符,是则当空值标记。

```go
// group.go
var ErrKeyNotFound = errors.New("key not found")
const nullPlaceholder = "_LIGHTCACHE_NULL_"

// getLocally:not-found 时塞占位符
func (g *Group) getLocally(key string) (ByteView, error) {
    bytes, err := g.getter.Get(key)
    if err != nil {
        if errors.Is(err, ErrKeyNotFound) {
            g.populateCache(key, ByteView{b: []byte(nullPlaceholder)}) // 塞占位符
        }
        return ByteView{}, err // 仍返回 error(保持 not-found 语义)
    }
    value := ByteView{b: cloneBytes(bytes)}
    g.populateCache(key, value)
    return value, nil
}

// Group.Get:命中占位符则返回 ErrKeyNotFound
if v, ok := g.mainCache.get(key); ok {
    if string(v.b) == nullPlaceholder { // 靠内容判别
        return ByteView{}, ErrKeyNotFound
    }
    return v, nil
}
```

为什么用占位符字符串而不是 `ByteView{}` 零值?因为分布式缓存(以及任何序列化/跨节点场景)无法靠内存对象状态区分。Redisson(Java 分布式 Redis 封装)内置防穿透就是用固定占位符字符串 `_REDIS_NULL_PLACEHOLDER_`——这是业界成熟方案。本方案对齐这个思路,用值内容判别,通用且不易出错。

## 靠内容判别,合法空值不误判

占位符方案的核心保证:**合法空值不会被误判成空值标记**。

- 空值标记 `ByteView{b: []byte("_LIGHTCACHE_NULL_")}`:内容是 `_LIGHTCACHE_NULL_`
- 合法空值 `ByteView{b: []byte("")}`(DB 里某 key 真值是空字符串):内容是 `""`

`string(v.b) == nullPlaceholder` 只对占位符成立,合法空值的内容是 `""`,不等于占位符,**不会被误判**。靠值内容区分,不依赖 `b == nil` 之类的内存对象状态约定,严格且通用。

## sentinel error 识别 not-found

`getLocally` 用 `errors.Is(err, ErrKeyNotFound)` 判断是否 not-found:

- getter 确认"key 不存在"时显式返回 `ErrKeyNotFound` → 缓存占位符
- getter 返回其他 error(DB 连接失败、超时等真错误)→ **不缓存占位符**,直接透传 error

这避免了 ggcache 的契约缺陷:ggcache 的 retriever 在 not-found 时返回 `[]byte{}, nil`(空 slice + nil error),但 getLocally 又用 `err != nil` 判断——契约不自洽,空值标记和"合法空值"走岔路。本方案用 sentinel error,契约清晰:**getter 必须用 `ErrKeyNotFound` 明确表达"查无记录",框架据此决定缓存占位符**。

## 复用全局 TTL,零成本带过期

占位符走 `populateCache` → `cache.add` → `strategy.Add`,和正常值完全同路径,自动带上 `expireAt`(绝对过期,与 TTL 扩展一致)。**空值和正常值同 TTL,到期一起重新探 DB**——简单,但代价是空值会挡同样久(若全局 TTL 10 分钟,一个 not-found key 会挡 10 分钟才重新探)。

如果需要空值用更短 TTL(到期快重新探),可给 cache 加 `addWithTTL` 重载,空值走它算更短 expireAt。本方案为简单复用全局 TTL,未做 per-entry 短 TTL。

## 防击穿 + 防穿透协作

not-found 也走 singleflight(在 `load` 里):同 key 并发 not-found 只回源一次,然后写占位符。后续命中占位符直接挡住,不再走 singleflight。**防击穿 + 防穿透协作正常**:

- 并发 not-found:singleflight 去重成一次回源(防击穿)
- 后续相同 not-found:占位符挡住(防穿透)

## 向后兼容

- **现有测试**:`TestGet` 的 "unknown" 断言(`err == nil → Fatalf`)保留通过——命中占位符返回 `ErrKeyNotFound`,err 非 nil,断言仍过
- **HTTP handler**:`main.go` 用 `err != nil` 判 not-found 返回 500,占位符返回 `ErrKeyNotFound` 仍命中这条,行为不变
- **getter 侧**:not-found 从 `fmt.Errorf("%s not exist", key)` 改成 `ErrKeyNotFound`,需 getter 配合(框架提供 sentinel,getter 显式返回)

## 风险

- **占位符冲突(极小概率)**:若业务数据的真实值恰好等于 `_LIGHTCACHE_NULL_`,会被误判成空值标记。可接受——业务值不会正好是这个 sentinel 字符串(Redisson 同款取舍);若担心可换更长的占位符。
- **空值风暴占内存**:占位符 Len=16,大量不存在的 key 会占 `key+16` 字节内存。靠全局 TTL(到期清)+ 容量淘汰(满了删)兜底。**DoS 风险:大量随机 key 能撑满缓存挤掉真实数据**,生产可加空值独立容量上限(本方案未做,留后续)。
- **空值标记 TTL 依赖全局 TTL(已知缺陷)**:占位符复用 `WithTTL` 的全局 TTL。若 Group 没配 TTL(ttl=0),占位符的 `expireAt` 是零值 → **永不过期**,只能等容量淘汰清除,生产上有 DoS/脏数据风险(详见 README 扩展点"per-key TTL")。**根治方案**:做 per-key TTL 扩展(对标 Redis `EXPIRE`),空值占位符用独立的短 TTL,不再依赖全局 TTL 是否开启。当前先用"用空值缓存建议配 TTL"约束。

## 相关文件

- `internal/cache/group.go` —— `ErrKeyNotFound` sentinel、`nullPlaceholder` 常量、`getLocally` 缓存占位符、`Group.Get` 命中占位符返回 `ErrKeyNotFound`
- `cmd/server/main.go` / `internal/cache/group_test.go` —— getter not-found 返回 `ErrKeyNotFound`
- `internal/cache/group_test.go` —— `TestNullCache` 系列(防穿透、真错误不缓存、TTL 过期回源、合法空值不误判)
