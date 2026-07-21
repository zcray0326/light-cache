// 分布式缓存节点入口:启动 Proxy(接入层)或 Store(存储层)节点。
// 节点列表由 etcd 服务发现动态获取(lease+keepalive 注册 + watch 刷环):
//   - store 模式:注册自己进环,对外 /_lightcache,带 db 回源
//   - proxy 模式:不进环,对外 /api 转发 store,防穿透在 Proxy
//
// 本地运行(docker-compose 一键起 etcd 集群 + 3 store + proxy,见 docker-compose.yml):
//
//	docker-compose up
//	curl "http://localhost:9999/api?key=Tom"   → 630
//
// 手动单机测试(需本地起 etcd):
//
//	go run ./cmd/server -mode=store -port=8001 -etcd=http://127.0.0.1:2379
//	go run ./cmd/server -mode=store -port=8002 -etcd=http://127.0.0.1:2379
//	go run ./cmd/server -mode=proxy -port=9999 -etcd=http://127.0.0.1:2379
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

// createStoreGroup 创建存储层(Store)Group:LRU 策略,未命中从 db 回源。
// Store 带真实 getter(本地回源 DB),防穿透不在 Store 做(透传给 Proxy)。
func createStoreGroup() *cache.Group {
	return cache.NewGroup("scores", 2<<10, "lru", cache.GetterFunc(
		func(key string) ([]byte, error) {
			log.Println("[SlowDB] search key", key)
			if v, ok := db[key]; ok {
				return []byte(v), nil
			}
			return nil, cache.ErrKeyNotFound
		}))
}

// createProxyGroup 创建接入层(Proxy)Group:LRU 策略,WithProxyMode 配置。
// Proxy 不带 db 不回源(allowLocalFallback=false + noopGetter 兜底),防穿透在 Proxy 层。
func createProxyGroup() *cache.Group {
	return cache.NewGroup("scores", 2<<10, "lru", cache.GetterFunc(
		func(key string) ([]byte, error) {
			// Proxy 不回源,这个 getter 实际不会被调(WithProxyMode 会用 noopGetter 覆盖)。
			// 留着仅为 NewGroup 非 nil 约束,WithProxyMode 内部会覆盖成 noopGetter。
			return nil, cache.ErrKeyNotFound
		}), cache.WithProxyMode())
}

// startStoreServer 启动存储层(Store)节点:注册自己到 etcd 进环,从 etcd 发现 peers 建一致性哈希环,
// 开启 HTTP 服务(对外 /_lightcache/<group>/<key>,供 Proxy 远程取)。节点列表全程动态。
// Store 带真实 getter,未命中本地回源 DB;防穿透不在 Store 做(not-found 透传给 Proxy)。
func startStoreServer(selfAddr string, cli *clientv3.Client, group *cache.Group) {
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

	log.Println("light-cache store node is running at", selfAddr)
	// selfAddr 形如 "http://host:port",ListenAndServe 要去掉 "http://" 前缀(7 字符)
	// pool 当 http.Handler,处理 /_lightcache/<group>/<key> 请求(供 Proxy 远程取)
	log.Fatal(http.ListenAndServe(selfAddr[7:], pool))
}

// startProxyServer 启动接入层(Proxy)节点:接收外部 /api 请求,通过 Group.Get 走分布式缓存。
// Proxy **不注册 etcd**(不背数据分片,不进环),只 List+Watch 拿 store 列表建环用于 PickPeer 转发。
// Proxy 不回源(WithProxyMode),远程 store 失败返回 error;防穿透在 Proxy(远程 not-found 时塞占位符)。
func startProxyServer(apiAddr string, cli *clientv3.Client, group *cache.Group) {
	ctx := context.Background()

	// Proxy 不调 Register:不进环,不被一致性哈希路由(它不背数据)。
	// 只 List+Watch 拿 store 列表建环,供 Group.Get → PickPeer 选 store 转发。
	peers, _ := discovery.ListPeers(ctx, cli)
	pool := cache.NewHTTPPool(apiAddr)
	pool.Set(peers...)
	group.RegisterPeers(pool)

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

	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			view, err := group.Get(key)
			if err != nil {
				// ErrKeyNotFound 返回 404(命中空值占位符或 store 确认 not-found),其他 error 返回 500
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
	log.Println("frontend api server is running at", apiAddr)
	log.Fatal(http.ListenAndServe(apiAddr[7:], nil))
}

func main() {
	var port int
	var mode string
	var host string
	var etcdEndpoints string
	flag.IntVar(&port, "port", 8001, "light-cache server port")
	flag.StringVar(&mode, "mode", "store", "node mode: proxy (接入层,转发+防穿透) or store (存储层,回源+存数据)")
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

	if mode == "proxy" {
		// Proxy(接入层):不回源、不注册 etcd。对外 /api 转发到 store,防穿透在 Proxy。
		// Group 配 Proxy 模式(allowLocalFallback=false + noopGetter 兜底)。
		group := createProxyGroup()
		startProxyServer("http://"+host+":9999", cli, group)
		return
	}
	// Store(存储层):注册 etcd 进环,对外 /_lightcache,带 db 回源。
	group := createStoreGroup()
	startStoreServer(selfAddr, cli, group)
}
