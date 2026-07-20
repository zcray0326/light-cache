package eviction

import (
	"fmt"
	"time"
)

// Value 接口:值要能报告自己占多少字节,用于内存计量。
type Value interface {
	Len() int
}

// entry 是链表节点里保存的键值对,LRU/FIFO 共用。
// 节点里仍存 key,是为了淘汰时能用 key 从 map 里删除对应映射。
// expireAt 是绝对过期时刻(写入时定死,Get 不刷新),零值表示永不过期(TTL 未启用)。
type entry struct {
	key      string
	value    Value
	expireAt time.Time
}

// expired 判断该 entry 是否已过期。expireAt 为零值(永不过期)时返回 false。
// 绝对过期语义:entry 自包含过期时刻,无需外部传 ttl —— 与 Redis 的 lazy expiration 对齐。
func (e *entry) expired() bool {
	if e.expireAt.IsZero() {
		return false
	}
	return time.Now().After(e.expireAt)
}

// EvictionType 用 iota 枚举所有淘汰策略。已实现 LRU/FIFO/LFU,后续可加 ARC。
type EvictionType int

const (
	EvictionLRU  EvictionType = iota // 最近最少使用
	EvictionFIFO                     // 先进先出
	EvictionLFU                      // 最不经常使用
)

// String 让枚举可读,打印/日志时方便。加策略时这里也要加 case。
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

// StringToEvictionType 反查:从配置字符串(如 "lru")拿到枚举。
// 工厂 New() 用它。加策略时这里和上面 String() 两处都要同步加。
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

// CacheStrategy 是所有淘汰策略必须实现的接口 —— 策略模式的核心。
// 所有方法都非并发安全,由上层 cache 用 Mutex 保护。
// TTL 相关:CleanUp() 遍历删过期 entry(无参,entry 自带 expireAt 自判);
// 后台 goroutine 和 Stop 由上层 cache 层负责(共享 cache 的 Mutex,避免 race)。
type CacheStrategy interface {
	Get(key string) (value Value, ok bool)
	Add(key string, value Value)
	Len() int
	CleanUp()
}

// New 是工厂:按 name 造一个对应策略的缓存。新增策略只要在这里加一个 case。
// ttl 为全局过期时长:>0 时启用 TTL(Add 时算 expireAt = now+ttl,后台 goroutine 定期 CleanUp);
// 为 0 时关闭 TTL,行为完全同无 TTL 版本(向后兼容)。
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
