package cache

import (
	"fmt"
	"log"
	"reflect"
	"testing"
	"time"
)

var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

// TestGetter 验证 GetterFunc 能当 Getter 接口用(接口型函数适配器)。
func TestGetter(t *testing.T) {
	var f Getter = GetterFunc(func(key string) ([]byte, error) {
		return []byte(key), nil
	})

	expect := []byte("key")
	if v, _ := f.Get("key"); !reflect.DeepEqual(v, expect) {
		t.Errorf("callback failed")
	}
}

// TestGet 验证 Group 的核心流程:第一次 Get 走回源,第二次 Get 命中不走回源。
// 顺带验证回源不存在的 key 返回 error。
// 用 LRU 策略测试(走 Group 的 mainCache.get → cache.strategy.Get)。
func TestGet(t *testing.T) {
	loadCounts := make(map[string]int, len(db))
	gee := NewGroup("scores", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			log.Println("[SlowDB] search key", key)
			if v, ok := db[key]; ok {
				if _, ok := loadCounts[key]; !ok {
					loadCounts[key] = 0
				}
				loadCounts[key] += 1
				return []byte(v), nil
			}
			return nil, ErrKeyNotFound
		}))

	for k, v := range db {
		// 第一次:缓存未命中,走回源;loadCounts 计数 +1
		if view, err := gee.Get(k); err != nil || view.String() != v {
			t.Fatal("failed to get value of Tom")
		}
		// 第二次:应命中缓存,不再回源(loadCounts 不应再增加)
		if _, err := gee.Get(k); err != nil || loadCounts[k] > 1 {
			t.Fatalf("cache %s miss", k)
		}
	}

	// 回源不存在的 key:getter 返回 ErrKeyNotFound,Group.Get 应透传该 error
	if view, err := gee.Get("unknown"); err == nil {
		t.Fatalf("the value of unknow should be empty, but %s got", view)
	}
	// 第二次 Get 同一不存在的 key:应命中空值占位符(防穿透),不再回源,但仍返回 ErrKeyNotFound
	if _, err := gee.Get("unknown"); err == nil {
		t.Fatalf("second get of unknown should still be not-found (placeholder cached)")
	}
}

// TestGet_FIFO 用 FIFO 策略再跑一遍同样的流程,验证多策略在 Group 层都能工作。
// 这是相对 GeeCache 原版的扩展点:同一套 Group 代码,换 evictionType 即可换底层策略。
func TestGet_FIFO(t *testing.T) {
	loadCounts := make(map[string]int, len(db))
	gee := NewGroup("scores-fifo", 2<<10, "fifo", GetterFunc(
		func(key string) ([]byte, error) {
			if v, ok := db[key]; ok {
				loadCounts[key]++
				return []byte(v), nil
			}
			return nil, ErrKeyNotFound
		}))

	for k, v := range db {
		if view, err := gee.Get(k); err != nil || view.String() != v {
			t.Fatalf("fifo: first get %s failed", k)
		}
		if _, err := gee.Get(k); err != nil || loadCounts[k] > 1 {
			t.Fatalf("fifo: cache %s miss", k)
		}
	}
}

