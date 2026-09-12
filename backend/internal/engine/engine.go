// 接收方向翻译编排 —— V1 单条消息路径的逐行等价：
//
//	_flush_llm（单条分支）→ _translate_with_mixed_lang → _call_api 的本地部分
//
// 顺序不可变（每个分支都有真实语义）：
//  1. 混合语言拆分：全目标语言 → 原样返回；纯外来 → 整条 callAPI；混合 → 逐片段 callAPI
//  2. callAPI：非文字跳过 → 已是目标语言跳过 → 本地词典拦截 → 同文本合并 → LLM → 后处理
//
// 注意：词典拦截发生在 callAPI 层，因此对混合文本的**外来片段**同样生效
// （V1 行为：如 "你好 wtf" 的 "wtf" 片段会命中词典 → "你好什么鬼"，零 API 调用）。
//
// 批量路径在 batch.go，与这里的单条路径共用 finish()（写缓存与统计只有一处实现）。
package engine

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/cache"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// LLMCaller 是引擎对 LLM 的抽象。
// 真实实现由 internal/providers 提供（竞速/轮转/熔断），
// 另外两个返回值是本次实际生效的 Provider label 与模型名。
type LLMCaller interface {
	Call(ctx context.Context, text string) (translated, provider, model string, err error)
}

// DefaultBatchSeparator 与 V1 translator.py:216 的 BATCH_SEPARATOR 一致。
const DefaultBatchSeparator = "\n---\n"

// inFlightWait 与 V1 translator.py:622 的 `event_to_await.wait(timeout=10.0)` 一致。
const inFlightWait = 10 * time.Second

// CacheSize 与 V1 translator.py 的 CACHE_SIZE 一致。
const CacheSize = 1000

// Translator 是接收方向引擎。
type Translator struct {
	// Target 对应 cfg.target_language
	Target string

	// BatchSeparator 对应 V1 的 BATCH_SEPARATOR（"\n---\n"）。
	// 空字符串按 DefaultBatchSeparator 处理。
	BatchSeparator string

	Dict *dictionary.Dictionary
	LLM  LLMCaller

	// Cache 是翻译结果缓存，对应 V1 translator.py 的 self._cache。
	//
	// key 是**整条原文**，value 是**整条最终译文**——与 V1 完全一致。
	// 不要改成片段级缓存：混合文本的重组语义依赖整条结果。
	// 为 nil 表示不缓存（测试与差分工具用）。
	Cache *cache.LRU

	// InFlight 把同原文的并发请求合并成一次真实调用，
	// 对应 V1 translator.py:612-627 的 _in_flight / _in_flight_results。
	// 为 nil 表示不合并。
	//
	// ⚠️ 它与 Cache 是**两回事**，不要互相替代：
	//   Cache    是「这句以前翻过」——跨时间，存的是最终译文，容量 1000
	//   InFlight 是「这句**此刻**正在翻」——跨并发，存的是给等待者传结果的中转，
	//            超过 300 条整体清空（V1 translator.py:304-307）
	InFlight *cache.InFlight

	// Batch 是批量翻译器（V1 translator.py:354-363 的 0.3 秒聚集窗口）。
	// 为 nil 表示逐条翻译——API 的同步入口与测试都走这条。
	Batch *Batcher

	mu    sync.Mutex
	stats domain.Stats

	// lastProvider / lastModel 是**最近一次真实调用**生效的 Provider 与模型，
	// 对应 V1 的 self._last_provider / self._last_model（translator.py:269-270、
	// 写入点 631-632）。合并命中的请求没有自己的 Provider，
	// V1 记日志时取的也是这个全局值，所以这里保持同样的语义。
	lastMu       sync.Mutex
	lastProvider string
	lastModel    string
}

// Stats 返回统计快照。对应 V1 message_types.TranslationStats。
func (t *Translator) Stats() domain.Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// ResetStats 清零统计（配置变更后重新计数用）。
func (t *Translator) ResetStats() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats = domain.Stats{}
}

// ClearCache 清空翻译结果缓存。
//
// 配置变更时**必须**调：没配 Provider 时返回的占位文案「〔未配置 Provider〕」
// 不报错，会被当成正常译文写进缓存（key 是原文）。用户在设置页配好 Key 之后
// 再看同一条消息会走缓存命中、根本到不了 LLM，于是「配了也没用」。
// 换目标语言、换 Provider 之后，旧译文同样不该继续复用。
func (t *Translator) ClearCache() {
	if t.Cache != nil {
		t.Cache.Clear()
	}
}

// Translate 处理**一条**领域事件，返回成品消息。
//
// 这是同步的单条入口：不经过批量窗口，调用方拿到结果才返回。
// API 的 /api/translate 与测试用它；聊天日志那条高频链路走 Batcher，
// 否则同步等待会把 0.3 秒的聚集窗口彻底废掉（循环卡在第一条上，
// 后面几条根本没机会入批）。
func (t *Translator) Translate(ctx context.Context, ev *domain.Event) domain.RenderedMessage {
	if msg, done := t.preTranslate(ev); done {
		return msg
	}
	translated, state, provider, model := t.translateOne(ctx, ev.Text)
	return t.finish(ev, translated, state, provider, model)
}

