// 分布式缓存节点入口:启动 Proxy(接入层)或 Store(存储层)。
// Group 名单从 config.yml 读启动时批量建(幂等);节点列表由 etcd 服务发现动态获取。
// store:注册进环,对外 /_lightcache,带 db 回源。proxy:不进环,对外 /api 转发 store,防穿透在 Proxy。
//
// 运行:docker-compose up;curl "http://localhost:9999/api?key=Tom" → 630
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

// sharedGetter 是所有 Group 共用的回源 getter(查同一 db,not-found 返回 ErrKeyNotFound)。后续要按 group 路由数据源再改。
var sharedGetter = cache.GetterFunc(func(key string) ([]byte, error) {
	log.Println("[SlowDB] search key", key)
	if v, ok := db[key]; ok {
		return []byte(v), nil
	}
	return nil, cache.ErrKeyNotFound
})

// buildGroups 按 config 批量建 Group(幂等)。store 用 sharedGetter 回源,proxy 用 WithProxyMode 不回源。
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

// startStoreServer 启动 Store:注册 etcd 进环,对每个 Group RegisterPeers,起 HTTP 服务处理 /_lightcache。
func startStoreServer(selfAddr string, cli *clientv3.Client, gm map[string]*cache.Group) {
	ctx := context.Background()
	go func() {
		if err := discovery.Register(ctx, cli, selfAddr, 10*time.Second); err != nil {
			log.Printf("[discovery] register failed: %v", err)
		}
	}()

	peers, err := discovery.ListPeers(ctx, cli)
	if err != nil {
		log.Printf("[discovery] list peers failed: %v", err)
	}
	pool := cache.NewHTTPPool(selfAddr)
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

	log.Println("light-cache store node is running at", selfAddr)
	log.Fatal(http.ListenAndServe(selfAddr[7:], pool))
}

// startProxyServer 启动 Proxy:不注册 etcd,建环 + 每个 Group RegisterPeers,对外 /api?key=&group= 转发。
func startProxyServer(apiAddr string, cli *clientv3.Client, gm map[string]*cache.Group) {
	ctx := context.Background()

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

	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			groupName := r.URL.Query().Get("group")
			if groupName == "" {
				groupName = "scores"
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
	var mode, host, configPath string
	flag.IntVar(&port, "port", 8001, "light-cache server port")
	flag.StringVar(&mode, "mode", "store", "node mode: proxy (接入层) or store (存储层)")
	flag.StringVar(&host, "host", "localhost", "advertise host (docker 里用容器名)")
	flag.StringVar(&configPath, "config", "config.yml", "config file path")
	flag.Parse()

	if err := config.Init(configPath); err != nil {
		log.Fatalf("load config %s failed: %v", configPath, err)
	}

	selfAddr := "http://" + host + ":" + strconv.Itoa(port)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   config.Conf.Etcd.Endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("connect etcd failed: %v", err)
	}
	defer cli.Close()

	if mode == "proxy" {
		startProxyServer("http://"+host+":9999", cli, buildGroups("proxy"))
		return
	}
	startStoreServer(selfAddr, cli, buildGroups("store"))
}
