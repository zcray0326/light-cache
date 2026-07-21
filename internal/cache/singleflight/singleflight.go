// Package singleflight 防缓存击穿:同 key 并发请求只让第一个执行,其余等结果共享。
// 名字来自 golang.org/x/sync/singleflight,思路一致:WaitGroup 让重复请求等待,map 记录进行中的调用。
package singleflight

import "sync"

// call 代表一次进行中(或已完成)的 Do 调用。
type call struct {
	wg  sync.WaitGroup // 让后来的重复请求等待第一个完成
	val interface{}
	err error
}

// Group 是 singleflight 命名空间,维护"哪些 key 正在执行"。
type Group struct {
	mu sync.Mutex
	m  map[string]*call // 懒初始化:key → 进行中的 call
}

// Do 执行 fn:同 key 同时刻只有一个 fn 在跑,并发重复调用等待第一个共享结果。
// 防击穿关键:把未命中后的回源/远程取包进 fn,1000 并发只回源一次。
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok { // 重复请求:等待第一个
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn() // 执行(放锁外,不阻塞其他 key)
	c.wg.Done()         // 唤醒等待者

	g.mu.Lock()
	delete(g.m, key) // 执行完删除,让后续同 key 能重新触发
	g.mu.Unlock()

	return c.val, c.err
}
