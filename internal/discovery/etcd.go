// Package discovery 用 etcd 做服务发现(裸 KV + lease)。
// 注册:lease+KeepAlive,崩溃后 lease 过期自动摘除。发现:ListPeers 全量拉 + WatchPeers 发信号触发调用方全量重建环。
// key 格式:/light-cache/nodes/<addr>,value 为 <addr>。
package discovery

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.etcd.io/etcd/client/v3"
)

const Service = "light-cache"                 // 服务名,etcd key 前缀
const nodesPrefix = "/" + Service + "/nodes/" // 节点 key 统一前缀

// nodeKey 拼出节点在 etcd 的 key:/light-cache/nodes/<addr>。
func nodeKey(addr string) string {
	return nodesPrefix + addr
}

// Register 把本节点注册到 etcd(lease+KeepAlive)。崩溃后 lease 过期自动删 key。阻塞到 ctx 取消或 keepalive 断。
func Register(ctx context.Context, cli *clientv3.Client, addr string, leaseTTL time.Duration) error {
	lease, err := cli.Grant(ctx, int64(leaseTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("grant lease: %w", err)
	}
	if _, err = cli.Put(ctx, nodeKey(addr), addr, clientv3.WithLease(lease.ID)); err != nil {
		return fmt.Errorf("put node key: %w", err)
	}
	keepRespCh, err := cli.KeepAlive(ctx, lease.ID)
	if err != nil {
		return fmt.Errorf("keepalive: %w", err)
	}
	log.Printf("[discovery] registered %s with lease TTL %v", addr, leaseTTL)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-keepRespCh:
			if !ok { // keepalive 断,靠 lease 过期摘除
				log.Printf("[discovery] keepalive closed for %s, relying on lease TTL", addr)
				return nil
			}
		}
	}
}

// ListPeers 拉一次全量节点列表(启动初始化环用)。
func ListPeers(ctx context.Context, cli *clientv3.Client) ([]string, error) {
	resp, err := cli.Get(ctx, nodesPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	peers := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		if addr := strings.TrimSpace(string(kv.Value)); addr != "" {
			peers = append(peers, addr)
		}
	}
	return peers, nil
}

// WatchPeers 持续监听前缀变化,任何 put/delete 发信号到 update。不传具体增删,调用方收到信号重新 ListPeers 全量刷环。
func WatchPeers(ctx context.Context, cli *clientv3.Client, update chan<- struct{}) {
	watchCh := cli.Watch(ctx, nodesPrefix, clientv3.WithPrefix())
	for watchResp := range watchCh {
		for range watchResp.Events {
			select {
			case update <- struct{}{}:
			default: // chan 满,合并多次变化为一次重建
			}
		}
	}
}