// TestGet_WithTTL 验证 Group 级 TTL:命中 → 过期 → 重新回源。
// 这是 ggcache 没覆盖的关键场景:过期后再次 Get 应回源(loadCounts 再增),而非一直返回旧值。
//
// 时序(短 TTL):
//   - 第一次 Get:未命中,回源 → loadCounts[Tom]=1,缓存写入(带 expireAt)
//   - 第二次 Get:命中缓存,不回源(loadCounts 不增)
//   - 等 TTL 过期
//   - 第三次 Get:缓存过期,重新回源 → loadCounts[Tom]=2(关键:回源再次发生)
func TestGet_WithTTL(t *testing.T) {
	loadCounts := make(map[string]int)
	const ttl = 50 * time.Millisecond

	gee := NewGroup("scores-ttl", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			if v, ok := db[key]; ok {
				loadCounts[key]++
				return []byte(v), nil
			}
			return nil, ErrKeyNotFound
		}), WithTTL(ttl))
	defer gee.Stop() // 停后台清理 goroutine,防泄漏

	// 第一次:未命中回源
	if view, err := gee.Get("Tom"); err != nil || view.String() != "630" {
		t.Fatalf("first get Tom failed: %v", err)
	}
	if loadCounts["Tom"] != 1 {
		t.Fatalf("first get should trigger 1 load, got %d", loadCounts["Tom"])
	}

	// 第二次:命中,不回源
	if _, err := gee.Get("Tom"); err != nil {
		t.Fatalf("second get Tom failed: %v", err)
	}
	if loadCounts["Tom"] != 1 {
		t.Fatalf("second get should hit cache (no load), got %d", loadCounts["Tom"])
	}

	// 等 TTL 过期
	time.Sleep(ttl + 30*time.Millisecond)

	// 第三次:过期,重新回源 —— 关键断言,ggcache 没测的点
	if view, err := gee.Get("Tom"); err != nil || view.String() != "630" {
		t.Fatalf("third get (after TTL) should reload, failed: %v", err)
	}
	if loadCounts["Tom"] != 2 {
		t.Fatalf("after TTL, get should reload (loadCounts=2), got %d", loadCounts["Tom"])
	}
}

// TestGet_WithTTL_LazyExpire 验证过期 entry 不会返回脏数据:
// TTL 过期但后台 goroutine 还没扫到时,Get 惰性检查应识别过期 → 回源拿新值。
func TestGet_WithTTL_LazyExpire(t *testing.T) {
	loadCounts := make(map[string]int)
	const ttl = 50 * time.Millisecond

	gee := NewGroup("scores-lazy", 2<<10, "lfu", GetterFunc(
		func(key string) ([]byte, error) {
			loadCounts[key]++
			return []byte(db[key]), nil
		}), WithTTL(ttl))
	defer gee.Stop()

	gee.Get("Sam")                        // 回源一次
	time.Sleep(ttl + 30*time.Millisecond) // 过期,但可能后台还没扫到

	// 惰性检查:Get 应发现过期,回源拿新值,而非返回过期值
	if _, err := gee.Get("Sam"); err != nil {
		t.Fatalf("lazy expire get failed: %v", err)
	}
	if loadCounts["Sam"] != 2 {
		t.Fatalf("lazy expire should reload, got %d", loadCounts["Sam"])
	}
}

// TestNullCache 验证缓存穿透防御(空值缓存),Proxy 模式:
// store 返回 not-found 时,Proxy 塞占位符,后续同 key 命中占位符挡住 store(不再调 store),仍返回 ErrKeyNotFound。
func TestNullCache(t *testing.T) {
	// mock store:"nope" 不存在(返回 ErrKeyNotFound),"Tom" 存在(返回 630)
	store := &mockPeerGetter{getByKey: map[string][]byte{"Tom": []byte("630")}}
	gee := NewGroup("scores-null", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			return nil, ErrKeyNotFound // Proxy 不回源,这个 getter 实际不被调(WithProxyMode 覆盖成 noopGetter)
		}), WithProxyMode())
	defer gee.Stop()
	gee.RegisterPeers(&mockPeerPicker{getter: store})

	// ① 首次 Get 不存在的 key:Proxy 转发 store,store 返回 not-found,Proxy 塞占位符
	if _, err := gee.Get("nope"); err == nil {
		t.Fatalf("first get nope should return ErrKeyNotFound")
	}
	if store.callCount != 1 {
		t.Fatalf("first get should call store once, got %d", store.callCount)
	}

	// ② 第二次 Get 同 key:命中占位符,不再调 store(防穿透关键),仍返回 ErrKeyNotFound
	if _, err := gee.Get("nope"); err == nil {
		t.Fatalf("second get nope should still be not-found (placeholder)")
	}
	if store.callCount != 1 {
		t.Fatalf("second get should hit placeholder, not call store, got %d", store.callCount)
	}

	// ③ 连续多次 Get,store 调用次数始终为 1(占位符持续挡住)
	for i := 0; i < 5; i++ {
		gee.Get("nope")
	}
	if store.callCount != 1 {
		t.Fatalf("placeholder should block all subsequent store calls, got %d", store.callCount)
	}
}

