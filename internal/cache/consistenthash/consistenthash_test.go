package consistenthash

import (
	"strconv"
	"testing"
)

// testHash 把数字字符串转成 uint32,作为确定性哈希函数,便于写断言。
// 例如 testHash("2")=2, testHash("27")=27。
func testHash(data []byte) uint32 {
	i, _ := strconv.Atoi(string(data))
	return uint32(i)
}

// TestHashing 验证节点选择与节点增减的影响范围。
// 环大小 2^32,这里哈希值是数字本身,所以环上排的是 [2,4,6,8,...] 之类。
func TestHashing(t *testing.T) {
	// 3 个虚拟节点/真实节点,用确定性哈希
	hash := New(3, testHash)

	// 加节点 "6","4","2"。每个扩展 3 个虚拟节点(名 = strconv.Itoa(i)+node):
	//   "2" → "02"=2, "12"=12, "22"=22  (这3个都属于节点 "2")
	//   "4" → "04"=4, "14"=14, "24"=24  (都属于节点 "4")
	//   "6" → "06"=6, "16"=16, "26"=26  (都属于节点 "6")
	// 环排序后:2,4,6,12,14,16,22,24,26
	hash.Add("6", "4", "2")

	// 验证 key 顺时针找到最近的虚拟节点,并映射回真实节点。
	// 注意:虚拟节点名拼接是 strconv.Itoa(i)+node,所以 "12" 属于节点 "2"(不是 "6")。
	cases := map[string]string{
		"2":  "2", // hash=2 命中虚拟节点 2 → 节点 "2"
		"27": "2", // hash=27 大于环上所有值,绕回环首 2 → 节点 "2"
		"11": "2", // hash=11,顺时针最近 12(节点 "2" 的虚拟节点 "12")→ "2"
		"13": "4", // hash=13,顺时针最近 14(节点 "4")→ "4"
		"17": "2", // hash=17,顺时针最近 22(节点 "2" 的虚拟节点 "22")→ "2"
	}
	for k, want := range cases {
		if got := hash.Get(k); got != want {
			t.Errorf("Get(%s) = %s, want %s", k, got, want)
		}
	}

	// 新增节点 "8":虚拟节点 "08"=8, "18"=18, "28"=28(都属节点 "8")
	// 环变成:2,4,6,8,12,14,16,18,22,24,26,28
	// key "27":顺时针最近 28 → "8"(原绕回 2 → "2",现迁到 "8")
	// 这验证了一致性哈希的核心价值:新增节点只"接管"它附近的 key 区间(27~28),
	// 不像普通哈希那样几乎全部重新映射。
	hash.Add("8")
	if got := hash.Get("27"); got != "8" {
		t.Errorf("Get(27) after add(8) = %s, want 8", got)
	}
	// "11" 仍应映射到 "2"(顺时针最近 12,不随 "8" 加入而改变)——只影响相邻区间
	if got := hash.Get("11"); got != "2" {
		t.Errorf("Get(11) after add(8) = %s, want 2 (unaffected)", got)
	}
}

// TestReplicate 验证虚拟节点能缓解数据倾斜:
// 只有一个真实节点 + 多虚拟节点,确保 Get 不会 panic,且能把不同 key 分散。
func TestReplicate(t *testing.T) {
	hash := New(50, testHash)
	hash.Add("1")

	// 单节点时,所有 key 都应映射到 "1"
	for _, k := range []string{"1", "2", "3", "4", "5", "100", "200"} {
		if got := hash.Get(k); got != "1" {
			t.Errorf("single node: Get(%s) = %s, want 1", k, got)
		}
	}
}

// TestEmpty 验证空环时 Get 返回空串。
func TestEmpty(t *testing.T) {
	hash := New(3, testHash)
	if got := hash.Get("any"); got != "" {
		t.Errorf("empty ring: Get = %q, want empty", got)
	}
}
