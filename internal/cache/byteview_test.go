package cache

import "testing"

// TestByteView_Len 验证 Len 返回字节数。
func TestByteView_Len(t *testing.T) {
	v := ByteView{b: []byte("1234")}
	if v.Len() != 4 {
		t.Fatalf("ByteView.Len() = %d, want 4", v.Len())
	}
}

// TestByteView_ByteSliceImmutable 验证 ByteSlice 返回的是拷贝:
// 外部修改返回的切片,不应影响 ByteView 内部的数据 —— 这是"只读"的保证。
func TestByteView_ByteSliceImmutable(t *testing.T) {
	v := ByteView{b: []byte("abcd")}
	out := v.ByteSlice()
	out[0] = 'X' // 外部改拷贝

	// 内部数据不应被改
	if string(v.b) != "abcd" {
		t.Fatalf("internal data mutated: got %q, want %q", v.b, "abcd")
	}
	// 拷贝确实被改了(证明是两份数据)
	if string(out) != "Xbcd" {
		t.Fatalf("copy not changed: got %q", out)
	}
}

// TestByteView_String 验证 String 返回字符串,且不暴露内部切片。
func TestByteView_String(t *testing.T) {
	v := ByteView{b: []byte("hello")}
	if v.String() != "hello" {
		t.Fatalf("String() = %q, want %q", v.String(), "hello")
	}
}
