package cache

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/zcray0326/light-cache/internal/cache/consistenthash"
)

const (
	defaultBasePath = "/_lightcache/"
	defaultReplicas = 50 // 一致性哈希虚拟节点数,越大分布越均匀
)

// HTTPPool 既是 HTTP 服务端(响应其他节点请求),也是客户端(向其他节点取数据)。
// 服务端:实现 http.Handler,处理 /_lightcache/<group>/<key> 请求。
// 客户端:实现 PeerPicker(Set 注册节点 + PickPeer 选节点),httpGetter 实现 PeerGetter。
type HTTPPool struct {
	// self 是本节点对外地址,如 "localhost:9999"
	self string
	// basePath 是统一前缀,默认 /_lightcache/
	basePath string
	mu       sync.Mutex // 保护 peers 和 httpGetters 的并发读写
	peers    *consistenthash.Map
	// httpGetters 按节点地址缓存对应的 HTTP 客户端,如 "http://localhost:8001" → *httpGetter
	httpGetters map[string]*httpGetter
}

// NewHTTPPool 创建一个 HTTPPool,self 为本节点地址。
func NewHTTPPool(self string) *HTTPPool {
	return &HTTPPool{
		self:     self,
		basePath: defaultBasePath,
	}
}

// Log 带节点名打印日志。
func (p *HTTPPool) Log(format string, v ...interface{}) {
	log.Printf("[Server %s] %s", p.self, fmt.Sprintf(format, v...))
}

// ServeHTTP 处理所有 HTTP 请求,实现 http.Handler 接口(服务端职责)。
//
// 约定 URL 格式:/<basePath>/<groupname>/<key>
func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, p.basePath) {
		panic("HTTPPool serving unexpected path: " + r.URL.Path)
	}
	p.Log("%s %s", r.Method, r.URL.Path)

	parts := strings.SplitN(r.URL.Path[len(p.basePath):], "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	groupName := parts[0]
	key := parts[1]

	group := GetGroup(groupName)
	if group == nil {
		http.Error(w, "no such group: "+groupName, http.StatusNotFound)
		return
	}

	view, err := group.Get(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(view.ByteSlice())
}

// Set 更新节点列表,重建一致性哈希环并为每个节点建一个 httpGetter(客户端职责)。
// 每次调用都重新建环,便于后续支持动态增减节点。
func (p *HTTPPool) Set(peers ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers = consistenthash.New(defaultReplicas, nil)
	p.peers.Add(peers...)
	p.httpGetters = make(map[string]*httpGetter, len(peers))
	for _, peer := range peers {
		p.httpGetters[peer] = &httpGetter{baseURL: peer + p.basePath}
	}
}

// PickPeer 按 key 用一致性哈希选一个远程节点(排除自己),返回其 httpGetter。
// 实现 PeerPicker 接口(客户端职责)。
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peer := p.peers.Get(key); peer != "" && peer != p.self {
		p.Log("Pick peer %s", peer)
		return p.httpGetters[peer], true
	}
	return nil, false
}

// 编译期断言:*HTTPPool 实现了 PeerPicker 接口。
var _ PeerPicker = (*HTTPPool)(nil)

// httpGetter 是向单个远程节点取数据的 HTTP 客户端,实现 PeerGetter。
type httpGetter struct {
	baseURL string // 如 "http://localhost:8001/_lightcache/"
}

// Get 向 baseURL 拼出 /<group>/<key> 的请求 URL,HTTP GET 取回字节。
// group/key 用 url.QueryEscape 转义,避免含特殊字符破坏 URL。
func (h *httpGetter) Get(group string, key string) ([]byte, error) {
	u := fmt.Sprintf("%v%v/%v", h.baseURL, url.QueryEscape(group), url.QueryEscape(key))
	res, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned: %v", res.Status)
	}

	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %v", err)
	}
	return bytes, nil
}

// 编译期断言:*httpGetter 实现了 PeerGetter 接口。
var _ PeerGetter = (*httpGetter)(nil)
