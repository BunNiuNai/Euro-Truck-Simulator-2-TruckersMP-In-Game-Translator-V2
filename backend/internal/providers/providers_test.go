package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// openAIServer 起一个返回 OpenAI 格式响应的测试服务，并记录收到的请求体。
func openAIServer(t *testing.T, reply string, delay time.Duration) (*httptest.Server, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func provider(label, endpoint string) config.Provider {
	return config.Provider{
		ID: label, Label: label, Endpoint: endpoint, APIKey: "sk-test",
		Model: "test-model", Enabled: true, APIFormat: "openai", Timeout: 5,
	}
}

func newClient(ps ...config.Provider) *Client {
	return New(Options{
		Providers: ps,
		Target:    "zh-CN",
		SystemPrompt: func(target string) string {
			return "You translate into " + target + ". Output only the translation."
		},
	})
}

// ── 基础调用 ──

func TestOpenAICallSuccessAndPayload(t *testing.T) {
	srv, got := openAIServer(t, "你好世界", 0)
	c := newClient(provider("A", srv.URL))

	res, err := c.Translate(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if res.Translated != "你好世界" {
		t.Errorf("译文 = %q", res.Translated)
	}
	if res.Provider != "A" || res.Model != "test-model" {
		t.Errorf("Provider/Model = %q/%q", res.Provider, res.Model)
	}

	var payload map[string]any
	if err := json.Unmarshal(*got, &payload); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	// 与 V1 一致：system + user 两条消息、temperature=0、动态 max_tokens
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数量 = %d，期望 2（system + user）", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(fmt.Sprint(first["content"]), "zh-CN") {
		t.Errorf("system 消息不正确: %v", first)
	}
	if payload["temperature"] != float64(0) {
		t.Errorf("temperature = %v，期望 0", payload["temperature"])
	}
	// "hello world" 11 字符 → 56 + 11/4 = 58 → clamp 到 64
	if payload["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v，期望 64", payload["max_tokens"])
	}
}

