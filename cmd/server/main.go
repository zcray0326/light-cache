// light-cache 示例:演示怎么用这个内嵌分布式缓存库(对等节点)。
//
// 每个节点对等:都注册自己到 etcd(进环)、都对外提供 /api + /_lightcache、都带 db 回源。
// 节点列表由 etcd 服务发现动态获取(lease+keepalive 注册 + watch 刷环)。
//
// 运行(docker-compose 一键起 etcd 集群 + 多节点,见 docker-compose.yml):
//
//	docker-compose up
//	curl "http://localhost:8001/api?key=Tom"   → 630
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	cache "github.com/zcray0326/light-cache/internal/cache"
	"github.com/zcray0326/light-cache/internal/discovery"
	"go.etcd.io/etcd/client/v3"
)

var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

// createGroup 创建 "scores" Group:LRU 策略,未命中从 db 回源(not-found 返回 ErrKeyNotFound,空值缓存防穿透)。
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

// startServer 启动对等节点:注册 etcd 进环,建一致性哈希环 + RegisterPeers,
// 对外 /api(用户入口)+ /_lightcache(供其他节点远程取)。未命中本地回源 + 空值缓存防穿透。
func startServer(selfAddr string, cli *clientv3.Client, group *cache.Group) {
	ctx := context.Background()

	// 1. 注册自己到 etcd(进环,被一致性哈希路由)
	go func() {
		if err := discovery.Register(ctx, cli, selfAddr, 10*time.Second); err != nil {
			log.Printf("[discovery] register failed: %v", err)
		}
	}()

	// 2. 拉一次节点列表 → 建环 + 注入 Group
	peers, err := discovery.ListPeers(ctx, cli)
	if err != nil {
		log.Printf("[discovery] list peers failed: %v", err)
	}
	pool := cache.NewHTTPPool(selfAddr)
	pool.Set(peers...)
	group.RegisterPeers(pool)

	// 3. watch 刷环
	update := make(chan struct{}, 1)
	go discovery.WatchPeers(ctx, cli, update)
	go func() {
		for range update {
			if peers, err := discovery.ListPeers(ctx, cli); err == nil {
				pool.Set(peers...)
				log.Printf("[discovery] peers refreshed: %v", peers)
			}
		}
	}()

	// 4. /api 对外入口:用户打这里,Group.Get 未命中按一致性哈希选远程节点取(排除自己)
	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			view, err := group.Get(key)
			if err != nil {
				if errors.Is(err, cache.ErrKeyNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(view.ByteSlice())
		}))

	log.Println("light-cache node is running at", selfAddr)
	// pool 当 handler 处理 /_lightcache/<group>/<key>(供其他节点远程取);/api 走默认 mux
	http.Handle("/_lightcache/", pool)
	log.Fatal(http.ListenAndServe(selfAddr[7:], nil))
}

func main() {
	var port int
	var host, etcdEndpoints string
	flag.IntVar(&port, "port", 8001, "light-cache node port")
	flag.StringVar(&host, "host", "localhost", "advertise host (docker 里用容器名)")
	flag.StringVar(&etcdEndpoints, "etcd", "http://127.0.0.1:2379", "etcd endpoints, comma-separated")
	flag.Parse()

	selfAddr := "http://" + host + ":" + strconv.Itoa(port)

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(etcdEndpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("connect etcd failed: %v", err)
	}
	defer cli.Close()

	startServer(selfAddr, cli, createGroup())
}
