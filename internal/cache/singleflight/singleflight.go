// Package singleflight 防止缓存击穿:对同一个 key 的并发请求,只让第一个真正执行,
// 其余请求等待第一个完成后共享结果。避免高并发下大量请求同时穿透到回源/DB。
//
// 名字来自标准库 golang.org/x/sync/singleflight,实现思路一致:
// 用 WaitGroup 让"重复请求"阻塞等待,用 map 记录正在进行中的调用。
package singleflight

import "sync"

// call 代表一次正在进行(或已完成)的 Do 调用。
type call struct {
	wg  sync.WaitGroup // 让后来的重复请求等待第一个完成
	val interface{}    // fn 的返回值
	err error          // fn 的返回错误
}

// Group 是 singleflight 的核心:一个命名空间,内部维护"哪些 key 正在执行"。
type Group struct {
	mu sync.Mutex       // 保护 m 的并发读写
	m  map[string]*call // 懒初始化:key → 正在进行的 call
}

// Do 执行 fn 并返回结果。保证:同一 key 在同一时刻只有一个 fn 在跑,
// 并发到来的重复调用会等待第一个完成,拿到相同结果。
//
// 这是缓存击穿防御的关键:把"未命中后的回源/远程取"包进 fn, 即使 1000 个并发请求同一 key,也只回源一次,其余 999 个等结果。
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call) // 懒初始化,避免空 Group 占内存
	}
	// 已经有同 key 的 call 正在进行 → 这是重复请求,等待它
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait() // 阻塞,直到第一个完成 wg.Done()
		return c.val, c.err
	}
	// 没有 → 我是第一个,创建 call 并登记
	c := new(call)
	c.wg.Add(1) // 标记"有人在等"(防止 wg.Wait 在 Done 前返回)
	g.m[key] = c
	g.mu.Unlock()

	// 真正执行 fn(放锁外,执行期间不持锁,让别的 key 能并行)
	c.val, c.err = fn()
	c.wg.Done() // 唤醒所有等待的重复请求

	// 执行完,从 map 删除,让后续同 key 请求能重新触发(否则永久命中旧值)
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
