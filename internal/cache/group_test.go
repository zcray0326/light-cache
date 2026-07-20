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
			return nil, fmt.Errorf("%s not exist", key)
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

	// 回源不存在的 key:getter 返回 error,Group.Get 应透传该 error
	if view, err := gee.Get("unknown"); err == nil {
		t.Fatalf("the value of unknow should be empty, but %s got", view)
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
			return nil, fmt.Errorf("%s not exist", key)
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
			return nil, fmt.Errorf("%s not exist", key)
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
