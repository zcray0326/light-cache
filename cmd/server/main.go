// 分布式缓存节点入口:启动缓存节点或前端 API 节点。
// 节点列表不再硬编码,改由 etcd 服务发现动态获取:
//   - 启动即注册自己到 etcd(lease+keepalive)
//   - 启动时 ListPeers 拉一次初始化一致性哈希环
//   - 后台 watch etcd,节点变化时重新 List + HTTPPool.Set 全量刷环
//
// 本地运行(docker-compose 一键起 etcd 集群 + 3 缓存节点 + API 节点,见 docker-compose.yml):
//
//	docker-compose up
//	curl "http://localhost:9999/api?key=Tom"   → 630
//
// 手动单机测试(需本地起 etcd):
//
//	go run ./cmd/server -port=8001 -etcd=http://127.0.0.1:2379
//	go run ./cmd/server -port=8002 -etcd=http://127.0.0.1:2379
//	go run ./cmd/server -port=8003 -etcd=http://127.0.0.1:2379 -api
package main

import (
	"context"
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

// startCacheServer 启动一个缓存节点:从 etcd 发现 peers 建一致性哈希环,开启 HTTP 服务。
// 节点列表全程动态:启动时 ListPeers 初始化,后台 watch 刷环。RegisterPeers 只注入一次。
func startCacheServer(selfAddr string, cli *clientv3.Client, group *cache.Group) {
	ctx := context.Background()

	// 1. 注册自己到 etcd(lease 10s + keepalive 续约,goroutine 阻塞跑)
	go func() {
		if err := discovery.Register(ctx, cli, selfAddr, 10*time.Second); err != nil {
			log.Printf("[discovery] register failed: %v", err)
		}
	}()

	// 2. 拉一次当前节点列表 → 初始化环 + 注入 Group
	peers, err := discovery.ListPeers(ctx, cli)
	if err != nil {
		log.Printf("[discovery] list peers failed: %v", err)
	}
	pool := cache.NewHTTPPool(selfAddr)
	pool.Set(peers...) // 全量建环(可能为空,watch 后会补)
	group.RegisterPeers(pool)

	// 3. watch etcd:节点变化信号 → 重新 List 全量 → pool.Set 刷环(原子替换)
	update := make(chan struct{}, 1)
	go discovery.WatchPeers(ctx, cli, update)
	go func() {
		for range update {
			if peers, err := discovery.ListPeers(ctx, cli); err == nil {
				pool.Set(peers...) // HTTPPool.Set 内部 mu 保护,并发安全全量替换
				log.Printf("[discovery] peers refreshed: %v", peers)
			}
		}
	}()

	log.Println("light-cache node is running at", selfAddr)
	// selfAddr 形如 "http://host:port",ListenAndServe 要去掉 "http://" 前缀(7 字符)
	log.Fatal(http.ListenAndServe(selfAddr[7:], pool))
}

// startAPIServer 启动前端 API 节点:接收外部请求,通过 Group.Get 走分布式缓存。
// API 节点也需要注册 + watch,因为 Group.Get 未命中时按一致性哈希选远程节点,环必须最新。
func startAPIServer(apiAddr string, cli *clientv3.Client, group *cache.Group) {
	// API 节点也注册自己(可选,但保持拓扑一致):它在环里也能被路由
	go func() {
		if err := discovery.Register(context.Background(), cli, apiAddr, 10*time.Second); err != nil {
			log.Printf("[discovery] api register failed: %v", err)
		}
	}()

	// API 节点也建环 + watch,保证 PickPeer 选对远程节点
	peers, _ := discovery.ListPeers(context.Background(), cli)
	pool := cache.NewHTTPPool(apiAddr)
	pool.Set(peers...)
	group.RegisterPeers(pool)

	update := make(chan struct{}, 1)
	go discovery.WatchPeers(context.Background(), cli, update)
	go func() {
		for range update {
			if peers, err := discovery.ListPeers(context.Background(), cli); err == nil {
				pool.Set(peers...)
			}
		}
	}()

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
	var host string
	var etcdEndpoints string
	flag.IntVar(&port, "port", 8001, "light-cache server port")
	flag.BoolVar(&api, "api", false, "start a frontend api server?")
	flag.StringVar(&host, "host", "localhost", "advertise host for this node (docker 里用容器名,保证其他节点能回连)")
	flag.StringVar(&etcdEndpoints, "etcd", "http://127.0.0.1:2379", "etcd endpoints, comma-separated (3 节点集群可填多个)")
	flag.Parse()

	// 本节点对外地址:其他节点靠这个地址回连。docker 里 host=容器名,localhost=本地。
	selfAddr := "http://" + host + ":" + strconv.Itoa(port)

	// etcd client(配多个 endpoint 自动故障转移)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(etcdEndpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("connect etcd failed: %v", err)
	}
	defer cli.Close()

	group := createGroup()
	if api {
		// API 节点:只起前端 API server(转发到缓存节点),不起 cache server。
		// 避免和缓存节点分开部署时重复 RegisterPeers panic。
		startAPIServer("http://"+host+":9999", cli, group)
		return
	}
	startCacheServer(selfAddr, cli, group)
}
