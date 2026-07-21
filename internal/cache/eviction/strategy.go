package eviction

import (
	"fmt"
	"time"
)

// Value 是缓存值接口,要能报告占多少字节(内存计量用)。
type Value interface {
	Len() int
}

// entry 是 LRU/FIFO 链表节点里的键值对。存 key 是为淘汰时能从 map 删映射。
type entry struct {
	key      string
	value    Value
	expireAt time.Time // 绝对过期时刻,零值=永不过期
}

// expired 判断是否过期(零值永不过期)。entry 自带 expireAt,无需外部传 ttl(对齐 Redis lazy)。
func (e *entry) expired() bool {
	if e.expireAt.IsZero() {
		return false
	}
	return time.Now().After(e.expireAt)
}

// EvictionType 枚举淘汰策略(LRU/FIFO/LFU,后续可加 ARC)。
type EvictionType int

const (
	EvictionLRU EvictionType = iota
	EvictionFIFO
	EvictionLFU
)

// String 让枚举可读。加策略要同步加 case。
func (e EvictionType) String() string {
	switch e {
	case EvictionLRU:
		return "lru"
	case EvictionFIFO:
		return "fifo"
	case EvictionLFU:
		return "lfu"
	default:
		return "unknown"
	}
}

// StringToEvictionType 从配置字符串反查枚举。加策略要和 String() 同步加。
func StringToEvictionType(s string) (EvictionType, error) {
	switch s {
	case "lru":
		return EvictionLRU, nil
	case "fifo":
		return EvictionFIFO, nil
	case "lfu":
		return EvictionLFU, nil
	default:
		return EvictionLRU, fmt.Errorf("invalid eviction type: %q", s)
	}
}

// CacheStrategy 是淘汰策略接口(策略模式核心)。非并发安全,由上层 cache 用 Mutex 保护。
// TTL 的 CleanUp 遍历删过期(无参,entry 自带 expireAt);后台 goroutine 和 Stop 在上层 cache。
type CacheStrategy interface {
	Get(key string) (value Value, ok bool)
	Add(key string, value Value)
	Len() int
	CleanUp()
}

// New 是工厂,按 name 造策略。ttl>0 启用 TTL(Add 算 expireAt);为 0 关闭(向后兼容)。加策略加 case。
func New(name string, maxBytes int64, ttl time.Duration, onEvicted func(string, Value)) (CacheStrategy, error) {
	t, err := StringToEvictionType(name)
	if err != nil {
		return nil, err
	}
	switch t {
	case EvictionLRU:
		return NewLRU(maxBytes, ttl, onEvicted), nil
	case EvictionFIFO:
		return NewFIFO(maxBytes, ttl, onEvicted), nil
	case EvictionLFU:
		return NewLFU(maxBytes, ttl, onEvicted), nil
	default:
		return nil, fmt.Errorf("unsupported eviction type: %q", name)
	}
}