// preTranslate 处理两类「不进批次」的消息，顺序与 V1 消费循环一致：
//  1. 自己的消息：原样输出，计入 self_skipped（translator.py:328-338）
//  2. 翻译结果缓存命中：直接返回，计入 cached（translator.py:340-352）
//
// done=true 表示已经可以直接输出。V1 里这两类都在 `batch.append(msg)` **之前**
// 就 continue 了，所以它们必须绕过批量窗口——放进批次会让它们平白多等 0.3 秒，
// 而且缓存命中的消息根本不该占用批量的 8 个名额。
func (t *Translator) preTranslate(ev *domain.Event) (domain.RenderedMessage, bool) {
	if ev.IsSelf {
		t.mu.Lock()
		t.stats.SelfSkipped++
		t.mu.Unlock()
		return domain.RenderedMessage{
			ID:         ev.ID,
			Speaker:    ev.Speaker,
			Original:   ev.Text,
			Translated: ev.Text,
			Lang:       DetectLanguage(ev.Text),
			Timestamp:  ev.Timestamp,
			IsSelf:     true,
			IsSystem:   ev.IsSystem,
			CacheState: "self_skip",
		}, true
	}

	if t.Cache != nil {
		if hit, ok := t.Cache.Get(ev.Text); ok {
			t.mu.Lock()
			t.stats.Cached++
			t.mu.Unlock()
			return domain.RenderedMessage{
				ID:         ev.ID,
				Speaker:    ev.Speaker,
				Original:   ev.Text,
				Translated: hit,
				Lang:       DetectLanguage(ev.Text),
				Timestamp:  ev.Timestamp,
				IsSelf:     ev.IsSelf,
				IsSystem:   ev.IsSystem,
				CacheState: "hit",
			}, true
		}
	}
	return domain.RenderedMessage{}, false
}

// finish 把译文组装成成品消息，并计入统计、写入缓存。
//
// 单条路径与批量路径**共用这一处**：两条路都要写缓存、都要计 translated。
// V1 的 _flush_llm 两个分支各写了一遍同样的代码，于是只有其中一边记得写缓存
// 时就会出一半消息永远走不到缓存的怪事——这里刻意不重复。
func (t *Translator) finish(
	ev *domain.Event, translated, state, provider, model string,
) domain.RenderedMessage {
	t.mu.Lock()
	t.stats.Translated++
	t.mu.Unlock()

	// 写缓存。**不缓存错误**：V1 会把 "[网络错误] …" 这类文案一起缓存，
	// 导致网络恢复后同一条消息仍然一直报错（见迁移对照表 D14）。
	// 批量分支同样受这一条约束。
	if t.Cache != nil && state != "error" {
		t.Cache.Put(ev.Text, translated)
	}

	if provider == "" {
		provider, model = t.lastUsed()
	}
	return domain.RenderedMessage{
		ID:         ev.ID,
		Speaker:    ev.Speaker,
		Original:   ev.Text,
		Translated: translated,
		Lang:       DetectLanguage(ev.Text),
		Timestamp:  ev.Timestamp,
		IsSelf:     ev.IsSelf,
		IsSystem:   ev.IsSystem,
		CacheState: state,
		Provider:   provider,
		Model:      model,
	}
}

// SetTarget 在锁保护下切换目标语言。
//
// ⚠️ 不要在运行期直接写 `t.Target = x`。字符串是两个机器字（指针+长度），
// 一边改一边读可能读到「旧指针 + 新长度」这种撕裂值，随后就是越界访问或崩溃。
// 字段保持公开只是为了构造时赋值方便，运行期一律走 SetTarget / targetNow。
func (t *Translator) SetTarget(lang string) {
	t.mu.Lock()
	t.Target = lang
	t.mu.Unlock()
}

// targetNow 读取当前目标语言（与 SetTarget 配对）。
func (t *Translator) targetNow() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Target
}

// TranslateText 是给 api 层用的便捷入口：只关心一段文本，不构造完整事件。
func (t *Translator) TranslateText(text string) domain.RenderedMessage {
	return t.Translate(context.Background(), &domain.Event{Text: text, Origin: "api"})
}

// TargetLanguage 返回目标语言（供 /api/health 与 /api/stats 展示）。
// 方法名不能叫 Target——那与字段 Target 冲突。
func (t *Translator) TargetLanguage() string { return t.targetNow() }

// batchSeparator 返回批量分隔符，未配置时用 V1 的默认值。
func (t *Translator) batchSeparator() string {
	if t.BatchSeparator != "" {
		return t.BatchSeparator
	}
	return DefaultBatchSeparator
}

// noteUsed 记录最近一次真实调用生效的 Provider 与模型。
func (t *Translator) noteUsed(provider, model string) {
	if provider == "" {
		return
	}
	t.lastMu.Lock()
	t.lastProvider, t.lastModel = provider, model
	t.lastMu.Unlock()
}

