package cache

// ByteView 是只读缓存值,对外只暴露只读视图防外部修改污染缓存。
type ByteView struct {
	b []byte // 小写,外部只能通过方法取值
}

// Len 返回字节数。实现 eviction.Value 接口,可直接存进 LRU/FIFO。
func (v ByteView) Len() int {
	return len(v.b)
}

// ByteSlice 返回数据的拷贝(防外部修改污染缓存,只读的关键)。
func (v ByteView) ByteSlice() []byte {
	return cloneBytes(v.b)
}

// String 以字符串返回(string 本就不可变,无需额外拷贝)。
func (v ByteView) String() string {
	return string(v.b)
}

// cloneBytes 复制字节切片。
func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
