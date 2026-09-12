package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/cache"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// ── 测试替身 ──────────────────────────────────────────────────

// slowLLM 在每次调用里停顿，用来制造「同文本并发」的窗口。
// fakeLLM 是瞬时返回的，5 个协程会依次跑完，永远测不到合并。
type slowLLM struct {
	delay time.Duration

	mu    sync.Mutex
	calls []string
}

func (s *slowLLM) Call(_ context.Context, text string) (string, string, string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, text)
	s.mu.Unlock()

	time.Sleep(s.delay)
	return "【译】" + text, "P", "m", nil
}

func (s *slowLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// batchEvent 造一条待翻译事件，ID 唯一便于定位是哪一条出错。
func batchEvent(i int, text string) domain.Event {
	return domain.Event{
		ID:        string(rune('a' + i)),
		Speaker:   "Player1",
		Text:      text,
		Timestamp: "12:00:00",
	}
}

// runBatcher 把 events 全部喂给批量器，收回等量的成品。
//
// src 带缓冲且一次性填满：批量器一读就是「这几条同时到达」，
// 正是真实场景里连续几行聊天日志被一次扫出来的样子。
func runBatcher(
	t *testing.T, tr *Translator, window time.Duration, maxSize int, events []domain.Event,
) ([]domain.RenderedMessage, time.Duration) {
	t.Helper()

	b := NewBatcher(tr, window, maxSize)
	src := make(chan domain.Event, len(events))
	results := make(chan domain.RenderedMessage, len(events)+1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, src, results)

	start := time.Now()
	for _, e := range events {
		src <- e
	}

	got := make([]domain.RenderedMessage, 0, len(events))
	for range events {
		select {
		case m := <-results:
			got = append(got, m)
		case <-time.After(5 * time.Second):
			t.Fatalf("等待成品超时：只要到 %d/%d 条", len(got), len(events))
		}
	}
	return got, time.Since(start)
}

// ── 参数必须与 V1 一致 ────────────────────────────────────────

// V1 translator.py:215-216 —— 窗口 0.3 秒、分隔符 "\n---\n"。
// 这两个数字写错了批量就不会按 V1 的方式合并，但它们没有任何其它约束，
// 只有这条断言能挡住「顺手调成 1 秒更好」这种改动。
func TestBatchConstantsMatchV1(t *testing.T) {
	if BatchWindow != 300*time.Millisecond {
		t.Errorf("批量窗口应为 0.3 秒（V1 translator.py:215），实际 %v", BatchWindow)
	}
	if BatchMaxSize != 8 {
		t.Errorf("批量上限应为 8 条（V1 translator.py:360），实际 %d", BatchMaxSize)
	}
	if DefaultBatchSeparator != "\n---\n" {
		t.Errorf("分隔符应为 \"\\n---\\n\"（V1 translator.py:216），实际 %q", DefaultBatchSeparator)
	}
	if statsLogEvery != 50 {
		t.Errorf("统计日志间隔应为 50 条（V1 translator.py:372），实际 %d", statsLogEvery)
	}
}

// ── 窗口内到达的多条消息合并成一次调用 ────────────────────────

// V1 translator.py:354-363 + 447-448 —— 窗口内到达的消息攒成一批，
// 用分隔符连接后**只调一次** LLM。
//
// 这条是 P0-1 的核心：没有它，「实现了批量」就只是一句注释。
func TestBatchMergesMessagesIntoOneCall(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string {
		return strings.ReplaceAll(s, "hello", "你好")
	}}
	tr := newTranslator(t, llm, false)

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		batchEvent(0, "hello one"),
		batchEvent(1, "hello two"),
		batchEvent(2, "hello three"),
	})

	if n := llm.callCount(); n != 1 {
		t.Fatalf("3 条消息在窗口内到达应只调用 1 次 LLM（V1 translator.py:447），实际 %d 次: %q",
			n, llm.calls)
	}
	want := "hello one\n---\nhello two\n---\nhello three"
	if llm.calls[0] != want {
		t.Errorf("合并文本应为分隔符连接的三段\n得到 %q\n期望 %q", llm.calls[0], want)
	}

	// 拆分成功 → 按顺序分配（V1 translator.py:449-452）
	for i, want := range []string{"你好 one", "你好 two", "你好 three"} {
		if got[i].Translated != want {
			t.Errorf("第 %d 条译文应为 %q，实际 %q", i+1, want, got[i].Translated)
		}
		if got[i].CacheState != "llm" {
			t.Errorf("第 %d 条状态应为 llm，实际 %q", i+1, got[i].CacheState)
		}
	}
}