// lastUsed 读取最近一次真实调用生效的 Provider 与模型。
func (t *Translator) lastUsed() (string, string) {
	t.lastMu.Lock()
	defer t.lastMu.Unlock()
	return t.lastProvider, t.lastModel
}

// translateOne = _translate_with_mixed_lang + _call_api（本地部分）。
//
// 返回值依次是：译文、缓存状态、生效 Provider、生效模型。
// 缓存状态取值：skip_empty / all_target / mixed /
// skip_nontext / skip_target / local_dict / llm / error。
func (t *Translator) translateOne(
	ctx context.Context, text string,
) (string, string, string, string) {
	if strings.TrimSpace(text) == "" {
		return text, "skip_empty", "", ""
	}

	// 目标语言取一次快照：本函数中间可能被 SetTarget 改掉，而分段、判定、
	// 重组必须用**同一个**目标语言，否则会拼出半新半旧的结果。
	target := t.targetNow()

	segments := SplitMixedText(text, target)
	if len(segments) == 0 {
		return text, "skip_empty", "", ""
	}

	var foreign []Segment
	hasTarget := false
	for _, seg := range segments {
		if seg.IsTarget {
			hasTarget = true
		} else {
			foreign = append(foreign, seg)
		}
	}

	if len(foreign) == 0 {
		// 全部已是目标语言——不译（V1 _translate_with_mixed_lang 的纯目标分支）
		return text, "all_target", "", ""
	}

	if !hasTarget {
		// 纯外来——整条走 callAPI
		return t.callAPI(ctx, text)
	}

	// 混合：每个外来片段单独走 callAPI（片段级词典拦截在此生效）
	//
	// V1 translator.py:413-418 在这里 try/except，片段翻译失败就**回退原文片段**。
	// V2 的 callAPI 不抛异常（返回 err.Error() 文本 + "error" 状态），
	// 所以这里要显式判断：否则失败文案会被当成译文嵌进句子中间，
	// 变成「你好 [网络错误] 世界」这种东西。
	translations := make(map[string]string, len(foreign))
	provider, model := "", ""
	for _, fseg := range foreign {
		stripped := strings.TrimSpace(fseg.Text)
		if stripped == "" {
			translations[fseg.Text] = fseg.Text
			continue
		}
		out, state, p, m := t.callAPI(ctx, stripped)
		if state == "error" {
			out = fseg.Text // V1 fallback to original
		} else if p != "" {
			provider, model = p, m
		}
		translations[fseg.Text] = out
	}
	return ReassembleMixed(segments, translations), "mixed", provider, model
}

// callAPI = V1 _call_api（translator.py:601-644）。
//
// 顺序不可换：
//  1. _should_skip          —— 非文字 / 已是目标语言，直接原样返回
//  2. 本地词典拦截           —— 俚语/短语命中则零 API 调用
//  3. 同文本并发合并         —— 相同原文的并发请求只真正翻一次
//  4. LLM                   —— Provider 竞速
//  5. 出口后处理            —— 仅单条消息，批量请求跳过
func (t *Translator) callAPI(
	ctx context.Context, text string,
) (string, string, string, string) {
	// 1) 跳过：非文字 或 已是目标语言（对应 _should_skip）
	if t.Dict != nil && dictionary.NonTranslatable(text) {
		return text, "skip_nontext", "", ""
	}
	if ShouldSkip(text, t.targetNow()) {
		return text, "skip_target", "", ""
	}

	// 2) 本地词典拦截：俚语/短语直接命中，零 API 调用
	if t.Dict != nil {
		if quick, ok := t.Dict.Lookup(text); ok {
			return quick, "local_dict", "", ""
		}
	}

	// 3~4) 真实调用。抽成闭包是为了既能直接调，也能交给 InFlight 合并。
	call := func(ctx context.Context) (string, error) {
		out, provider, model, err := t.LLM.Call(ctx, text)
		if err != nil {
			return "", err
		}
		t.noteUsed(provider, model)

		// 出口后处理**只对单条消息**做——V1 translator.py:634 的
		// `if BATCH_SEPARATOR not in text`。
		//
		// 批量请求的整段译文里混着分隔符，在这里补译缩写或保留 @mention
		// 会串到别的消息上（比如把第 3 条的 @名字 挪到第 1 条前面）。
		if !strings.Contains(text, t.batchSeparator()) {
			if t.Dict != nil {
				out = t.Dict.FixLeftoverShorthand(out)
			}
			out = dictionary.PreserveMentionPrefix(text, out)
		}
		return out, nil
	}

	var out string
	var err error
	if t.InFlight != nil {
		// 合并命中时返回的是**别人刚翻好的**结果，provider 只能取全局最近值——
		// V1 记日志用的 `_last_provider` 也是这个语义（translator.py:439-445）。
		out, _, err = t.InFlight.Do(ctx, text, inFlightWait, call)
	} else {
		out, err = call(ctx)
	}
	if err != nil {
		return err.Error(), "error", "", ""
	}
	provider, model := t.lastUsed()
	return out, "llm", provider, model
}
