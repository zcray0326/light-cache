// 分布式缓存节点入口:启动 Proxy(接入层)或 Store(存储层)节点。
// Group 名单从 config.yml 读,启动时按角色批量建(幂等)。
// 节点列表由 etcd 服务发现动态获取(lease+keepalive 注册 + watch 刷环):
//   - store 模式:注册自己进环,对外 /_lightcache,带 db 回源
//   - proxy 模式:不进环,对外 /api 转发 store,防穿透在 Proxy
//
// 本地运行(docker-compose 一键起 etcd 集群 + 3 store + proxy,见 docker-compose.yml):
//
//	docker-compose up
//	curl "http://localhost:9999/api?key=Tom"   → 630
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"strconv"
	"time"

	cache "github.com/zcray0326/light-cache/internal/cache"
	"github.com/zcray0326/light-cache/internal/config"
	"github.com/zcray0326/light-cache/internal/discovery"
	"go.etcd.io/etcd/client/v3"
)

var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

// sharedGetter 是所有 Group 共用的回源 getter(查同一个 db,not-found 返回 ErrKeyNotFound)。
// 跟 ggcache 一样(所有 group 共用一个 retriever),先简单;后续要按 group 路由不同数据源再改。
var sharedGetter = cache.GetterFunc(func(key string) ([]byte, error) {
	log.Println("[SlowDB] search key", key)
	if v, ok := db[key]; ok {
		return []byte(v), nil
	}
	return nil, cache.ErrKeyNotFound
})

// buildGroups 按 config 批量建 Group(幂等:已存在的返回旧的)。
// role="store" 用 sharedGetter(本地回源);role="proxy" 用 WithProxyMode(不回源,防穿透在 Proxy)。
// TTL(若配置)通过 WithTTL 选项传入(NewGroup 构造时应用)。
// 返回 GroupManager(map[name]*Group),供 startServer 对每个 Group RegisterPeers。
func buildGroups(role string) map[string]*cache.Group {
	gm := make(map[string]*cache.Group)
	for _, c := range config.Conf.Groups {
		opts := []cache.GroupOption{}
		if ttl := c.TTLDuration(); ttl > 0 {
			opts = append(opts, cache.WithTTL(ttl))
		}
		var g *cache.Group
		if role == "proxy" {
			opts = append(opts, cache.WithProxyMode())
			g = cache.NewGroup(c.Name, c.MaxBytes, c.EvictionType,
				cache.GetterFunc(func(string) ([]byte, error) { return nil, cache.ErrKeyNotFound }),
				opts...)
		} else {
			g = cache.NewGroup(c.Name, c.MaxBytes, c.EvictionType, sharedGetter, opts...)
		}
		gm[c.Name] = g
	}
	return gm
}

// startStoreServer 启动存储层(Store)节点:注册自己到 etcd 进环,建一致性哈希环,
// 对每个 Group 调 RegisterPeers(避免只给部分 Group 注册),开启 HTTP 服务。
func startStoreServer(selfAddr string, cli *clientv3.Client, gm map[string]*cache.Group) {
	ctx := context.Background()

	// 1. 注册自己到 etcd
	go func() {
		if err := discovery.Register(ctx, cli, selfAddr, 10*time.Second); err != nil {
			log.Printf("[discovery] register failed: %v", err)
		}
	}()

	// 2. 拉一次节点列表 → 建环 + 对每个 Group RegisterPeers
	peers, err := discovery.ListPeers(ctx, cli)
	if err != nil {
		log.Printf("[discovery] list peers failed: %v", err)
	}
	pool := cache.NewHTTPPool(selfAddr)
	pool.Set(peers...)
	for _, g := range gm {
		g.RegisterPeers(pool) // 每个 Group 都注册(幂等性靠 RegisterPeers 的"只许一次"语义,首次注册)
	}

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

	log.Println("light-cache store node is running at", selfAddr)
	// pool 当 handler,处理 /_lightcache/<group>/<key>(ServeHTTP 按 group name 走 GetGroup)
	log.Fatal(http.ListenAndServe(selfAddr[7:], pool))
}

// startProxyServer 启动接入层(Proxy)节点:不注册 etcd,建环 + 对每个 Group RegisterPeers,
// 对外 /api?key=&group=(解析 group 参数选对应 Group)。
func startProxyServer(apiAddr string, cli *clientv3.Client, gm map[string]*cache.Group) {
	ctx := context.Background()

	// Proxy 不 Register,只 List+Watch 建环
	peers, _ := discovery.ListPeers(ctx, cli)
	pool := cache.NewHTTPPool(apiAddr)
	pool.Set(peers...)
	for _, g := range gm {
		g.RegisterPeers(pool)
	}

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

	// /api?key=Tom&group=scores:解析 group 参数选对应 Group(默认 scores)
	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			groupName := r.URL.Query().Get("group")
			if groupName == "" {
				groupName = "scores" // 默认 group
			}
			group := cache.GetGroup(groupName)
			if group == nil {
				http.Error(w, "no such group: "+groupName, http.StatusNotFound)
				return
			}
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
	log.Println("frontend api server is running at", apiAddr)
	log.Fatal(http.ListenAndServe(apiAddr[7:], nil))
}

func main() {
	var port int
	var mode string
	var host string
	var configPath string
	flag.IntVar(&port, "port", 8001, "light-cache server port")
	flag.StringVar(&mode, "mode", "store", "node mode: proxy (接入层) or store (存储层)")
	flag.StringVar(&host, "host", "localhost", "advertise host (docker 里用容器名)")
	flag.StringVar(&configPath, "config", "config.yml", "config file path")
	flag.Parse()

	// 读 config.yml(Group 名单 + etcd endpoints)
	if err := config.Init(configPath); err != nil {
		log.Fatalf("load config %s failed: %v", configPath, err)
	}

	selfAddr := "http://" + host + ":" + strconv.Itoa(port)

	// etcd client(endpoints 从 config 读)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.Conf.Etcd.Endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("connect etcd failed: %v", err)
	}
	defer cli.Close()

	if mode == "proxy" {
		gm := buildGroups("proxy")
		startProxyServer("http://"+host+":9999", cli, gm)
		return
	}
	gm := buildGroups("store")
	startStoreServer(selfAddr, cli, gm)
}