// TestNullCache_RealErrorNotCached 验证真错误(DB 故障等)不缓存占位符:
// getter 返回非 ErrKeyNotFound 的 error 时,每次都回源(不被占位符挡)。
func TestNullCache_RealErrorNotCached(t *testing.T) {
	loadCounts := make(map[string]int)
	gee := NewGroup("scores-err", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			loadCounts[key]++
			return nil, fmt.Errorf("db connection refused") // 非 sentinel 的真错误
		}))
	defer gee.Stop()

	gee.Get("k1")
	gee.Get("k1") // 真错误不缓存占位符,第二次仍回源
	if loadCounts["k1"] != 2 {
		t.Fatalf("real error should not be cached, should load twice, got %d", loadCounts["k1"])
	}
}

// TestNullCache_TTLExpiresAndReloads 验证占位符也有 TTL:Proxy 模式下,过期后重新调 store(而非永久挡住)。
func TestNullCache_TTLExpiresAndReloads(t *testing.T) {
	const ttl = 50 * time.Millisecond
	store := &mockPeerGetter{getByKey: map[string][]byte{}} // "ghost" 不存在
	gee := NewGroup("scores-null-ttl", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			return nil, ErrKeyNotFound
		}), WithProxyMode(), WithTTL(ttl))
	defer gee.Stop()
	gee.RegisterPeers(&mockPeerPicker{getter: store})

	gee.Get("ghost") // 首次调 store,返回 not-found,Proxy 塞占位符
	if store.callCount != 1 {
		t.Fatalf("first get should call store once, got %d", store.callCount)
	}

	gee.Get("ghost") // 命中占位符,不调 store
	if store.callCount != 1 {
		t.Fatalf("placeholder hit should not call store, got %d", store.callCount)
	}

	time.Sleep(ttl + 30*time.Millisecond) // 占位符过期

	gee.Get("ghost") // 过期,重新调 store(重新塞占位符)
	if store.callCount != 2 {
		t.Fatalf("after TTL, should call store again (placeholder expired), got %d", store.callCount)
	}
}

// TestNullCache_LegitEmptyValueNotMisjudged 验证合法空值不被误判为占位符:
// getter 返回 []byte("")(空字符串)的 key,命中后正常返回空字符串,不当 not-found。
// 这是占位符方案的核心保证:靠值内容区分,合法空值内容是 "",不等于 "_LIGHTCACHE_NULL_"。
func TestNullCache_LegitEmptyValueNotMisjudged(t *testing.T) {
	loadCounts := make(map[string]int)
	gee := NewGroup("scores-empty", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			loadCounts[key]++
			return []byte(""), nil // 合法空值:nil error,空 slice
		}))
	defer gee.Stop()

	// 首次 Get:回源,返回空字符串 + nil error(不是 ErrKeyNotFound)
	view, err := gee.Get("emptykey")
	if err != nil {
		t.Fatalf("legit empty value should not be ErrKeyNotFound, got %v", err)
	}
	if view.String() != "" {
		t.Fatalf("legit empty value should be empty string, got %q", view.String())
	}
	if loadCounts["emptykey"] != 1 {
		t.Fatalf("first get should load once, got %d", loadCounts["emptykey"])
	}

	// 第二次 Get:命中真实空值(非占位符),不回源,仍返回空字符串 + nil error
	view2, err := gee.Get("emptykey")
	if err != nil {
		t.Fatalf("second get legit empty should still be nil error, got %v", err)
	}
	if view2.String() != "" {
		t.Fatalf("second get should return cached empty string, got %q", view2.String())
	}
	if loadCounts["emptykey"] != 1 {
		t.Fatalf("second get should hit cache, no load, got %d", loadCounts["emptykey"])
	}
}

// ---- 测试 mock:模拟 Proxy 转发到 store ----

// mockPeerGetter 模拟一个远程 store 节点的 PeerGetter。
// getByKey: key → 该 store 返回什么(值 []byte 或 ErrKeyNotFound)。store 调用计数计入 callCount。
type mockPeerGetter struct {
	getByKey  map[string][]byte // key → store 返回的值(若存在)
	callCount int               // store 被调用次数(测 Proxy 防穿透:第二次应不再调 store)
}

