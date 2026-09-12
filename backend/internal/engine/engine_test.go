package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/cache"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// ── 测试替身 ──────────────────────────────────────────────────

// fakeLLM 记录每次调用，可注入错误。用于断言「缓存命中时不再调用」。
type fakeLLM struct {
	mu    sync.Mutex
	calls []string
	err   error
	// reply 按输入返回译文；nil 时返回 "【译】"+输入
	reply func(string) string
}

func (f *fakeLLM) Call(_ context.Context, text string) (string, string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, text)
	err := f.err
	reply := f.reply
	f.mu.Unlock()

	if err != nil {
		return "", "", "", err
	}
	if reply != nil {
		return reply(text), "FakeProvider", "fake-model", nil
	}
	return "【译】" + text, "FakeProvider", "fake-model", nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// minimalDict 构造一个最小可用词库。
//
// 为什么不用 Load() 无参：它会直接报「未加载到任何词库」。
// 也不能直接依赖 resources/dictionary.json——单元测试应当自给自足。
func minimalDict(t *testing.T) *dictionary.Dictionary {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "dictionary.json")
	const body = `{
	  "schemaVersion": 1,
	  "meta": {"generatedFrom": "test", "generatedAt": "2025-01-01T00:00:00Z"},
	  "slang": {"wtf": {"text": "什么鬼", "pauseAfter": false}},
	  "phrases": {"thank you": "谢谢"},
	  "structuredActions": {},
	  "systemMessages": [],
	  "ets2Terms": {},
	  "promptMapping": ""
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := dictionary.Load(p)
	if err != nil {
		t.Fatalf("加载测试词库失败: %v", err)
	}
	return d
}

func newTranslator(t *testing.T, llm LLMCaller, withCache bool) *Translator {
	t.Helper()
	tr := &Translator{
		Target:         "zh-CN",
		BatchSeparator: "\n---\n",
		Dict:           minimalDict(t),
		LLM:            llm,
	}
	if withCache {
		tr.Cache = cache.NewLRU(CacheSize)
	}
	return tr
}

func ev(text string) *domain.Event {
	return &domain.Event{ID: "e1", Speaker: "Player1", Text: text, Timestamp: "12:00:00"}
}

// ── 自己的消息：不翻译 ────────────────────────────────────────

// V1 translator.py:328-338 —— 自己的消息原样输出，计入 self_skipped，不进 LLM。
func TestSelfMessageIsNotTranslated(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)

	e := ev("你好世界")
	e.IsSelf = true
	msg := tr.Translate(context.Background(), e)

	if msg.Translated != "你好世界" {
		t.Errorf("自己的消息应原样返回，得到 %q", msg.Translated)
	}
	if !msg.IsSelf {
		t.Error("IsSelf 应为 true")
	}
	if llm.callCount() != 0 {
		t.Errorf("自己的消息不该调用 LLM，实际调用了 %d 次", llm.callCount())
	}

	st := tr.Stats()
	if st.SelfSkipped != 1 || st.Translated != 0 || st.Cached != 0 {
		t.Errorf("统计错误: %+v，期望 selfSkipped=1", st)
	}
}

// ── 翻译结果缓存 ──────────────────────────────────────────────

// V1 translator.py:341-352 —— 相同原文第二次直接命中缓存，不再调 LLM。
func TestCacheHitAvoidsLLM(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)
	ctx := context.Background()

	first := tr.Translate(ctx, ev("hello world"))
	if first.CacheState == "hit" {
		t.Fatalf("第一次不该是缓存命中，状态=%q", first.CacheState)
	}
	if llm.callCount() != 1 {
		t.Fatalf("第一次应调用 LLM 一次，实际 %d 次", llm.callCount())
	}

	second := tr.Translate(ctx, ev("hello world"))
	if second.CacheState != "hit" {
		t.Errorf("第二次应是缓存命中，状态=%q", second.CacheState)
	}
	if second.Translated != first.Translated {
		t.Errorf("缓存命中应返回相同译文：%q vs %q", second.Translated, first.Translated)
	}
	if llm.callCount() != 1 {
		t.Errorf("缓存命中后不该再调 LLM，实际共 %d 次", llm.callCount())
	}

	st := tr.Stats()
	if st.Cached != 1 || st.Translated != 1 {
		t.Errorf("统计错误: %+v，期望 translated=1 cached=1", st)
	}
	if got := st.SavingsPct(); got != "50%" {
		t.Errorf("节省率 = %q，期望 50%%", got)
	}
}

// 缓存的 key 必须是**整条原文**：不同原文不能互相命中。
func TestCacheKeyIsWholeText(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)
	ctx := context.Background()

	tr.Translate(ctx, ev("hello world"))
	tr.Translate(ctx, ev("hello there"))

	if llm.callCount() != 2 {
		t.Errorf("不同原文应各调一次 LLM，实际共 %d 次", llm.callCount())
	}
	if st := tr.Stats(); st.Cached != 0 {
		t.Errorf("不该有缓存命中，实际 cached=%d", st.Cached)
	}
}

// 没有缓存时报错状态不会污染后续。
func TestCacheDisabledStillWorks(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, false) // Cache 为 nil
	ctx := context.Background()

	tr.Translate(ctx, ev("hello world"))
	tr.Translate(ctx, ev("hello world"))

	if llm.callCount() != 2 {
		t.Errorf("未启用缓存时应每次都调用，实际 %d 次", llm.callCount())
	}
	if st := tr.Stats(); st.Cached != 0 || st.Translated != 2 {
		t.Errorf("统计错误: %+v", st)
	}
}

// ── 错误不进入缓存（有意修 V1 缺陷 D14）────────────────────────

// V1 会把 "[网络错误] …" 这类文案一起缓存，导致网络恢复后同一条消息
// 仍然一直报错。V2 明确不缓存错误状态。
func TestErrorIsNotCached(t *testing.T) {
	llm := &fakeLLM{err: errors.New("boom")}
	tr := newTranslator(t, llm, true)
	ctx := context.Background()

	first := tr.Translate(ctx, ev("hello world"))
	if first.CacheState != "error" {
		t.Fatalf("期望状态 error，实际 %q", first.CacheState)
	}
	if !strings.Contains(first.Translated, "boom") {
		t.Errorf("错误文本应可见，实际 %q", first.Translated)
	}

	// 网络恢复
	llm.mu.Lock()
	llm.err = nil
	llm.mu.Unlock()

	second := tr.Translate(ctx, ev("hello world"))
	if second.CacheState == "hit" {
		t.Fatal("错误结果不该被缓存——这会导致故障恢复后仍然一直报错")
	}
	if second.CacheState != "llm" {
		t.Errorf("恢复后应重新翻译，状态=%q", second.CacheState)
	}
}

// ── 统计累加 ──────────────────────────────────────────────────

// 统计口径与 V1 一致：self_skipped / cached / translated 三分类，互不重叠。
func TestStatsAccumulate(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)
	ctx := context.Background()

	self := ev("你好")
	self.IsSelf = true

	tr.Translate(ctx, self)          // self_skipped
	tr.Translate(ctx, ev("hello"))   // translated
	tr.Translate(ctx, ev("hello"))   // cached
	tr.Translate(ctx, ev("goodbye")) // translated

	st := tr.Stats()
	if st.SelfSkipped != 1 {
		t.Errorf("selfSkipped = %d，期望 1", st.SelfSkipped)
	}
	if st.Cached != 1 {
		t.Errorf("cached = %d，期望 1", st.Cached)
	}
	if st.Translated != 2 {
		t.Errorf("translated = %d，期望 2", st.Translated)
	}
	if st.Total() != 4 {
		t.Errorf("total = %d，期望 4", st.Total())
	}
	// 分子只含 cached + selfSkipped（V1 口径），且为截断
	if got := st.SavingsPct(); got != "50%" {
		t.Errorf("节省率 = %q，期望 50%%", got)
	}
}

func TestResetStats(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)
	tr.Translate(context.Background(), ev("hello"))
	if tr.Stats().Translated != 1 {
		t.Fatal("统计没累加")
	}

	tr.ResetStats()
	if st := tr.Stats(); st.Total() != 0 {
		t.Errorf("重置后应为零值，实际 %+v", st)
	}
}

// ── 本地词典优先于 LLM ────────────────────────────────────────

// 俚语直接命中词典，零 API 调用，状态为 local_dict。
func TestLocalDictBeatsLLM(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)

	msg := tr.Translate(context.Background(), ev("wtf"))
	if msg.Translated != "什么鬼" {
		t.Errorf("词典命中译文 = %q，期望 什么鬼", msg.Translated)
	}
	if msg.CacheState != "local_dict" {
		t.Errorf("状态 = %q，期望 local_dict", msg.CacheState)
	}
	if llm.callCount() != 0 {
		t.Errorf("词典命中不该调用 LLM，实际 %d 次", llm.callCount())
	}
	// 词典命中同样算 translated（V1 里它发生在批次内部）
	if st := tr.Stats(); st.Translated != 1 {
		t.Errorf("translated = %d，期望 1", st.Translated)
	}
}

// 纯目标语言不翻译。
func TestAllTargetLanguageSkipped(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)

	msg := tr.Translate(context.Background(), ev("你好世界"))
	if msg.Translated != "你好世界" {
		t.Errorf("目标语言应原样返回，实际 %q", msg.Translated)
	}
	if llm.callCount() != 0 {
		t.Errorf("不该调用 LLM，实际 %d 次", llm.callCount())
	}
}

// ── TranslateText 与 Translate 共享统计与缓存 ──────────────────

// /api/translate 走的是 TranslateText；它必须与主链路共用缓存与统计，
// 否则前端手动翻译不会体现在统计条上。
func TestTranslateTextSharesCache(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, true)

	tr.TranslateText("hello world")
	tr.TranslateText("hello world")

	if llm.callCount() != 1 {
		t.Errorf("TranslateText 应共享缓存，实际调用 %d 次", llm.callCount())
	}
	st := tr.Stats()
	if st.Cached != 1 || st.Translated != 1 {
		t.Errorf("TranslateText 应共享统计，实际 %+v", st)
	}
}