// 缺陷 D13 的修正验证：anthropic 必须用顶层 system + content[0].text。
func TestAnthropicFormatFixed(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("缺少 x-api-key 头")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("缺少 anthropic-version 头")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"你好世界"}]}`))
	}))
	defer srv.Close()

	p := provider("Claude", srv.URL)
	p.APIFormat = "anthropic"
	c := newClient(p)

	res, err := c.Translate(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("anthropic 格式翻译失败（V1 在此处必然失败）: %v", err)
	}
	if res.Translated != "你好世界" {
		t.Errorf("译文 = %q，期望 你好世界", res.Translated)
	}

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	// 顶层 system 字段（V1 错放在 messages 里，Anthropic 会 400 拒绝）
	if payload["system"] == nil {
		t.Error("anthropic 请求缺少顶层 system 字段")
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("anthropic messages 数量 = %d，期望 1（只有 user）", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("anthropic 的 messages 里不应有 system 角色")
	}
}

// ── 竞速 ──

func TestRaceFastestWins(t *testing.T) {
	slow, slowGot := openAIServer(t, "慢速译文", 300*time.Millisecond)
	fast, _ := openAIServer(t, "快速译文", 0)
	_ = slowGot

	c := newClient(provider("Slow", slow.URL), provider("Fast", fast.URL))

	start := time.Now()
	res, err := c.Translate(context.Background(), "hello world")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("竞速失败: %v", err)
	}
	if res.Provider != "Fast" {
		t.Errorf("胜出者 = %q，期望 Fast（竞速应取先返回的）", res.Provider)
	}
	// 应远快于慢速 provider 的 300ms
	if elapsed > 200*time.Millisecond {
		t.Errorf("耗时 %v —— 没有并发竞速（看起来在串行等待）", elapsed)
	}
}

// 竞速跳过「无效结果」：返回原文的 provider 不算成功，应等下一家。
func TestRaceSkipsInvalidResult(t *testing.T) {
	// 这家把原文原样返回 → LooksUntranslated 判定失败
	invalid, _ := openAIServer(t, "hello world", 0)
	valid, _ := openAIServer(t, "你好世界", 100*time.Millisecond)

	c := newClient(provider("Invalid", invalid.URL), provider("Valid", valid.URL))

	res, err := c.Translate(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("应回退到有效的那家: %v", err)
	}
	if res.Provider != "Valid" {
		t.Errorf("胜出者 = %q，期望 Valid（Invalid 返回原文应被跳过）", res.Provider)
	}
	if res.Translated != "你好世界" {
		t.Errorf("译文 = %q", res.Translated)
	}
}

func TestRaceAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	c := newClient(provider("A", srv.URL), provider("B", srv.URL))

	_, err := c.Translate(context.Background(), "hello world")
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	// V1 的真实文案：_call_api_internal 抛通用异常 → _format_error 包成
	// "[翻译失败] 所有 Provider 翻译失败"。具体错误码只进日志。
	if err.Error() != "[翻译失败] 所有 Provider 翻译失败" {
		t.Errorf("错误文案 = %q，应与 V1 逐字一致", err.Error())
	}
}

func TestNoEnabledProvider(t *testing.T) {
	p := provider("A", "http://127.0.0.1:1")
	p.Enabled = false
	c := newClient(p)

	_, err := c.Translate(context.Background(), "hi")
	if err == nil {
		t.Fatal("没有启用 Provider 时应报错")
	}
	if !strings.Contains(err.Error(), "未配置任何启用的 Provider") {
		t.Errorf("错误文案 = %q", err.Error())
	}
}

// 具体错误码在 callProvider 层确定（映射发生在这里），
// 竞速层则统一包装成 V1 的通用文案。
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   domain.ErrorCode
		substr string
	}{
		{401, domain.ErrAuthFailed, "[认证失败] API 密钥无效，请检查设置"},
		{403, domain.ErrForbidden, "[权限不足] 无权访问该 API，请检查密钥权限"},
		{429, domain.ErrRateLimited, "[请求过于频繁] 请稍后重试"},
		{500, domain.ErrServerError, "[服务器错误 500] API 服务器异常，请稍后重试"},
		{502, domain.ErrServerError, "[服务器错误 502]"},
		{503, domain.ErrServerError, "[服务器错误 503]"},
		{404, domain.ErrHTTPError, "[HTTP 错误 404"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":{"message":"detail-msg"}}`))
			}))
			defer srv.Close()

			c := newClient(provider("A", srv.URL))
			_, err := c.callProvider(context.Background(), provider("A", srv.URL), "hello world", "zh-CN")
			if err == nil {
				t.Fatal("应报错")
			}
			if code := AsError(err).Code; code != tc.want {
				t.Errorf("错误码 = %s，期望 %s", code, tc.want)
			}
			if !strings.Contains(err.Error(), tc.substr) {
				t.Errorf("错误文案 = %q，期望包含 %q", err.Error(), tc.substr)
			}
		})
	}
}

func TestBadResponseMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`这不是 JSON`))
	}))
	defer srv.Close()

	c := newClient(provider("A", srv.URL))
	_, err := c.callProvider(context.Background(), provider("A", srv.URL), "hello world", "zh-CN")
	if err == nil {
		t.Fatal("应报错")
	}
	if code := AsError(err).Code; code != domain.ErrBadResponse {
		t.Errorf("错误码 = %s，期望 BAD_RESPONSE", code)
	}
	if !strings.Contains(err.Error(), "[响应格式错误]") {
		t.Errorf("错误文案 = %q，期望包含 [响应格式错误]", err.Error())
	}
}

// 响应缺少 choices[0]（V1 会 KeyError/IndexError → 响应格式错误）
func TestMissingChoicesMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","object":"chat.completion"}`))
	}))
	defer srv.Close()

	c := newClient(provider("A", srv.URL))
	_, err := c.callProvider(context.Background(), provider("A", srv.URL), "hello world", "zh-CN")
	if code := AsError(err).Code; code != domain.ErrBadResponse {
		t.Errorf("错误码 = %s，期望 BAD_RESPONSE", code)
	}
}

func TestTimeoutMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	p := provider("A", srv.URL)
	p.Timeout = 1 // 1 秒超时
	c := newClient(p)

	start := time.Now()
	_, err := c.callProvider(context.Background(), p, "hello world", "zh-CN")
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Errorf("耗时 %v —— Provider 自带的 timeout 字段没生效", elapsed)
	}
	if code := AsError(err).Code; code != domain.ErrTimeout {
		t.Errorf("错误码 = %s，期望 TIMEOUT", code)
	}
	if !strings.Contains(err.Error(), "[请求超时]") {
		t.Errorf("错误文案 = %q，期望 [请求超时]", err.Error())
	}
}

