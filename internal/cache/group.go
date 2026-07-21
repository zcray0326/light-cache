package cache

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/zcray0326/light-cache/internal/cache/singleflight"
)

// ErrKeyNotFound 是 not-found sentinel:getter 确认 key 不存在时返回,
// getLocally/load 用 errors.Is 识别(防穿透用)。真错误(DB 故障)不应返回它。
var ErrKeyNotFound = errors.New("key not found")

// nullPlaceholder 是空值占位符(对齐 Redisson)。not-found 时塞 ByteView{b: []byte(它)},
// 命中后靠内容判别 string(v.b) == 它 区分占位符与真实值(合法空值内容是 "" 不误判)。
const nullPlaceholder = "_LIGHTCACHE_NULL_"

// Getter 是回源接口:未命中时 Group 调它从数据源取数据。
type Getter interface {
	Get(key string) ([]byte, error)
}

// GetterFunc 是接口型函数,让普通函数当 Getter 用(适配器,同 http.HandlerFunc)。
type GetterFunc func(key string) ([]byte, error)

// Get 实现 Getter 接口。
func (f GetterFunc) Get(key string) ([]byte, error) {
	return f(key)
}

// Group 是缓存命名空间,持有本地缓存和未命中回源逻辑(对等节点:既接请求又回源,不分会角色)。
// 命中直接返回;未命中先尝试远程节点(若有),失败回退本地回源 + 写回缓存。
type Group struct {
	name      string              // 命名空间
	getter    Getter              // 未命中回源
	mainCache *cache              // 本地并发缓存(指针,避免值拷贝分离锁)
	peers     PeerPicker          // 远程节点选择器;nil 表示单机
	loader    *singleflight.Group // 防击穿:同 key 并发回源只执行一次
	ttl       time.Duration       // 全局 TTL,0=不启用
}

var (
	mu     sync.RWMutex
	groups = make(map[string]*Group)
)

// GroupOption 是 NewGroup 的函数式选项。
type GroupOption func(*Group)

// WithTTL 设置全局 TTL(惰性 + 后台清理)。
func WithTTL(ttl time.Duration) GroupOption {
	return func(g *Group) { g.ttl = ttl }
}

// NewGroup 创建 Group 并注册到全局表。幂等:同名已存在返回旧的(双重检查锁,不覆盖不泄漏)。
func NewGroup(name string, maxBytes int64, evictionType string, getter Getter, opts ...GroupOption) *Group {
	if getter == nil {
		panic("nil Getter")
	}
	mu.RLock()
	if g, ok := groups[name]; ok {
		mu.RUnlock()
		return g
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	if g, ok := groups[name]; ok { // double-check 防并发重复建
		return g
	}
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

// GetGroup 按名字取已注册的 Group,不存在返回 nil。
func GetGroup(name string) *Group {
	mu.RLock()
	g := groups[name]
	mu.RUnlock()
	return g
}

// Get 查 key:命中(含空值占位符)直接返回,未命中走 load。
func (g *Group) Get(key string) (ByteView, error) {
	if key == "" {
		return ByteView{}, fmt.Errorf("key is required")
	}
	if v, ok := g.mainCache.get(key); ok {
		if string(v.b) == nullPlaceholder { // 空值占位符:挡了 DB,仍返回 not-found
			return ByteView{}, ErrKeyNotFound
		}
		log.Println("[light-cache] hit")
		return v, nil
	}
	return g.load(key)
}

// Stop 关闭后台清理 goroutine。ttl=0 时空操作安全。测试用完调防泄漏。
func (g *Group) Stop() {
	g.mainCache.stop()
}

// RegisterPeers 注册远程节点选择器,注册后进分布式模式。只许一次(重复 panic)。
func (g *Group) RegisterPeers(peers PeerPicker) {
	if g.peers != nil {
		panic("RegisterPeerPicker called more than once")
	}
	g.peers = peers
}

// load 处理未命中:singleflight 防击穿,先尝试远程节点,失败回退本地回源(对等,不分会角色)。
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
		return g.getLocally(key) // 远程不可用或单机 → 本地回源(空值缓存防穿透在这)
	})
	if err == nil {
		return viewi.(ByteView), nil
	}
	return
}

// getFromPeer 从远程节点取数据(取回不写回本地,原版 GeeCache 设计)。
func (g *Group) getFromPeer(peer PeerGetter, key string) (ByteView, error) {
	bytes, err := peer.Get(g.name, key)
	if err != nil {
		return ByteView{}, err
	}
	return ByteView{b: bytes}, nil
}

// getLocally 回源取数据并写回缓存。not-found(ErrKeyNotFound)时塞空值占位符防穿透(分散:每个节点都做)。
func (g *Group) getLocally(key string) (ByteView, error) {
	bytes, err := g.getter.Get(key)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) { // not-found:塞占位符,短时挡住后续同 key,防穿透
			g.populateCache(key, ByteView{b: []byte(nullPlaceholder)})
		}
		return ByteView{}, err
	}
	value := ByteView{b: cloneBytes(bytes)} // 拷贝,防调用方修改 slice 污染缓存
	g.populateCache(key, value)
	return value, nil
}

// populateCache 把回源结果写回缓存。
func (g *Group) populateCache(key string, value ByteView) {
	g.mainCache.add(key, value)
}
