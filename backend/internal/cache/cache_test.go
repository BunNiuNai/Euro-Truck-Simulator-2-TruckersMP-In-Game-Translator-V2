package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── LRU ──

func TestLRUHitAndMiss(t *testing.T) {
	c := NewLRU(3)
	if _, ok := c.Get("nope"); ok {
		t.Error("空缓存不应命中")
	}
	c.Put("a", "1")
	got, ok := c.Get("a")
	if !ok || got != "1" {
		t.Errorf("Get(a) = (%q, %v)，期望 (1, true)", got, ok)
	}
}

func TestLRUEvictsOldest(t *testing.T) {
	c := NewLRU(3)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3")
	c.Put("d", "4") // 触发淘汰，最旧的 a 应被逐出

	if _, ok := c.Get("a"); ok {
		t.Error("a 应已被淘汰")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s 应仍在缓存中", k)
		}
	}
	if c.Len() != 3 {
		t.Errorf("Len() = %d，期望 3", c.Len())
	}
}

// 命中后该键应被视为「最近使用」，从而不被下次淘汰。
func TestLRUGetRefreshesRecency(t *testing.T) {
	c := NewLRU(3)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3")
	c.Get("a")      // a 变成最近使用
	c.Put("d", "4") // 应淘汰最久未用的 b（而非 a）

	if _, ok := c.Get("b"); ok {
		t.Error("b 应已被淘汰（因为 a 被 Get 刷新过）")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("a 不应被淘汰（刚被 Get 刷新）")
	}
}

func TestLRUUpdateExistingKey(t *testing.T) {
	c := NewLRU(2)
	c.Put("a", "1")
	c.Put("a", "2")
	if c.Len() != 1 {
		t.Errorf("重复 Put 同一键不应增加条目数，Len() = %d", c.Len())
	}
	if got, _ := c.Get("a"); got != "2" {
		t.Errorf("Get(a) = %q，期望 2（应被覆盖）", got)
	}
}

func TestLRUClear(t *testing.T) {
	c := NewLRU(2)
	c.Put("a", "1")
	c.Clear()
	if c.Len() != 0 {
		t.Errorf("Clear 后 Len() = %d，期望 0", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Clear 后不应命中")
	}
}

// 容量 1000 是 V1 的既定值（translator.py:214 CACHE_SIZE = 1000）
func TestLRUCapacity1000(t *testing.T) {
	c := NewLRU(1000)
	for i := 0; i < 1500; i++ {
		c.Put(string(rune('a'+i%26))+string(rune(i)), "v")
	}
	if c.Len() != 1000 {
		t.Errorf("Len() = %d，期望 1000（容量上限）", c.Len())
	}
}

// ── 同文本并发合并 ──

// 8 个并发的相同原文只应产生 1 次真实调用，且都拿到同一结果。
func TestInFlightMergesConcurrent(t *testing.T) {
	f := NewInFlight(300)
	var calls int32
	const n = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]string, n)
	shareds := make([]bool, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, shared, err := f.Do(context.Background(), "same-text", 3*time.Second,
				func(context.Context) (string, error) {
					atomic.AddInt32(&calls, 1)
					time.Sleep(120 * time.Millisecond) // 给其他 goroutine 时间进入等待
					return "译文", nil
				})
			results[i], shareds[i], errs[i] = res, shared, err
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("真实调用次数 = %d，期望 1（相同原文应合并）", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d 出错: %v", i, errs[i])
		}
		if results[i] != "译文" {
			t.Errorf("goroutine %d 结果 = %q，期望 译文", i, results[i])
		}
	}
	// 至少有一个是「自己算的」，其余应为 shared
	sharedCount := 0
	for _, s := range shareds {
		if s {
			sharedCount++
		}
	}
	if sharedCount == 0 {
		t.Error("应至少有请求复用了别人的结果（shared=true）")
	}
	if f.Pending() != 0 {
		t.Errorf("完成后 Pending() = %d，期望 0（否则会泄漏等待者）", f.Pending())
	}
}

// 不同原文不应互相合并。
func TestInFlightDifferentKeysNotMerged(t *testing.T) {
	f := NewInFlight(300)
	var calls int32
	var wg sync.WaitGroup
	for _, text := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			_, _, _ = f.Do(context.Background(), t, time.Second,
				func(context.Context) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "x", nil
				})
		}(text)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("真实调用次数 = %d，期望 3", got)
	}
}

// 失败路径必须释放等待者，否则后续请求会一直等下去。
func TestInFlightReleasesOnError(t *testing.T) {
	f := NewInFlight(300)
	sentinel := errors.New("boom")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = f.Do(context.Background(), "k", time.Second,
			func(context.Context) (string, error) {
				time.Sleep(30 * time.Millisecond)
				return "", sentinel
			})
	}()
	time.Sleep(5 * time.Millisecond)

	// 等待者应在对方失败后自己重试，而不是永远等下去
	done := make(chan struct{})
	go func() {
		_, _, _ = f.Do(context.Background(), "k", 2*time.Second,
			func(context.Context) (string, error) { return "ok", nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("等待者被卡住：失败路径没有释放 in-flight 槽位")
	}
	wg.Wait()
	if f.Pending() != 0 {
		t.Errorf("Pending() = %d，期望 0", f.Pending())
	}
}

// 等待超时后应自己翻译，而不是报错或卡住。
func TestInFlightWaitTimeoutFallsThrough(t *testing.T) {
	f := NewInFlight(300)
	// 占住槽位且长时间不返回
	go func() {
		_, _, _ = f.Do(context.Background(), "k", time.Second,
			func(context.Context) (string, error) {
				time.Sleep(2 * time.Second)
				return "slow", nil
			})
	}()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	res, shared, err := f.Do(context.Background(), "k", 100*time.Millisecond,
		func(context.Context) (string, error) { return "own", nil })
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("超时后自己翻译不应报错: %v", err)
	}
	if res != "own" {
		t.Errorf("结果 = %q，期望 own（超时后自行翻译）", res)
	}
	if shared {
		t.Error("超时后不应标记为 shared")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("耗时 %v，超时后应立即自己翻译", elapsed)
	}
}

// 结果表超限时整体清空（V1：>300 条 clear）。
func TestInFlightResultsBounded(t *testing.T) {
	f := NewInFlight(10)
	for i := 0; i < 50; i++ {
		key := string(rune('a' + i))
		_, _, _ = f.Do(context.Background(), key, time.Second,
			func(context.Context) (string, error) { return "v", nil })
	}
	f.mu.Lock()
	n := len(f.results)
	f.mu.Unlock()
	if n > 10 {
		t.Errorf("结果表条目数 = %d，期望不超过上限 10", n)
	}
}
