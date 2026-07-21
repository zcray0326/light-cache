// Package consistenthash 实现一致性哈希(虚拟节点 + 二分查找)。
// 解决节点增减时缓存大面积失效:节点变化只影响相邻区间,非整体重哈希。
package consistenthash

import (
	"hash/crc32"
	"sort"
	"strconv"
	"sync"
)

// Map 是一致性哈希环。非并发安全(并发由上层封装)。
type Map struct {
	mu       sync.RWMutex
	hash     Hash           // 哈希函数,默认 crc32
	replicas int            // 每个真实节点的虚拟节点数
	keys     []int          // 已排序的虚拟节点哈希环,二分查找
	hashMap  map[int]string // 虚拟节点哈希 → 真实节点
}

// Hash 是哈希函数类型。
type Hash func(data []byte) uint32

// New 创建哈希环。replicas=虚拟节点数,fn=nil 用 crc32.ChecksumIEEE。
func New(replicas int, fn Hash) *Map {
	m := &Map{
		replicas: replicas,
		hash:     crc32.ChecksumIEEE,
		hashMap:  make(map[int]string),
	}
	if fn != nil {
		m.hash = fn
	}
	return m
}

// Add 添加节点,每个扩展为 replicas 个虚拟节点。虚拟节点名 = 编号(i 在前)+ 节点名。
func (m *Map) Add(nodes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, node := range nodes {
		for i := 0; i < m.replicas; i++ {
			hash := int(m.hash([]byte(strconv.Itoa(i) + node)))
			m.keys = append(m.keys, hash)
			m.hashMap[hash] = node
		}
	}
	sort.Ints(m.keys)
}

// Get 返回承载 key 的真实节点:算 key 哈希,环上顺时针找第一个 >= 它的虚拟节点,映射回真实节点。
func (m *Map) Get(key string) string {
	if m.IsEmpty() {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	hash := int(m.hash([]byte(key)))
	idx := sort.Search(len(m.keys), func(i int) bool {
		return m.keys[i] >= hash
	})
	if idx == len(m.keys) { // 比所有都大,绕回环首
		idx = 0
	}
	return m.hashMap[m.keys[idx]]
}

// IsEmpty 判断环是否无节点。
func (m *Map) IsEmpty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys) == 0
}
