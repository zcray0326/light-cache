// Package discovery 用 etcd 做服务发现:节点启动注册自己,其他节点 watch 拿实时列表。
//
// 设计(对齐 ggcache 的思路,但用裸 KV 而非 endpoints.Manager):
//   - 注册:lease(TTL 10s)+ KeepAlive 续约。节点崩溃后 lease 过期,etcd 自动删 key,其他节点 watch 到。
//   - 发现:ListPeers 全量拉一次(启动初始化环用);WatchPeers 持续 watch,变化发 chan struct{} 信号。
//   - 环刷新:watch 信号触发调用方重新 ListPeers + HTTPPool.Set 全量重建环(全量替换,不做增量)。
//
// key 格式:/light-cache/nodes/<addr>,value 为 <addr>。
// 节点下线靠 lease 过期(先按 ggcache,5s/10s 僵尸窗口后续改优雅退出主动 Delete)。
package discovery

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.etcd.io/etcd/client/v3"
)

// Service 是服务名,作为 etcd key 前缀。
const Service = "light-cache"

// nodesPrefix 是节点 key 的统一前缀:所有节点注册在此前缀下。
const nodesPrefix = "/" + Service + "/nodes/"

// nodeKey 拼出某节点在 etcd 里的 key:/light-cache/nodes/<addr>。
// addr 形如 "http://localhost:8001",直接拼进 key(不转义,内部约定)。
func nodeKey(addr string) string {
	return nodesPrefix + addr
}

// Register 把本节点 addr 注册到 etcd,绑定 lease(TTL=leaseTTL)+ KeepAlive 续约。
// 节点崩溃后 lease 到期,etcd 自动删 key,其他节点 watch 到后刷新环。
// 阻塞直到 ctx 取消或 keepalive 断开。stop 在当前实现里仅作 ctx 语义补充,后续优雅退出可扩展。
func Register(ctx context.Context, cli *clientv3.Client, addr string, leaseTTL time.Duration) error {
	// 1. 申请 lease(TTL 内没续约就过期,节点崩溃后自动摘除)
	lease, err := cli.Grant(ctx, int64(leaseTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("grant lease: %w", err)
	}

	// 2. 用 lease 写自己的注册项:key=/light-cache/nodes/<addr>, value=<addr>
	if _, err = cli.Put(ctx, nodeKey(addr), addr, clientv3.WithLease(lease.ID)); err != nil {
		return fmt.Errorf("put node key: %w", err)
	}

	// 3. KeepAlive:etcd 自动定期续约,只要本进程活着 key 就在。返回的 ch 收到续约响应。
	keepRespCh, err := cli.KeepAlive(ctx, lease.ID)
	if err != nil {
		return fmt.Errorf("keepalive: %w", err)
	}

	log.Printf("[discovery] registered %s with lease TTL %v", addr, leaseTTL)

	// 4. 阻塞监听:ctx 取消(进程退出)或 keepalive 断开(etcd 连不上)时返回。
	// 当前实现不主动 Delete key,靠 lease 过期摘除(后续可改成优雅退出主动 Delete)。
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case resp, ok := <-keepRespCh:
			if !ok {
				// keepalive channel 关闭:lease 被撤销或 etcd 连接断。靠 lease 过期摘除。
				log.Printf("[discovery] keepalive closed for %s, relying on lease TTL to deregister", addr)
				return nil
			}
			// 续约响应到达,继续保活(resp.TTL 是新的 TTL)
			_ = resp
		}
	}
}

// ListPeers 拉一次全量节点列表(启动初始化环用)。返回所有注册在 /light-cache/nodes/ 下的 addr。
func ListPeers(ctx context.Context, cli *clientv3.Client) ([]string, error) {
	resp, err := cli.Get(ctx, nodesPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	peers := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		// value 就是 addr;若有人把别的塞进来,trim 后空就跳过
		addr := strings.TrimSpace(string(kv.Value))
		if addr != "" {
			peers = append(peers, addr)
		}
	}
	return peers, nil
}

// WatchPeers 持续监听 /light-cache/nodes/ 前缀变化,任何 put/delete 都往 update 发信号。
// 信号只表示"有变化了",不传具体增删——调用方收到信号自己重新 ListPeers 拉全量,再刷环。
// 全量重建而非增量:简单可靠,避免增量要处理"同节点先删后加"等边角。
func WatchPeers(ctx context.Context, cli *clientv3.Client, update chan<- struct{}) {
	watchCh := cli.Watch(ctx, nodesPrefix, clientv3.WithPrefix())
	for watchResp := range watchCh {
		for range watchResp.Events {
			// 任何 put/delete 都发信号(非阻塞,update 是带缓冲的 chan)
			select {
			case update <- struct{}{}:
			default:
				// chan 满了说明上一轮重建还没消费完,跳过本次信号(合并多次变化为一次重建)
			}
		}
	}
}
