package eviction

import "fmt"

// Value 接口:值要能报告自己占多少字节,用于内存计量。
type Value interface {
	Len() int
}

// entry 是链表节点里保存的键值对,LRU/FIFO 共用。
// 节点里仍存 key,是为了淘汰时能用 key 从 map 里删除对应映射。
type entry struct {
	key   string
	value Value
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
type CacheStrategy interface {
	Get(key string) (value Value, ok bool)
	Add(key string, value Value)
	Len() int
}

// New 是工厂:按 name 造一个对应策略的缓存。新增策略只要在这里加一个 case。
func New(name string, maxBytes int64, onEvicted func(string, Value)) (CacheStrategy, error) {
	t, err := StringToEvictionType(name)
	if err != nil {
		return nil, err
	}
	switch t {
	case EvictionLRU:
		return NewLRU(maxBytes, onEvicted), nil
	case EvictionFIFO:
		return NewFIFO(maxBytes, onEvicted), nil
	case EvictionLFU:
		return NewLFU(maxBytes, onEvicted), nil
	default:
		return nil, fmt.Errorf("unsupported eviction type: %q", name)
	}
}