// V1 translator.py:360 —— 攒满 8 条立刻冲刷，不等窗口到期。
//
// 用 5 秒的窗口来测：如果实现是「等窗口」，这条会等满 5 秒才出结果，
// 断言耗时就能抓住它，而不只是抓调用次数。
func TestBatchFlushesImmediatelyWhenFull(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string {
		return strings.ReplaceAll(s, "hello", "你好")
	}}
	tr := newTranslator(t, llm, false)

	events := make([]domain.Event, 0, 3)
	for i := 0; i < 3; i++ {
		events = append(events, batchEvent(i, "hello "+string(rune('a'+i))))
	}

	got, elapsed := runBatcher(t, tr, 5*time.Second, 3, events)

	if n := llm.callCount(); n != 1 {
		t.Fatalf("攒满上限应立刻合并成 1 次调用，实际 %d 次", n)
	}
	if elapsed > 2*time.Second {
		t.Errorf("攒满上限应立刻冲刷，不该等 5 秒窗口；实际耗时 %v", elapsed)
	}
	if len(got) != 3 {
		t.Fatalf("应产出 3 条成品，实际 %d 条", len(got))
	}
}

// ── 拆分失败必须逐条回退 ──────────────────────────────────────

// V1 translator.py:453-462 —— LLM 没按分隔符回显时逐条重翻。
//
// 不这么做的话，整段合并译文会**全部挂到第一条**上，后面几条拿到的是别人的
// 翻译。这比翻译失败更坏：用户看不出错，只会觉得机翻乱来。
func TestBatchFallsBackPerMessageWhenSeparatorMissing(t *testing.T) {
	llm := &fakeLLM{reply: func(string) string { return "统一译文" }}
	tr := newTranslator(t, llm, false)

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		batchEvent(0, "hello one"),
		batchEvent(1, "hello two"),
		batchEvent(2, "hello three"),
	})

	// 1 次合并调用 + 3 次逐条回退
	if n := llm.callCount(); n != 4 {
		t.Fatalf("拆分失败应回退逐条翻译（1 次合并 + 3 次逐条 = 4），实际 %d 次: %q",
			n, llm.calls)
	}
	for i := range got {
		if got[i].Translated != "统一译文" {
			t.Errorf("第 %d 条应各自拿到逐条翻译结果，实际 %q", i+1, got[i].Translated)
		}
	}
}

// V1 translator.py:483-497 —— 合并调用整个失败时，**整批**都显示同一条错误文案。
//
// 关键是不能再去逐条回退：那会白白打 3 次注定失败的请求，
// 把「Provider 挂了」放大成 4 倍请求量。
func TestBatchFailureMarksWholeBatchOnce(t *testing.T) {
	llm := &fakeLLM{err: context.DeadlineExceeded}
	tr := newTranslator(t, llm, false)

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		batchEvent(0, "hello one"),
		batchEvent(1, "hello two"),
		batchEvent(2, "hello three"),
	})

	if n := llm.callCount(); n != 1 {
		t.Fatalf("整批失败后不该再逐条重试，实际调用 %d 次", n)
	}
	for i := range got {
		if got[i].CacheState != "error" {
			t.Errorf("第 %d 条状态应为 error，实际 %q", i+1, got[i].CacheState)
		}
		if got[i].Translated == "" {
			t.Errorf("第 %d 条应带上失败原因，实际为空", i+1)
		}
	}
}

// ── 自己的消息与缓存命中不进批次 ──────────────────────────────

