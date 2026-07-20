package cache

// PeerPicker 负责为给定 key 选出承载它的远程节点。
// 实现者(HTTPPool)用一致性哈希决定 key 该去哪个节点,返回该节点的 PeerGetter。
type PeerPicker interface {
	PickPeer(key string) (peer PeerGetter, ok bool)
}

// PeerGetter 负责从某个远程节点取回指定 group 的 key 对应值。
// 实现者(httpGetter)用 HTTP 向目标节点发起请求,拿到字节。
type PeerGetter interface {
	Get(group string, key string) ([]byte, error)
}
