package cache

// ByteView 是只读的缓存值,内部持有字节切片。
// 对外只暴露只读视图,防止外部修改污染缓存里的数据。
type ByteView struct {
	b []byte // 小写,外部不可直接访问,只能通过方法取值
}

// Len 返回值的字节数。
// 实现 eviction.Value 接口,这样 ByteView 能直接存进 eviction 的 LRU/FIFO 缓存。
func (v ByteView) Len() int {
	return len(v.b)
}

// ByteSlice 返回数据的拷贝。
// 返回拷贝而非内部 b,是为了防止外部修改切片进而污染缓存 —— 这是"只读"的关键。
func (v ByteView) ByteSlice() []byte {
	return cloneBytes(v.b)
}

// String 以字符串形式返回数据。
// string 在 Go 里本就不可变,string(b) 会产生新数据,无需额外拷贝。
func (v ByteView) String() string {
	return string(v.b)
}

// cloneBytes 复制一份字节切片。
func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
