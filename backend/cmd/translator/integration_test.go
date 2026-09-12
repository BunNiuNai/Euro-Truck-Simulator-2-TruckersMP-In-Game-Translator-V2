package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/engine"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/providers"
)

// 端到端集成：真实 HTTP（httptest）→ providers → engine → 词库 → 成品消息。
//
// 这个测试是「V2 能真的翻译」的凭证，同时验证一条关键性能属性：
// **纯中文与词典命中的消息绝不打网络**（V1 基线里这类占 42%）。

type mockLLM struct {
	server *httptest.Server
	hits   int32
	reply  string
	echo   bool // true 时把原文回显进译文（便于断言「哪一段走了网络」）
}

// newMockLLM 回显原文（用于断言片段是否走了网络）。
func newMockLLM(t *testing.T, reply string) *mockLLM { return newMock(t, reply, true) }

// newMockLLMPlain 只返回固定译文，便于触发「输出里没有 @玩家名」等分支。
func newMockLLMPlain(t *testing.T, reply string) *mockLLM { return newMock(t, reply, false) }

func newMock(t *testing.T, reply string, echo bool) *mockLLM {
	t.Helper()
	m := &mockLLM{reply: reply, echo: echo}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.hits, 1)
		body, _ := io.ReadAll(r.Body)

		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Temperature float64 `json:"temperature"`
			MaxTokens   int     `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(400)
			return
		}
		// 校验请求形状与 V1 一致
		if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"messages 结构不符合 V1 约定"}}`)
			return
		}
		if payload.Temperature != 0 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"temperature 应为 0"}}`)
			return
		}
		translated := m.reply
		if m.echo {
			translated += "[" + payload.Messages[1].Content + "]"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": translated}},
			},
		})
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockLLM) count() int { return int(atomic.LoadInt32(&m.hits)) }

func buildTranslator(t *testing.T, mock *mockLLM) *engine.Translator {
	t.Helper()
	resDir := findResourcesDir()
	dict, err := dictionary.Load(filepath.Join(resDir, "dictionary.json"))
	if err != nil {
		t.Fatalf("加载词库失败: %v", err)
	}

	client := providers.New(providers.Options{
		Providers: []config.Provider{{
			ID: "mock", Label: "Mock", Endpoint: mock.server.URL,
			APIKey: "sk-test", Model: "mock-model", Enabled: true,
			APIFormat: "openai", Timeout: 5,
		}},
		Target: "zh-CN",
		SystemPrompt: func(target string) string {
			return engine.ReceiveSystemPrompt(target, dict.PromptMapping())
		},
	})

	return &engine.Translator{
		Target:         "zh-CN",
		BatchSeparator: "\n---\n",
		Dict:           dict,
		LLM:            providerLLM{c: client},
	}
}

func translate(t *testing.T, tr *engine.Translator, text string) domain.RenderedMessage {
	t.Helper()
	return tr.Translate(context.Background(), &domain.Event{
		ID: text, Text: text, Speaker: "Tester", Origin: "test",
	})
}

func TestEndToEndLocalHitNeverCallsNetwork(t *testing.T) {
	mock := newMockLLM(t, "【译】")
	tr := buildTranslator(t, mock)

	cases := []struct {
		text  string
		want  string
		state string
	}{
		{"wtf", "什么鬼", "local_dict"},              // 词典命中
		{"rec ban", "已录屏，等封禁", "local_dict"},     // 结构化短语
		{"truck", "卡车", "local_dict"},              // ETS2 词汇
		{"...", "...", "skip_nontext"},              // 非文字
		{"你好世界", "你好世界", "all_target"},           // 纯中文（split 短路，不走 skip_target）
		{"thank youuu", "谢谢", "local_dict"},        // 压缩重复字母后命中
	}
	for _, c := range cases {
		msg := translate(t, tr, c.text)
		if msg.Translated != c.want {
			t.Errorf("%q → %q，期望 %q", c.text, msg.Translated, c.want)
		}
		if msg.CacheState != c.state {
			t.Errorf("%q 的状态 = %q，期望 %q", c.text, msg.CacheState, c.state)
		}
	}

	if mock.count() != 0 {
		t.Errorf("本地可处理的消息打了 %d 次网络 —— 这会让 42%% 的零调用率失效", mock.count())
	}
}

func TestEndToEndForeignGoesToLLM(t *testing.T) {
	mock := newMockLLM(t, "【译】")
	tr := buildTranslator(t, mock)

	msg := translate(t, tr, "why did you ram me")
	if msg.CacheState != "llm" {
		t.Fatalf("状态 = %q，期望 llm", msg.CacheState)
	}
	if !strings.HasPrefix(msg.Translated, "【译】") {
		t.Errorf("译文 = %q，应来自 mock LLM", msg.Translated)
	}
	// 出口后处理生效：mock 回显里的 "ram" 是俚语 token，
	// 会被 V1 的 fix_leftover_shorthand 补译成 "撞人，"。
	// 真实场景下 LLM 输出是中文、没有残留英文，这一步通常不触发。
	if !strings.Contains(msg.Translated, "撞人") {
		t.Errorf("译文 = %q，出口后处理应把残留的 ram 补译成 撞人", msg.Translated)
	}
	if mock.count() != 1 {
		t.Errorf("网络调用次数 = %d，期望 1", mock.count())
	}
	if msg.Lang != "英语" {
		t.Errorf("检测语言 = %q，期望 英语", msg.Lang)
	}
}

// 出口后处理链：补译残留缩写 → 保留 @玩家名。
func TestEndToEndPostProcessing(t *testing.T) {
	mock := newMockLLMPlain(t, "好的")
	tr := buildTranslator(t, mock)

	msg := translate(t, tr, "hello there")
	if msg.CacheState != "llm" {
		t.Fatalf("状态 = %q，期望 llm", msg.CacheState)
	}
	if msg.Translated != "好的" {
		t.Errorf("译文 = %q，期望 好的", msg.Translated)
	}

	// @玩家名 前缀保留：LLM 输出里**没有**该前缀时才补上
	// （V1 preserve_mention_prefix 的语义）
	msg2 := translate(t, tr, "@Player123 hello there")
	if !strings.HasPrefix(msg2.Translated, "@Player123") {
		t.Errorf("译文 = %q，@玩家名 前缀必须保留", msg2.Translated)
	}
	if mock.count() != 2 {
		t.Errorf("网络调用次数 = %d，期望 2", mock.count())
	}
}

// 出口后处理会补译 LLM 漏译的缩写（V1 fix_leftover_shorthand）。
func TestEndToEndFixesLeftoverShorthand(t *testing.T) {
	mock := newMockLLMPlain(t, "wtf 你在干嘛")
	tr := buildTranslator(t, mock)

	msg := translate(t, tr, "hello there")
	if !strings.Contains(msg.Translated, "什么鬼") {
		t.Errorf("译文 = %q，残留的 wtf 应被补译成 什么鬼", msg.Translated)
	}
}

// 混合消息：中文原样保留，只有外文片段走网络。
func TestEndToEndMixedLanguage(t *testing.T) {
	mock := newMockLLM(t, "【译】")
	tr := buildTranslator(t, mock)

	msg := translate(t, tr, "你好 where are you 我的朋友")
	if msg.CacheState != "mixed" {
		t.Fatalf("状态 = %q，期望 mixed", msg.CacheState)
	}
	if !strings.Contains(msg.Translated, "你好") || !strings.Contains(msg.Translated, "我的朋友") {
		t.Errorf("译文 = %q，中文片段必须原样保留", msg.Translated)
	}
	if !strings.Contains(msg.Translated, "【译】") {
		t.Errorf("译文 = %q，外文片段应被翻译", msg.Translated)
	}
	if mock.count() != 1 {
		t.Errorf("网络调用次数 = %d，期望 1（只翻译外文片段，且合并）", mock.count())
	}
}

// 词库命中发生在片段层：混合消息里的外文片段若命中词典，同样不打网络。
func TestEndToEndDictHitInsideMixedSegment(t *testing.T) {
	mock := newMockLLM(t, "【译】")
	tr := buildTranslator(t, mock)

	msg := translate(t, tr, "你好 wtf")
	if msg.Translated != "你好什么鬼" {
		t.Errorf("译文 = %q，期望 你好什么鬼（片段级词典拦截）", msg.Translated)
	}
	if mock.count() != 0 {
		t.Errorf("网络调用次数 = %d，期望 0（片段命中词典）", mock.count())
	}
}

// 全部 Provider 失败时，用户看到的文案必须与 V1 一致。
func TestEndToEndAllProvidersFailMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	resDir := findResourcesDir()
	dict, err := dictionary.Load(filepath.Join(resDir, "dictionary.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := providers.New(providers.Options{
		Providers: []config.Provider{{
			ID: "bad", Label: "Bad", Endpoint: srv.URL, APIKey: "k",
			Model: "m", Enabled: true, Timeout: 2,
		}},
		Target: "zh-CN",
	})
	tr := &engine.Translator{
		Target: "zh-CN", BatchSeparator: "\n---\n",
		Dict: dict, LLM: providerLLM{c: client},
	}

	msg := translate(t, tr, "why did you ram me")
	if msg.CacheState != "error" {
		t.Errorf("状态 = %q，期望 error", msg.CacheState)
	}
	if msg.Translated != "[翻译失败] 所有 Provider 翻译失败" {
		t.Errorf("文案 = %q，应与 V1 逐字一致", msg.Translated)
	}
}