// ── 熔断 ──

func TestBreakerOpensAfter3Failures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := newClient(provider("A", srv.URL))

	// 连续 3 次失败 → 进入冷却
	for i := 0; i < 3; i++ {
		_, _ = c.Translate(context.Background(), "hello world")
	}
	if !c.isCooling("A") {
		t.Fatal("连续 3 次失败后应进入冷却")
	}

	// 冷却中：应被过滤（但因为没有其他可用 Provider，会强制重试全部 —— V1 行为）
	before := atomic.LoadInt32(&hits)
	_, _ = c.Translate(context.Background(), "hello world")
	after := atomic.LoadInt32(&hits)
	if after <= before {
		t.Error("全部冷却时应强制重试（V1 行为），而不是谁都不试")
	}
}

// 成功应立即清零失败计数。
func TestBreakerResetsOnSuccess(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"你好"}}]}`))
	}))
	defer srv.Close()

	c := newClient(provider("A", srv.URL))
	for i := 0; i < 2; i++ {
		_, _ = c.Translate(context.Background(), "hello world")
	}
	fail.Store(false)
	if _, err := c.Translate(context.Background(), "hello world"); err != nil {
		t.Fatalf("恢复后应成功: %v", err)
	}

	c.mu.Lock()
	h := *c.health["A"]
	c.mu.Unlock()
	if h.Failures != 0 || !h.CoolUntil.IsZero() {
		t.Errorf("成功后应清零熔断状态，得到 %+v", h)
	}
}

// 冷却中的 Provider 应被跳过，让健康的顶上。
func TestCoolingProviderSkipped(t *testing.T) {
	var badHits int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badHits, 1)
		w.WriteHeader(500)
	}))
	defer bad.Close()
	good, _ := openAIServer(t, "你好世界", 0)

	c := newClient(provider("Bad", bad.URL), provider("Good", good.URL))

	// 直接驱动熔断，不依赖竞速时序（否则测试会不稳定）
	for i := 0; i < 3; i++ {
		c.noteResult("Bad", false)
	}
	if !c.isCooling("Bad") {
		t.Fatal("连续 3 次失败后应进入冷却")
	}

	// 冷却期间调用竞速：Bad 不应被调用，Good 顶上
	before := atomic.LoadInt32(&badHits)
	if _, err := c.Translate(context.Background(), "hello world"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&badHits); got != before {
		t.Errorf("冷却中的 Provider 被调用了 %d 次", got-before)
	}
}

// ── 13 字段配置的有效性 ──

func TestThinkingDisabledForDeepSeek(t *testing.T) {
	srv, got := openAIServer(t, "你好", 0)
	p := provider("DeepSeek", srv.URL)
	p.Model = "deepseek-chat"
	c := newClient(p)

	if _, err := c.Translate(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(*got, &payload)
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Errorf("DeepSeek 应关闭 thinking，payload.thinking = %v", payload["thinking"])
	}
}

func TestExtraHeadersAndBodyApplied(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Write([]byte(`{"choices":[{"message":{"content":"你好"}}]}`))
	}))
	defer srv.Close()

	p := provider("Custom", srv.URL)
	p.ExtraHeaders = map[string]string{
		"X-Custom-Key": "prefix-{api_key}",
		"X-Static":     "fixed",
	}
	p.ExtraBody = map[string]any{"temperature": 0.7, "top_p": 0.9}
	c := newClient(p)

	if _, err := c.Translate(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	// {api_key} 模板替换
	if v := hdr.Get("X-Custom-Key"); v != "prefix-sk-test" {
		t.Errorf("X-Custom-Key = %q，期望 prefix-sk-test（{api_key} 未替换）", v)
	}
	if hdr.Get("X-Static") != "fixed" {
		t.Errorf("X-Static 未生效")
	}
	// extra_body 覆盖基础 payload（V1: payload.update(extra_body)）
	if got["temperature"] != 0.7 || got["top_p"] != 0.9 {
		t.Errorf("extra_body 未覆盖基础 payload: %v", got)
	}
}

func TestEndpointWithoutScheme(t *testing.T) {
	// 无 http:// 前缀时应自动补 https://（V1 行为）。
	// 这里用 127.0.0.1:1 保证连接失败，只验证不发 panic 且报网络错误。
	p := provider("A", "127.0.0.1:1/v1/chat/completions")
	c := newClient(p)
	_, err := c.Translate(context.Background(), "hello")
	if err == nil {
		t.Fatal("连接不可达应报错")
	}
}

func TestEmptyEndpointDoesNotPanic(t *testing.T) {
	p := provider("A", "")
	c := newClient(p)
	_, err := c.Translate(context.Background(), "hello")
	if err == nil {
		t.Fatal("空 endpoint 应报错")
	}
}

// ── 模型列表 ──

func TestFetchModelsParsesAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ⚠️ 必须断言路径**完全相等**，不能只判 HasSuffix：
		//    `/v1/v1/models` 同样以 `/v1/models` 结尾——后缀剥离顺序那个 bug
		//    （V1 缺陷 D21）当初就是从这个太宽松的断言底下溜过去的。
		if r.URL.Path != "/v1/models" {
			t.Errorf("请求路径 = %q，期望恰好 /v1/models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[
			{"id":"gpt-4o-preview"},
			{"id":"deepseek-chat"},
			{"id":"alpha-model"},
			{"id":"gpt-4o-mini"}
		]}`))
	}))
	defer srv.Close()

	// endpoint 带 /v1/chat/completions 后缀，应被剥掉后再拼 /v1/models
	p := provider("A", srv.URL+"/v1/chat/completions")
	c := newClient(p)

	res := c.FetchModels(context.Background(), p)
	if !res.Success {
		t.Fatalf("拉取失败: %s", res.Error)
	}
	if len(res.Models) != 4 {
		t.Fatalf("模型数 = %d，期望 4: %v", len(res.Models), res.Models)
	}
	// 非 preview/beta/alpha 的排前面（V1 sort_key）
	if res.Models[0] != "deepseek-chat" {
		t.Errorf("首位 = %q，期望 deepseek-chat（预览版应排后面）: %v", res.Models[0], res.Models)
	}
}

func TestFetchModelsHandlesStringItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":["model-a","model-b"]}`))
	}))
	defer srv.Close()

	p := provider("A", srv.URL)
	c := newClient(p)
	res := c.FetchModels(context.Background(), p)
	if !res.Success || len(res.Models) != 2 {
		t.Errorf("字符串形态的 data 应被支持，得到 %+v", res)
	}
}

// ── 连通性测试 ──

func TestTestConnectionSuccess(t *testing.T) {
	srv, _ := openAIServer(t, "pong", 0)
	p := provider("A", srv.URL)
	c := newClient(p)

	res := c.TestConnection(context.Background(), p)
	if !res.Success {
		t.Fatalf("连通测试应成功: %s", res.Message)
	}
	if !strings.Contains(res.Message, "连通成功") {
		t.Errorf("文案 = %q", res.Message)
	}
}

func TestTestConnectionAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	p := provider("A", srv.URL)
	c := newClient(p)
	res := c.TestConnection(context.Background(), p)
	if res.Success {
		t.Fatal("401 应判定为失败")
	}
	if !strings.Contains(res.Message, "API Key 无效 (401)") {
		t.Errorf("文案 = %q，应与 V1 一致", res.Message)
	}
}

func TestTestConnectionNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	p := provider("A", srv.URL)
	c := newClient(p)
	res := c.TestConnection(context.Background(), p)
	if !strings.Contains(res.Message, "未找到 (404)") {
		t.Errorf("文案 = %q，应与 V1 一致（含提示检查路径）", res.Message)
	}
}

// ── token 计算 ──

func TestMaxOutputTokens(t *testing.T) {
	if got := MaxOutputTokens("hi", "\n---\n"); got != 64 {
		t.Errorf("短文本 = %d，期望 64", got)
	}
	long := strings.Repeat("字", 500)
	if got := MaxOutputTokens(long, "\n---\n"); got != 160 {
		t.Errorf("长文本 = %d，期望 160", got)
	}
	if got := MaxOutputTokens("a\n---\nb\n---\nc", "\n---\n"); got != 1500 {
		t.Errorf("批处理 = %d，期望 1500", got)
	}
}
