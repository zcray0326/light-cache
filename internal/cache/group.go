package cache

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/zcray0326/light-cache/internal/cache/singleflight"
)

// ErrKeyNotFound 是 sentinel error:回源 getter 确认"key 不存在"时返回它。
// getLocally 用 errors.Is 识别它来决定缓存空值占位符(防穿透);
// 真错误(DB 故障等)绝不应返回它,避免把错误结果当 not-found 缓存。
var ErrKeyNotFound = errors.New("key not found")

// nullPlaceholder 是空值标记的固定占位符字符串(对齐 Redisson 的 _REDIS_NULL_PLACEHOLDER_ 思路)。
// not-found 时往缓存塞 ByteView{b: []byte(nullPlaceholder)};
// Group.Get 命中后靠内容判别 string(v.b) == nullPlaceholder 区分占位符与真实值。
// 靠值内容判别,不靠内存对象状态:合法空值(空字符串)内容是 "",不等于占位符,不会误判。
const nullPlaceholder = "_LIGHTCACHE_NULL_"

// Getter 回源接口:缓存未命中时,Group 调用它从数据源(如 DB)取数据。
type Getter interface {
	Get(key string) ([]byte, error)
}

// GetterFunc 是接口型函数,让普通函数也能当 Getter 用(适配器模式)。
// 这样用户可直接传一个 func,不必定义结构体实现接口 —— 与标准库 http.HandlerFunc 同理。
type GetterFunc func(key string) ([]byte, error)

// Get 实现 Getter 接口。
func (f GetterFunc) Get(key string) ([]byte, error) {
	return f(key)
}

// Group 是一个缓存命名空间,持有并发缓存和未命中回源逻辑。
// 外部只与 Group 打交道:命中直接返回,未命中先尝试远程节点(若有),再回退本地回源。
type Group struct {
	name      string              // 命名空间,隔离不同业务的缓存
	getter    Getter              // 未命中时的本地回源
	mainCache *cache              // 本地并发缓存(指针:避免值拷贝分离 mu,导致后台 goroutine 和业务读写用不同锁)
	peers     PeerPicker          // 远程节点选择器(Day5 分布式);为 nil 表示单机模式
	loader    *singleflight.Group // Day6 防击穿:同 key 并发回源只执行一次
	ttl       time.Duration       // 全局 TTL,0=不启用;透传给 mainCache,Add 时算 expireAt
}

var (
	mu     sync.RWMutex
	groups = make(map[string]*Group)
)

// GroupOption 是 NewGroup 的函数式选项,用于可选配置(如 TTL),向后兼容。
type GroupOption func(*Group)

// WithTTL 给 Group 设置全局 TTL:缓存条目写入后 ttl 时刻过期,惰性 + 后台清理。
// 不传此选项则无 TTL(向后兼容,行为同原先)。
func WithTTL(ttl time.Duration) GroupOption {
	return func(g *Group) { g.ttl = ttl }
}

// NewGroup 创建一个 Group 并注册到全局表(可按 name 查找)。
// evictionType 决定底层淘汰策略("lru"/"fifo"/"lfu"),maxBytes 为内存上限(0=不限),
// getter 为未命中回源。opts 为可选配置(如 WithTTL),不传则默认无 TTL。
func NewGroup(name string, maxBytes int64, evictionType string, getter Getter, opts ...GroupOption) *Group {
	if getter == nil {
		panic("nil Getter")
	}
	mu.Lock()
	defer mu.Unlock()
	g := &Group{
		name:   name,
		getter: getter,
		loader: &singleflight.Group{},
	}
	for _, opt := range opts {
		opt(g)
	}
	g.mainCache = newCache(evictionType, maxBytes, g.ttl)
	groups[name] = g
	return g
}

// GetGroup 按名字取已注册的 Group,不存在返回 nil。只读,用读锁。
func GetGroup(name string) *Group {
	mu.RLock()
	g := groups[name]
	mu.RUnlock()
	return g
}

// Get 是 Group 的核心:先查本地缓存,未命中则回源 + 写回。
func (g *Group) Get(key string) (ByteView, error) {
	if key == "" {
		return ByteView{}, fmt.Errorf("key is required")
	}

	// 1. 先查本地缓存
	if v, ok := g.mainCache.get(key); ok {
		// 命中空值占位符:之前确认过该 key 不存在,挡了 DB,但仍对外返回 not-found(保持语义)
		if string(v.b) == nullPlaceholder {
			return ByteView{}, ErrKeyNotFound
		}
		log.Println("[light-cache] hit")
		return v, nil
	}

	// 2. 未命中 → load(先尝试远程节点,失败再回退本地回源)
	return g.load(key)
}

// Stop 关闭 Group 后台清理 goroutine(若有)。生产环境 Group 是长期对象,随进程退出即可;
// 测试用完调用防止 goroutine 泄漏。ttl=0 时无 goroutine,空操作安全。
func (g *Group) Stop() {
	g.mainCache.stop()
}

// RegisterPeers 注册远程节点选择器。注册后 Group 进分布式模式。
// 只允许注册一次(重复注册 panic),避免运行中换节点拓扑导致状态混乱。
func (g *Group) RegisterPeers(peers PeerPicker) {
	if g.peers != nil {
		panic("RegisterPeerPicker called more than once")
	}
	g.peers = peers
}

// load 处理未命中:用 singleflight 保证同一 key 的并发回源(本地或远程)只执行一次,
// 防缓存击穿。fn 内先尝试远程节点(若有),失败回退本地回源。
func (g *Group) load(key string) (value ByteView, err error) {
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		if g.peers != nil {
			if peer, ok := g.peers.PickPeer(key); ok {
				if value, err = g.getFromPeer(peer, key); err == nil {
					return value, nil
				}
				log.Println("[light-cache] Failed to get from peer", err)
			}
		}
		// 远程不可用或单机模式 → 本地回源
		return g.getLocally(key)
	})
	if err == nil {
		// loader.Do 返回 interface{},断言回 ByteView
		return viewi.(ByteView), nil
	}
	return
}

// getFromPeer 从远程节点取数据。
// 注意:远程取回的值不写回本地缓存(原版 GeeCache 设计),避免节点间数据冗余扩散。
func (g *Group) getFromPeer(peer PeerGetter, key string) (ByteView, error) {
	bytes, err := peer.Get(g.name, key)
	if err != nil {
		return ByteView{}, err
	}
	return ByteView{b: bytes}, nil
}

// getLocally 从回源 getter 取数据,包成 ByteView 写回缓存。
// 若 getter 确认 key 不存在(返回 ErrKeyNotFound):缓存一个空值占位符短时挡住后续相同 key 的请求,
// 防缓存穿透;但仍返回 ErrKeyNotFound(对外保持 not-found 语义)。
// 真错误(DB 故障等)不缓存占位符,直接透传 error。
func (g *Group) getLocally(key string) (ByteView, error) {
	bytes, err := g.getter.Get(key)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			// not-found:缓存占位符,短时挡住后续相同 key 的请求,防穿透
			g.populateCache(key, ByteView{b: []byte(nullPlaceholder)})
		}
		return ByteView{}, err
	}
	// 回源拿到的 []byte 必须拷贝:调用方可能继续持有/修改这个 slice,不拷贝会污染缓存。
	value := ByteView{b: cloneBytes(bytes)}
	g.populateCache(key, value)
	return value, nil
}

// populateCache 把回源结果写回缓存。
func (g *Group) populateCache(key string, value ByteView) {
	g.mainCache.add(key, value)
}
