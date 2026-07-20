// Package consistenthash 实现一致性哈希,用于分布式缓存 Day4。
//
// 一致性哈希解决"节点增减时缓存大面积失效"的问题:把 key 和节点都映射到
// 一个 0~2^32-1 的哈希环上,key 顺时针找最近节点。节点变化只影响相邻区间,
// 而非整体重哈希。引入虚拟节点解决"节点少时数据倾斜"。
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
	hash     Hash           // 哈希函数,可替换,默认 crc32
	replicas int            // 每个真实节点的虚拟节点数
	keys     []int          // 已排序的哈希环(虚拟节点哈希值),用二分查找
	hashMap  map[int]string // 虚拟节点哈希值 → 真实节点名的映射map
}

// Hash 函数类型:输入字节,返回 uint32 哈希值。
type Hash func(data []byte) uint32

// New 创建一个一致性哈希 Map。
// replicas:虚拟节点数(越大分布越均匀,内存开销也越大)。
// fn:自定义哈希函数,传 nil 用默认 crc32.ChecksumIEEE。
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

// Add 向环上添加若干真实节点,每个会扩展为 replicas 个虚拟节点。
// 虚拟节点名 = strconv.Itoa(i) + 节点名(i 在前),i 从 0 到 replicas-1。
func (m *Map) Add(nodes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, node := range nodes {
		for i := 0; i < m.replicas; i++ {
			// 虚拟节点哈希 = hash(编号 + 节点名),如节点 "2" 的第 3 个虚拟节点 = hash("2"+"2")=hash("22")
			// 注意:i 在前,所以 "12" 属于节点 "2"(i=1),不是节点 "6"。
			hash := int(m.hash([]byte(strconv.Itoa(i) + node)))
			m.keys = append(m.keys, hash)
			m.hashMap[hash] = node // 虚拟节点 → 真实节点
		}
	}
	// 哈希环排序,Get 时才能二分查找顺时针最近节点
	sort.Ints(m.keys)
}

// Get 根据 key 返回应该承载它的真实节点。
// 算法:算 key 的哈希值,在环上顺时针找第一个 >= 该哈希的虚拟节点,再映射回真实节点。
func (m *Map) Get(key string) string {
	if m.IsEmpty() {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	hash := int(m.hash([]byte(key)))

	// 二分查找:环上第一个 >= hash 的虚拟节点
	// sort.Search 返回满足 f 为 true 的最小下标;都 false 时返回 len(环),即"绕回"到环首
	idx := sort.Search(len(m.keys), func(i int) bool {
		return m.keys[i] >= hash
	})

	// 若 hash 比所有虚拟节点都大,idx == len,顺时针绕回环首(取模)
	if idx == len(m.keys) {
		idx = 0
	}
	return m.hashMap[m.keys[idx]]
}

// IsEmpty 判断环上是否没有节点。
func (m *Map) IsEmpty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys) == 0
}
