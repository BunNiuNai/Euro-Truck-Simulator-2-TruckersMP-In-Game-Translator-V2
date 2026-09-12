package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// failOnLLM 只对指定输入报错，其余正常翻译。
//
// fakeLLM 的错误是全局开关（要么全成功要么全失败），测不了
// 「一个片段失败、另一个成功」这种最需要覆盖的情况。
type failOnLLM struct {
	failOn string
	calls  []string
}

func (f *failOnLLM) Call(_ context.Context, text string) (string, string, string, error) {
	f.calls = append(f.calls, text)
	if text == f.failOn {
		return "", "", "", errors.New("[网络错误] 连接超时")
	}
	return "【译】" + text, "P", "m", nil
}

// V1 translator.py:413-418 —— 混合文本里某个外来片段翻译失败时，
// 回退成**原文片段**，而不是把失败文案塞进句子中间。
//
// ⚠️ 这条分支在 V2 里必须显式写：V1 是靠 try/except 兜住的，
// 而 V2 的 callAPI **不抛异常**——它把错误转成 `err.Error()` 文本加上
// "error" 状态返回。所以只要漏判这个状态，用户看到的就是
//
//	早上好 [网络错误] 连接超时
//
// 这比整条翻译失败更坏：整条失败时界面至少是「一条没翻出来」，
// 而夹在中间的失败文案看起来像模型翻出来的内容，用户不会去怀疑它。
func TestMixedSegmentFailureFallsBackToOriginal(t *testing.T) {
	llm := &fakeLLM{err: errors.New("[网络错误] 连接超时")}
	tr := newTranslator(t, llm, false)

	msg := tr.Translate(context.Background(), ev("早上好 hello"))

	if msg.Translated != "早上好 hello" {
		t.Errorf("片段翻译失败应整段回退原文，实际 %q", msg.Translated)
	}
	if strings.Contains(msg.Translated, "网络错误") {
		t.Errorf("失败文案不该出现在译文里，实际 %q", msg.Translated)
	}
}

// 一个片段失败、另一个成功时：失败的退原文，成功的使用译文，
// 目标语言片段原样保留。三者必须各归各位。
func TestMixedSegmentPartialFailureKeepsOthersTranslated(t *testing.T) {
	llm := &failOnLLM{failOn: "bad"}
	tr := newTranslator(t, llm, false)

	// 切成 [目标] [外来 bad] [目标] [外来 good]
	msg := tr.Translate(context.Background(), ev("你好 bad 世界 good"))

	got := msg.Translated
	if strings.Contains(got, "网络错误") {
		t.Fatalf("失败文案不该出现在译文里，实际 %q", got)
	}
	if !strings.Contains(got, "bad") {
		t.Errorf("失败片段应原样保留，实际 %q", got)
	}
	if !strings.Contains(got, "【译】good") {
		t.Errorf("成功片段应使用译文，实际 %q", got)
	}
	if !strings.Contains(got, "你好") || !strings.Contains(got, "世界") {
		t.Errorf("目标语言片段应原样保留，实际 %q", got)
	}
	// 只该请求两个外来片段，目标语言片段不进 LLM
	if len(llm.calls) != 2 {
		t.Errorf("应只翻译 2 个外来片段，实际调用 %d 次: %q", len(llm.calls), llm.calls)
	}
}

// 混合路径的缓存状态是 "mixed" 而不是 "error"——因为整条消息**有**产出
// （失败的片段退回了原文）。这一点影响缓存：V1 的 `_flush_llm` 会把它写进
// 缓存（translator.py:428），而 "error" 状态按缺陷 D14 是不写的。
func TestMixedPartialFailureIsCachedNotMarkedAsError(t *testing.T) {
	llm := &fakeLLM{err: errors.New("[网络错误] 连接超时")}
	tr := newTranslator(t, llm, true)

	first := tr.Translate(context.Background(), ev("早上好 hello"))
	if first.CacheState != "mixed" {
		t.Fatalf("混合路径的状态应为 mixed，实际 %q", first.CacheState)
	}

	// 第二次同一条消息应命中缓存，不再调 LLM
	before := llm.callCount()
	second := tr.Translate(context.Background(), ev("早上好 hello"))

	if second.CacheState != "hit" {
		t.Errorf("整条回退原文的结果应被缓存（V1 translator.py:428），实际状态 %q",
			second.CacheState)
	}
	if llm.callCount() != before {
		t.Errorf("缓存命中不该再调 LLM，实际新增 %d 次", llm.callCount()-before)
	}
	if second.Translated != first.Translated {
		t.Errorf("命中缓存的译文应与首次一致：%q vs %q", second.Translated, first.Translated)
	}
}

// 纯外来文本失败时是**整条** error（走 callAPI 的失败分支），
// 不要与上面的片段回退混为一谈：那条消息没有产出，必须能被上层看见
// 并用失败文案占位（V1 translator.py:483-497 整批同一条错误文案）。
func TestPureForeignFailureStaysError(t *testing.T) {
	llm := &fakeLLM{err: errors.New("[网络错误] 连接超时")}
	tr := newTranslator(t, llm, false)

	msg := tr.Translate(context.Background(), ev("hello world"))

	if msg.CacheState != "error" {
		t.Errorf("纯外来文本整条失败时状态应为 error，实际 %q", msg.CacheState)
	}
	if !strings.Contains(msg.Translated, "网络错误") {
		t.Errorf("整条失败时应带上失败原因，实际 %q", msg.Translated)
	}
	// Provider 不该在失败时被编造出来
	if msg.Provider != "" || msg.Model != "" {
		t.Errorf("失败时不该有 Provider/Model，实际 %q / %q", msg.Provider, msg.Model)
	}
}

// 混合文本里外来片段命中本地词典时，不走 LLM 也不该被当成失败。
// 这条守住「词典拦截发生在 callAPI 层，对片段同样生效」
// （V1 行为：`你好 wtf` 的 `wtf` 命中词典 → 零 API 调用）。
func TestMixedSegmentLocalDictHit(t *testing.T) {
	llm := &fakeLLM{}
	tr := newTranslator(t, llm, false)

	msg := tr.Translate(context.Background(), ev("你好 wtf"))

	if llm.callCount() != 0 {
		t.Fatalf("片段命中本地词典不该调 LLM，实际 %d 次", llm.callCount())
	}
	if !strings.Contains(msg.Translated, "什么鬼") {
		t.Errorf("片段应使用词典译文「什么鬼」，实际 %q", msg.Translated)
	}
	if msg.Translated != "你好什么鬼" {
		// 与 V1 一致：split 把空白并入前片段、reassemble 是 "".join，
		// 所以空格在结果里会消失。这不是 bug，见 batch_test.go 的同类断言。
		t.Errorf("重组结果应为「你好什么鬼」，实际 %q", msg.Translated)
	}
}

var _ = domain.Event{} // 保持 domain 导入（本文件只用它的字段类型）