// V1 translator.py:328-352 —— 这两类都在 `batch.append(msg)` **之前**就输出了。
//
// 放进批次有两个坏处：白等 0.3 秒，以及占用批量的 8 个名额
// （一次刷屏里大半是自己的消息时，真正要翻译的反而攒不满）。
func TestBatchBypassesSelfAndCachedMessages(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string {
		return strings.ReplaceAll(s, "hello", "你好")
	}}
	tr := newTranslator(t, llm, true)
	tr.Cache.Put("hello cached", "已缓存的译文")

	self := batchEvent(0, "你好世界")
	self.IsSelf = true

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		self,
		batchEvent(1, "hello cached"),
		batchEvent(2, "hello fresh"),
	})

	if n := llm.callCount(); n != 1 {
		t.Fatalf("自己的消息与缓存命中都不该进 LLM，实际调用 %d 次: %q", n, llm.calls)
	}
	if llm.calls[0] != "hello fresh" {
		t.Errorf("只有未命中的那条该进批次，实际合并文本 %q", llm.calls[0])
	}
	if got[0].CacheState != "self_skip" {
		t.Errorf("自己的消息状态应为 self_skip，实际 %q", got[0].CacheState)
	}
	if got[1].CacheState != "hit" || got[1].Translated != "已缓存的译文" {
		t.Errorf("缓存命中应直接返回缓存值，实际 state=%q text=%q",
			got[1].CacheState, got[1].Translated)
	}
	if got[2].Translated != "你好 fresh" {
		t.Errorf("第三条应正常翻译，实际 %q", got[2].Translated)
	}
}

// ── 单条仍走混合语言拆分 ──────────────────────────────────────

// V1 translator.py:422-445 —— 批次只有 1 条时走 _translate_with_mixed_lang，
// 而不是合并路径。合并路径是把整段直接送 LLM，不含混合语言拆分，
// 单条走错了会让「中文里夹的英文」整条被送去翻译。
func TestBatchSingleMessageUsesMixedPath(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string {
		return strings.ReplaceAll(s, "hello", "你好")
	}}
	tr := newTranslator(t, llm, false)

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		batchEvent(0, "早上好 hello"),
	})

	if len(got) != 1 {
		t.Fatalf("应产出 1 条成品，实际 %d 条", len(got))
	}
	// ⚠️ 结果是 "早上好你好"（**没有空格**），这不是 bug：
	//   · V1 split_mixed_text 把标点与空白并入**前一个**片段（translator.py:148），
	//     所以 "早上好 hello" 切成 [("早上好", True), (" hello", False)]
	//   · V1 用 fseg 原文当键存译文，但送去翻译的是 fseg.strip()（translator.py:411-414）
	//   · V1 reassemble_mixed 是 "".join(parts)（translator.py:211），不补分隔符
	// 三者合起来，那个空格在 V1 里本来就消失了。别「顺手」补一个空格回去。
	if got[0].Translated != "早上好你好" {
		t.Errorf("混合文本应保留中文片段并只翻译英文片段，实际 %q", got[0].Translated)
	}
	if n := llm.callCount(); n != 1 {
		t.Errorf("只该翻译外来片段一次，实际 %d 次: %q", n, llm.calls)
	}
	// 送进去的应该是 trim 过的片段——带前导空格的话 LLM 会多回一个空格，
	// 结果就和 V1 不一致了。
	if llm.calls[0] != "hello" {
		t.Errorf("送进 LLM 的应只是 trim 过的外来片段 %q，实际 %q", "hello", llm.calls[0])
	}
}

// ── P0-2：同文本并发合并 ──────────────────────────────────────

// V1 translator.py:612-627 —— 相同原文的并发请求只真正翻一次，
// 其余等待者复用结果。
//
// 少了这一条，多人同时刷同一句话（比如同一段广告）时请求量按人数翻倍。
func TestInFlightMergesConcurrentIdenticalRequests(t *testing.T) {
	llm := &slowLLM{delay: 200 * time.Millisecond}
	tr := newTranslator(t, llm, false)
	tr.InFlight = cache.NewInFlight(300)

	const n = 5
	// 开始信号：保证 5 个协程尽量同时冲进 callAPI，
	// 否则先到的那个早就翻完了，后面的根本看不到「在飞」。
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			tr.Translate(context.Background(), &domain.Event{
				ID: "same", Speaker: "P", Text: "hello same", Timestamp: "12:00:00",
			})
		}()
	}
	close(start)
	wg.Wait()

	if got := llm.callCount(); got != 1 {
		t.Errorf("%d 个同文本并发请求应只真正翻 1 次（V1 translator.py:612-627），实际 %d 次",
			n, got)
	}
}

