package cache

import (
	"fmt"
	"log"
	"reflect"
	"testing"
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
