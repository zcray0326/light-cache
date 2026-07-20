package singleflight

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDo 并发调用同一 key 的 Do,验证 singleflight 去重生效:
// fn 的执行次数应远小于并发数(理想为 1),证明"重复请求被合并"。
//
// fn 里 sleep 模拟真实回源耗时(HTTP/DB 通常毫秒级),这会让第一个 call 的窗口足够长,
// 让后续重复请求挤进来 wg.Wait,从而被合并。若 fn 瞬时完成,窗口太短,可能合并不到。
func TestDo(t *testing.T) {
	var count int32
	g := &Group{}

	const n = 100
	results := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, err := g.Do("key", func() (interface{}, error) {
				atomic.AddInt32(&count, 1)
				time.Sleep(50 * time.Millisecond) // 模拟回源 IO,撑大窗口让重复请求能挤进来等
				return "value", nil
			})
			if err != nil {
				t.Errorf("Do returned error: %v", err)
			}
			results[i] = v.(string)
		}(i)
	}
	close(start)
	wg.Wait()

	got := atomic.LoadInt32(&count)
	if got >= n {
		t.Fatalf("fn executed %d times (>= %d concurrent), singleflight 无效", got, n)
	}
	t.Logf("fn executed %d/%d times (去重生效,理想为1)", got, n)
	for i, r := range results {
		if r != "value" {
			t.Fatalf("results[%d] = %q, want %q", i, r, "value")
		}
	}
}

// TestDoDifferentKey 不同 key 的并发请求应各自独立执行(各一次),互不等待。
func TestDoDifferentKey(t *testing.T) {
	var count int32
	g := &Group{}

	var wg sync.WaitGroup
	const n = 10
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i)) // a,b,c,... 每个不同
			_, _ = g.Do(key, func() (interface{}, error) {
				atomic.AddInt32(&count, 1)
				return key, nil
			})
		}(i)
	}
	wg.Wait()

	// 不同 key,应执行 n 次
	if got := atomic.LoadInt32(&count); got != n {
		t.Fatalf("fn executed %d times, want %d (不同 key 应各自执行)", got, n)
	}
}

// TestDoErr 验证 fn 返回 error 时,所有等待者拿到相同 error。
func TestDoErr(t *testing.T) {
	g := &Group{}
	var wg sync.WaitGroup
	errs := make([]error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := g.Do("errkey", func() (interface{}, error) {
				return nil, errFailed
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != errFailed {
			t.Fatalf("errs[%d] = %v, want errFailed", i, e)
		}
	}
}

var errFailed = newError("failed")

func newError(msg string) error { return &myError{msg: msg} }

type myError struct{ msg string }

func (e *myError) Error() string { return e.msg }
