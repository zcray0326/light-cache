package cache

import (
	"errors"
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

// HTTPPool 既是 HTTP 服务端(处理 /_lightcache/<group>/<key>),也是客户端(PickPeer 选节点 + httpGetter 取数据)。
type HTTPPool struct {
	self        string     // 本节点对外地址
	basePath    string     // 统一前缀,默认 /_lightcache/
	mu          sync.Mutex // 保护 peers 和 httpGetters
	peers       *consistenthash.Map
	httpGetters map[string]*httpGetter // 节点地址 → HTTP 客户端
}

// NewHTTPPool 创建 HTTPPool,self 为本节点地址。
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

// ServeHTTP 处理 /<basePath>/<group>/<key> 请求(服务端职责)。
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
		// ErrKeyNotFound → 404(让 Proxy 识别 not-found 塞占位符防穿透),其他 error → 500
		if errors.Is(err, ErrKeyNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(view.ByteSlice())
}

// Set 全量重建一致性哈希环 + 为每个节点建 httpGetter(客户端职责)。
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

// PickPeer 一致性哈希选远程节点(排除自己),返回其 httpGetter。
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peer := p.peers.Get(key); peer != "" && peer != p.self {
		p.Log("Pick peer %s", peer)
		return p.httpGetters[peer], true
	}
	return nil, false
}

var _ PeerPicker = (*HTTPPool)(nil)

// httpGetter 是向单个远程节点取数据的 HTTP 客户端。
type httpGetter struct {
	baseURL string // 如 "http://localhost:8001/_lightcache/"
}

// Get 向 baseURL 拼 /<group>/<key>(转义)HTTP GET 取回字节。404 包成 ErrKeyNotFound 供 Proxy 识别。
func (h *httpGetter) Get(group string, key string) ([]byte, error) {
	u := fmt.Sprintf("%v%v/%v", h.baseURL, url.QueryEscape(group), url.QueryEscape(key))
	res, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusNotFound { // %w 包 ErrKeyNotFound,Proxy 据此塞占位符
			return nil, fmt.Errorf("%w: %v", ErrKeyNotFound, res.Status)
		}
		return nil, fmt.Errorf("server returned: %v", res.Status)
	}

	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %v", err)
	}
	return bytes, nil
}

var _ PeerGetter = (*httpGetter)(nil)