// 不同原文不该被合并掉——合并是按键合并，不是「一批只翻一条」。
func TestInFlightKeepsDistinctTextsSeparate(t *testing.T) {
	llm := &slowLLM{delay: 20 * time.Millisecond}
	tr := newTranslator(t, llm, false)
	tr.InFlight = cache.NewInFlight(300)

	var wg sync.WaitGroup
	for _, text := range []string{"hello one", "hello two", "hello three"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			tr.Translate(context.Background(), &domain.Event{
				ID: text, Speaker: "P", Text: text, Timestamp: "12:00:00",
			})
		}(text)
	}
	wg.Wait()

	if got := llm.callCount(); got != 3 {
		t.Errorf("三条不同原文应各翻一次，实际 %d 次: %q", got, llm.calls)
	}
}

// ── P0-3：日志要能记到 Provider / Model ───────────────────────

// V1 translator.py:631-632 + 439-445 —— 翻译日志里的 Provider 与模型
// 来自最近一次真实调用。V2 曾经把它们丢掉，于是日志退化成
// `时间 - - - 原文 - 译文`，「哪家 Provider 生效」无法复原。
func TestProviderAndModelReachRenderedMessage(t *testing.T) {
	llm := &fakeLLM{} // 返回 FakeProvider / fake-model
	tr := newTranslator(t, llm, false)

	msg := tr.Translate(context.Background(), ev("hello world"))

	if msg.Provider != "FakeProvider" || msg.Model != "fake-model" {
		t.Errorf("成品消息应带上生效的 Provider/Model，实际 provider=%q model=%q",
			msg.Provider, msg.Model)
	}
}

// 批量路径同样要带上 Provider/Model：V1 的批量分支记日志时
// 取的也是 _last_provider / _last_model（translator.py:476-482）。
func TestProviderAndModelReachBatchedMessages(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string {
		return strings.ReplaceAll(s, "hello", "你好")
	}}
	tr := newTranslator(t, llm, false)

	got, _ := runBatcher(t, tr, 120*time.Millisecond, 0, []domain.Event{
		batchEvent(0, "hello one"),
		batchEvent(1, "hello two"),
	})

	for i := range got {
		if got[i].Provider != "FakeProvider" || got[i].Model != "fake-model" {
			t.Errorf("批量第 %d 条应带上 Provider/Model，实际 provider=%q model=%q",
				i+1, got[i].Provider, got[i].Model)
		}
	}
}

// 混合文本里片段翻译成功时，Provider 来自那个片段；
// 全是本地词典命中时没有真实调用，不该凭空编一个 Provider。
func TestProviderEmptyWhenNothingWasCalled(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, false)

	// "wtf" 命中测试词库的俚语表 → 零 API 调用
	msg := tr.Translate(context.Background(), ev("wtf"))

	if msg.CacheState != "local_dict" {
		t.Fatalf("应命中本地词典，实际状态 %q", msg.CacheState)
	}
	if llm.callCount() != 0 {
		t.Fatalf("本地词典命中不该调用 LLM，实际 %d 次", llm.callCount())
	}
	if msg.Provider != "" || msg.Model != "" {
		t.Errorf("没有真实调用时不该有 Provider/Model，实际 provider=%q model=%q",
			msg.Provider, msg.Model)
	}
}

// ── 每 50 条统计日志 ─────────────────────────────────────────

// V1 translator.py:371-377 —— 每累计 50 条记一次统计。
func TestBatchStatsLogEvery50(t *testing.T) {
	llm := &fakeLLM{reply: func(s string) string { return s }}
	tr := newTranslator(t, llm, false)

	b := NewBatcher(tr, 60*time.Millisecond, 8)
	var fired int
	b.SetStatsLogger(func(domain.Stats) { fired++ })

	src := make(chan domain.Event, 120)
	results := make(chan domain.RenderedMessage, 120)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx, src, results)

	const total = 50
	for i := 0; i < total; i++ {
		src <- batchEvent(0, "hello "+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	// 收满 50 条成品后再看回调次数，避免在批量器还在冲刷时就断言
	for i := 0; i < total; i++ {
		select {
		case <-results:
		case <-time.After(5 * time.Second):
			t.Fatalf("等待成品超时：只要到 %d/%d 条", i, total)
		}
	}

	if fired != 1 {
		t.Errorf("累计 50 条应触发 1 次统计日志（V1 translator.py:372），实际 %d 次", fired)
	}
}
