// Package cache LRU 翻译缓存与同文本并发合并。
//
// V1 来源：
//   - LRUCache              translator.py:228~247（容量 1000）
//   - _in_flight / 结果表    translator.py:612~644（_call_api 的请求合并）
//
// 不变量：
//   - LRU 容量 1000，命中后移到队尾，超限淘汰最旧
//   - 同文本合并只对**真正并发**的请求生效；后到的请求不会命中历史结果
//     （V1 的 _in_flight_results 只在等待事件期间被读取）
//   - 等待超时（V1 为 10 秒）后不再等待，自己翻译——绝不因为合并而卡住
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// ── LRU ──────────────────────────────────────────────────────

// LRU 是容量受限的最近最少使用缓存。对应 V1 的 LRUCache。
type LRU struct {
	mu    sync.Mutex
	max   int
	ll    *list.List               // 队尾 = 最近使用，队首 = 最久未用
	items map[string]*list.Element
}

type lruEntry struct{ key, value string }

// NewLRU 创建容量为 max 的缓存（V1 用 1000）。
func NewLRU(max int) *LRU {
	if max <= 0 {
		max = 1
	}
	return &LRU{max: max, ll: list.New(), items: make(map[string]*list.Element, max)}
}

// Get 取缓存并把该键移到队尾。未命中返回 ("", false)。
func (c *LRU) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.ll.MoveToBack(el)
	return el.Value.(*lruEntry).value, true
}

// Put 写入并维护容量上限（超限时淘汰队首）。
func (c *LRU) Put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruEntry).value = value
		c.ll.MoveToBack(el)
	} else {
		c.items[key] = c.ll.PushBack(&lruEntry{key, value})
	}
	for c.ll.Len() > c.max {
		oldest := c.ll.Front()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry).key)
	}
}

// Len 返回当前条目数。
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Clear 清空缓存。
func (c *LRU) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[string]*list.Element, c.max)
}

// ── 同文本并发合并 ────────────────────────────────────────────

// InFlight 把「相同原文的并发请求」合并成一次真实调用。
//
// 对应 V1 的 _in_flight（等待事件）+ _in_flight_results（结果传递）。
type InFlight struct {
	mu         sync.Mutex
	pending    map[string]chan struct{}
	results    map[string]string
	maxResults int
}

// NewInFlight 创建合并器。maxResults 对应 V1 的「超过 300 条整体清空」。
func NewInFlight(maxResults int) *InFlight {
	if maxResults <= 0 {
		maxResults = 300
	}
	return &InFlight{
		pending:    map[string]chan struct{}{},
		results:    map[string]string{},
		maxResults: maxResults,
	}
}

// Do 执行 fn，若已有相同 key 的请求在飞则等待其结果。
//
// 返回 shared=true 表示复用了别人刚完成的结果，本次**没有**调用 fn。
// wait 超时或对方失败时，会自己调用 fn（V1 行为：绝不因合并而卡住）。
func (f *InFlight) Do(
	ctx context.Context,
	key string,
	wait time.Duration,
	fn func(context.Context) (string, error),
) (string, bool, error) {
	f.mu.Lock()
	existing, hasExisting := f.pending[key]
	if !hasExisting {
		f.pending[key] = make(chan struct{})
	}
	f.mu.Unlock()

	if hasExisting {
		select {
		case <-existing:
			f.mu.Lock()
			res, ok := f.results[key]
			f.mu.Unlock()
			if ok {
				return res, true, nil
			}
			// 对方没留下结果（失败或被清空）→ 自己翻译
		case <-time.After(wait):
			// 等待超时 → 自己翻译
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
		return f.callAndStore(ctx, key, fn, false)
	}

	return f.callAndStore(ctx, key, fn, true)
}

func (f *InFlight) callAndStore(
	ctx context.Context, key string,
	fn func(context.Context) (string, error), owner bool,
) (string, bool, error) {
	if owner {
		defer f.release(key) // 与 V1 的 finally 一致：无论成败都唤醒等待者
	}

	res, err := fn(ctx)
	if err != nil {
		return "", false, err
	}

	f.mu.Lock()
	if len(f.results) >= f.maxResults {
		f.results = map[string]string{} // V1：超过 300 条整体清空
	}
	f.results[key] = res
	f.mu.Unlock()

	return res, false, nil
}

func (f *InFlight) release(key string) {
	f.mu.Lock()
	ch, ok := f.pending[key]
	delete(f.pending, key)
	f.mu.Unlock()
	if ok {
		close(ch) // 唤醒所有等待者（它们随后去读 results）
	}
}

// Pending 返回当前在飞请求数，供测试与诊断使用。
func (f *InFlight) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}
