// 多节点示例:启动缓存节点或前端 API 节点。
//
// 运行(三个终端或用 run.sh):
//
//	go run ./cmd/server -port=8001
//	go run ./cmd/server -port=8002
//	go run ./cmd/server -port=8003 -api
//
// 测试(API 节点 9999 把请求转发到一致性哈希选出的缓存节点):
//
//	curl "http://localhost:9999/api?key=Tom"   → 630
//	curl "http://localhost:9999/api?key=kkk"   → kkk not exist
package main

import (
	"flag"
	"log"
	"net/http"

	cache "github.com/zcray0326/light-cache/internal/cache"
)

var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

// createGroup 创建名为 "scores" 的缓存组:LRU 策略,未命中从 db 回源。
func createGroup() *cache.Group {
	return cache.NewGroup("scores", 2<<10, "lru", cache.GetterFunc(
		func(key string) ([]byte, error) {
			log.Println("[SlowDB] search key", key)
			if v, ok := db[key]; ok {
				return []byte(v), nil
			}
			return nil, cache.ErrKeyNotFound
		}))
}

// startCacheServer 启动一个缓存节点:注册所有节点到一致性哈希环,开启 HTTP 服务。
func startCacheServer(addr string, addrs []string, group *cache.Group) {
	peers := cache.NewHTTPPool(addr)
	peers.Set(addrs...)
	group.RegisterPeers(peers)
	log.Println("light-cache node is running at", addr)
	// addr 形如 "http://localhost:8001",ListenAndServe 要去掉 "http://" 前缀(7 字符)
	log.Fatal(http.ListenAndServe(addr[7:], peers))
}

// startAPIServer 启动前端 API 节点:接收外部请求,通过 Group.Get 走分布式缓存。
// Group.Get 未命中时,由一致性哈希选出的远程节点回源,本 API 节点不回源。
func startAPIServer(apiAddr string, group *cache.Group) {
	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			view, err := group.Get(key)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(view.ByteSlice())
		}))
	log.Println("frontend api server is running at", apiAddr)
	log.Fatal(http.ListenAndServe(apiAddr[7:], nil))
}

func main() {
	var port int
	var api bool
	flag.IntVar(&port, "port", 8001, "light-cache server port")
	flag.BoolVar(&api, "api", false, "start a frontend api server?")
	flag.Parse()

	apiAddr := "http://localhost:9999"
	addrMap := map[int]string{
		8001: "http://localhost:8001",
		8002: "http://localhost:8002",
		8003: "http://localhost:8003",
	}

	var addrs []string
	for _, v := range addrMap {
		addrs = append(addrs, v)
	}

	group := createGroup()
	if api {
		go startAPIServer(apiAddr, group)
	}
	startCacheServer(addrMap[port], addrs, group)
}