func (m *mockPeerGetter) Get(group string, key string) ([]byte, error) {
	m.callCount++
	if v, ok := m.getByKey[key]; ok {
		return v, nil
	}
	return nil, ErrKeyNotFound // store 确认 not-found
}

// mockPeerPicker 总是返回同一个 mockPeerGetter(单 store 拓扑,测 Proxy 逻辑用)。
type mockPeerPicker struct {
	getter *mockPeerGetter
}

func (p *mockPeerPicker) PickPeer(key string) (PeerGetter, bool) {
	return p.getter, true
}

// 编译期断言:mock 实现了接口。
var _ PeerPicker = (*mockPeerPicker)(nil)
var _ PeerGetter = (*mockPeerGetter)(nil)

// TestProxyMode_NoLocalFallback 验证 Proxy 模式:远程 store 失败直接返回 error,不本地回源。
// Proxy 没带真实 getter(WithProxyMode 用 noopGetter),即使远程失败也不该走 getLocally。
func TestProxyMode_NoLocalFallback(t *testing.T) {
	store := &mockPeerGetter{getByKey: map[string][]byte{}} // 任何 key 都返回 not-found
	gee := NewGroup("scores-proxy-nofallback", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			t.Fatalf("Proxy should not call local getter (no local fallback)")
			return nil, nil
		}), WithProxyMode())
	defer gee.Stop()
	gee.RegisterPeers(&mockPeerPicker{getter: store})

	// store 返回 not-found → Proxy 塞占位符 + 返回 ErrKeyNotFound(不调 local getter)
	if _, err := gee.Get("missing"); err == nil {
		t.Fatalf("Proxy should return error when store returns not-found")
	}
}

// TestStoreMode_NoNullCache 验证 Store 模式:not-found 透传,不缓存空值占位符。
// Store 纯存真实数据,防穿透在 Proxy 层。Store 的 getter 返回 ErrKeyNotFound 时不塞占位符。
func TestStoreMode_NoNullCache(t *testing.T) {
	loadCounts := make(map[string]int)
	gee := NewGroup("scores-store-nocache", 2<<10, "lru", GetterFunc(
		func(key string) ([]byte, error) {
			loadCounts[key]++
			return nil, ErrKeyNotFound // Store 模式默认(not-found)
		})) // 不传 WithProxyMode → Store 模式,allowLocalFallback=true
	defer gee.Stop()
	// Store 无 peers(单机),load 直接走 getLocally

	gee.Get("absent")
	gee.Get("absent") // Store 不缓存占位符,第二次仍回源
	if loadCounts["absent"] != 2 {
		t.Fatalf("Store should not cache null placeholder, should load twice, got %d", loadCounts["absent"])
	}
}

// TestNewGroup_Idempotent 验证 NewGroup 幂等:同名重复建返回同一实例,不覆盖(不丢缓存、不泄漏 goroutine)。
// 这是对齐 ggcache 的双重检查锁模式,修正 light-cache 之前"groups[name]=g 静默覆盖"的缺陷。
func TestNewGroup_Idempotent(t *testing.T) {
	name := "scores-idempotent"
	getter := GetterFunc(func(string) ([]byte, error) { return []byte("v"), nil })
	g1 := NewGroup(name, 2<<10, "lru", getter)
	defer g1.Stop()
	g2 := NewGroup(name, 2<<10, "lru", getter)

	// 同名重复建:返回的是同一实例(指针相等),不是新覆盖
	if g1 != g2 {
		t.Fatalf("NewGroup should be idempotent: same name returns same instance, got different pointers")
	}

	// g1 回源写回的缓存,g2(重复建得到的同一实例)Get 应命中,证明缓存没丢
	view, err := g2.Get("k1") // 回源 getter 返回 "v",写回缓存
	if err != nil || view.String() != "v" {
		t.Fatalf("idempotent NewGroup: g2.Get should hit g1's populated cache, got %v %v", view, err)
	}
}

// TestGetGroup_NotFound 验证 GetGroup 不存在的 group 返回 nil(不 panic),由调用方判 nil。
func TestGetGroup_NotFound(t *testing.T) {
	if g := GetGroup("no-such-group"); g != nil {
		t.Fatalf("GetGroup for non-existent group should return nil, got %v", g)
	}
}
