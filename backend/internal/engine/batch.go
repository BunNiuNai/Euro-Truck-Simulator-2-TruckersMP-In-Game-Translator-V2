// 批量翻译 —— V1 消费循环里「攒批」那一段的等价物
// （translator.py:309-363 的 in_queue.get(timeout) + batch.append + _flush）。
//
// 为什么不能把 0.3 秒窗口塞进 Translator.Translate 里：
// main 的消费循环是**同步逐条**调用 Translate 的，第一条卡在翻译上时，
// 后面的消息根本还没被读出来，批次永远只有 1 条——看起来实现了批量，
// 实际一次都没合并过。V1 之所以能攒起来，是因为它的消费线程只负责
// 「从队列取 + 攒 + 到期冲刷」，取消息与翻译在同一个线程里交替进行。
//
// 所以这里照搬 V1 的形状：Run 独占 source，一边取一边攒，
// 成品从 results 出去，main 只负责消费成品。
//
// V1 的对应关系：
//
//	in_queue.get(timeout)  → select 里的 <-source
//	batch.append(msg)      → batch = append(batch, ev)
//	len(batch) >= 8        → b.maxSize
//	batch_deadline         → timer
//	_flush / _flush_llm    → flush
//	out_queue.put(...)     → results <-
package engine

import (
	"context"
	"strings"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// 与 V1 一致的批量参数。
const (
	// BatchWindow 对应 V1 translator.py:215 的 `BATCH_WINDOW = 0.3`。
	BatchWindow = 300 * time.Millisecond
	// BatchMaxSize 对应 V1 translator.py:360 的 `if len(batch) >= 8`。
	BatchMaxSize = 8
	// statsLogEvery 对应 V1 translator.py:372 的 `if self._msg_since_log >= 50`。
	statsLogEvery = 50
)

// Batcher 把短时间内到达的多条消息合并成一次 LLM 调用。
type Batcher struct {
	tr      *Translator
	window  time.Duration
	maxSize int

	// onStatsLog 每累计 statsLogEvery 条消息回调一次（V1 translator.py:371-377）。
	// 只由 Run 的协程调用，不需要加锁。
	onStatsLog func(domain.Stats)
	sinceLog   int
}

// NewBatcher 创建批量器。window/maxSize 传 0 表示用 V1 的默认值。
func NewBatcher(tr *Translator, window time.Duration, maxSize int) *Batcher {
	if window <= 0 {
		window = BatchWindow
	}
	if maxSize <= 0 {
		maxSize = BatchMaxSize
	}
	return &Batcher{tr: tr, window: window, maxSize: maxSize}
}

// SetStatsLogger 设置每 50 条消息的统计回调。
//
// ⚠️ 必须在 Run 之前调用（它只在 Run 的协程里被读，之后再改就是数据竞争）。
func (b *Batcher) SetStatsLogger(fn func(domain.Stats)) { b.onStatsLog = fn }

// Run 独占 source，把成品写进 results，直到 ctx 结束或 source 关闭。
//
// 它自己阻塞在翻译上没问题：main 那边只从 results 取，两边不会互相等死。
// 反过来如果让 main 循环去 Submit（把消息递进来），翻译期间 results 没人取，
// 中转满了就会变成「main 卡在递消息、批量器卡在递结果」的互锁。
func (b *Batcher) Run(
	ctx context.Context,
	source <-chan domain.Event,
	results chan<- domain.RenderedMessage,
) {
	var (
		batch    []domain.Event
		timer    *time.Timer
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	defer stopTimer()

	for {
		var deadline <-chan time.Time
		if timer != nil {
			deadline = timer.C
		}

		select {
		case <-ctx.Done():
			// V1 translator.py:323-326：收到 None（关闭）时把残余批次冲掉再退出。
			b.flush(ctx, batch, results)
			return

		case <-deadline:
			b.flush(ctx, batch, results)
			batch = nil
			stopTimer()

		case ev, ok := <-source:
			if !ok {
				b.flush(ctx, batch, results)
				return
			}

			// 自己的消息 / 缓存命中：**立刻**输出，不进批次。
			// V1 里这两类都在 `batch.append(msg)` 之前 continue 了
			// （translator.py:328-352）。
			if msg, done := b.tr.preTranslate(&ev); done {
				select {
				case results <- msg:
				case <-ctx.Done():
					return
				}
				continue
			}

			batch = append(batch, ev)
			if len(batch) == 1 {
				// 只在这批的**第一条**到达时起表——V1 translator.py:356-357
				// 的 `if batch_deadline is None` 是同一个意思。
				// 每条都重置的话，持续来消息就永远不会冲刷。
				timer = time.NewTimer(b.window)
			}
			if len(batch) >= b.maxSize {
				b.flush(ctx, batch, results)
				batch = nil
				stopTimer()
			}
		}
	}
}

// flush = V1 的 _flush + _flush_llm（translator.py:365-497）。
func (b *Batcher) flush(
	ctx context.Context, batch []domain.Event, results chan<- domain.RenderedMessage,
) {
	if len(batch) == 0 {
		return
	}

	b.sinceLog += len(batch)
	if b.sinceLog >= statsLogEvery {
		b.sinceLog = 0
		if b.onStatsLog != nil {
			b.onStatsLog(b.tr.Stats())
		}
	}

	// 冲刷时的 ctx 可能已经因为退出被取消。**不能**直接用 ctx：
	// V1 关闭时是把残余批次正常翻完再退出的（translator.py:323-326），
	// 用已取消的 ctx 会让最后几条消息全变成「context canceled」错误文案，
	// 用户看到一堆凭空出现的失败。
	flushCtx := context.WithoutCancel(ctx)

	if len(batch) == 1 {
		// 单条：走完整的混合语言拆分（_translate_with_mixed_lang）
		ev := batch[0]
		translated, state, provider, model := b.tr.translateOne(flushCtx, ev.Text)
		results <- b.tr.finish(&ev, translated, state, provider, model)
		return
	}

	sep := b.tr.batchSeparator()
	texts := make([]string, len(batch))
	for i := range batch {
		texts[i] = batch[i].Text
	}

	// ⚠️ 批量请求是**整段合并**后一次性走 callAPI（V1 translator.py:447-448），
	// 不逐条做混合语言拆分——拆分只发生在单条分支和下面的回退里。
	// 也就是说，一批里夹着的纯中文消息也会被一起送去翻译。
	// 这是 V1 的真实行为，不要「顺手修正」成逐条判断。
	out, state, provider, model := b.tr.callAPI(flushCtx, strings.Join(texts, sep))

	if state == "error" {
		// V1 translator.py:483-497：批量路径出错时**整批**都显示同一条错误文案。
		// 结果与 V1 一致，但少走一趟必然同样失败的逐条回退（V1 里
		// 逐条回退只在「调用成功但分隔符没回显」时发生，见下一段）。
		for i := range batch {
			ev := batch[i]
			results <- b.tr.finish(&ev, out, "error", "", "")
		}
		return
	}

	parts := strings.Split(out, sep)
	if len(parts) == len(batch) {
		// 拆分成功：按顺序分配（V1 translator.py:449-452）
		for i := range batch {
			ev := batch[i]
			results <- b.tr.finish(&ev, strings.TrimSpace(parts[i]), "llm", provider, model)
		}
		return
	}

	// 拆分失败：LLM 没按分隔符回显，逐条重新翻译。
	// 不这么做的话整段译文会全挂到第一条上，后面几条拿到的是别人的翻译
	// ——比翻译失败更坏，因为用户看不出错（V1 translator.py:453-462）。
	for i := range batch {
		ev := batch[i]
		translated, st, p, m := b.tr.translateOne(flushCtx, ev.Text)
		if st == "error" {
			// V1 translator.py:461-462 的 `except: translations[m.text] = m.text`
			// 回退成原文，并且**照常写缓存**（465 行在 try/except 之外）。
			// 状态单独取名而不是复用 "error"：finish 对 "error" 不写缓存
			// （缺陷 D14 的约束），而这里 V1 是写的。
			translated, st = ev.Text, "fallback"
		}
		results <- b.tr.finish(&ev, translated, st, p, m)
	}
}
