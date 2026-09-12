package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/providers"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/logger"
)

// ── 测试替身 ──

type fakeTranslator struct {
	lastText string
	stats    domain.Stats
}

func (f *fakeTranslator) TranslateText(text string) domain.RenderedMessage {
	f.lastText = text
	return domain.RenderedMessage{
		ID: "x", Original: text, Translated: "【译】" + text,
		Provider: "Mock", Model: "m", CacheState: "llm", Lang: "英语",
	}
}
func (f *fakeTranslator) TargetLanguage() string { return "zh-CN" }
func (f *fakeTranslator) Stats() domain.Stats    { return f.stats }

type fakeProviders struct {
	tested config.Provider
	// fetched 记录 FetchModels 收到的 Provider——当前没有用例读它；
	// 原先的用途「断言脱敏密钥被还原」已随哨兵机制删除。
	fetched config.Provider
	// models 是 FetchModels 的返回值，测试按需覆盖
	models providers.ModelList
}

func (f *fakeProviders) TestConnection(p config.Provider) (bool, string) {
	f.tested = p
	return true, "连通成功 — pong"
}

func (f *fakeProviders) FetchModels(_ context.Context, p config.Provider) providers.ModelList {
	f.fetched = p
	if f.models.Models == nil && f.models.Error == "" {
		// 默认给一个可用的结果，除非测试显式设定过。
		// 注意 **不能** 用 `!f.models.Success` 当判据：那样「成功但零模型」
		// 这个用例会被默认值悄悄覆盖掉，测出来永远是绿的。
		return providers.ModelList{Success: true, Models: []string{"m-a", "m-b"}}
	}
	return f.models
}

func (f *fakeProviders) Health() map[string]any {
	return map[string]any{"A": map[string]any{"failures": 0, "cooling": false}}
}

// ── 脚手架 ──

func newTestServer(t *testing.T) (*Server, *httptest.Server, *fakeTranslator, *fakeProviders, string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv(config.EnvDataDir, dataDir)

	resDir := t.TempDir()
	defaults := filepath.Join(resDir, "config.json")
	if err := os.WriteFile(defaults, []byte(`{"configVersion":1,"defaults":{
		"target_language":"zh-CN","max_messages":50,"send_hotkey":"shift+y",
		"llm_providers":[{"id":"p1","label":"DeepSeek","endpoint":"https://x/v1/chat/completions",
		"api_key":"sk-secret","model":"deepseek-chat","enabled":true,"timeout":8}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	presets := filepath.Join(resDir, "providers.json")
	if err := os.WriteFile(presets, []byte(`{"schemaVersion":1,"categories":[],"icons":{},"presets":[{"id":"deepseek","name":"DeepSeek"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ft := &fakeTranslator{}
	fp := &fakeProviders{}
	log := logger.New(filepath.Join(dataDir, "logs"))
	// LIFO：在 t.TempDir 的目录删除之前关闭日志文件句柄。
	// Windows 上文件被占用就删不掉——logger 包与这里的测试都踩过同一个坑。
	t.Cleanup(log.Close)

	s, err := New(Options{
		ConfigPath:  defaults,
		PresetsPath: presets,
		Log:         log,
		Translator:  ft,
		Providers:   fp,
	})
	if err != nil {
		t.Fatalf("创建 api 失败: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, ft, fp, dataDir
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// ── 接口测试 ──

func TestHealthEndpoint(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	status, body := getJSON(t, ts.URL+"/api/health")
	if status != 200 || body["ok"] != true {
		t.Fatalf("health 响应 = %d %v", status, body)
	}
	data := body["data"].(map[string]any)
	if data["targetLang"] != "zh-CN" {
		t.Errorf("targetLang = %v", data["targetLang"])
	}
	providers := data["providers"].(map[string]any)
	if providers["total"] != float64(1) || providers["enabled"] != float64(1) {
		t.Errorf("providers = %v", providers)
	}
	if _, ok := data["providerHealth"]; !ok {
		t.Error("health 应包含 providerHealth")
	}
}

// 关键：Provider 列表**绝不能泄露 API Key**。
func TestProvidersNeverLeakAPIKey(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 8192)
	n, _ := resp.Body.Read(raw)
	body := string(raw[:n])

	if strings.Contains(body, "sk-secret") {
		t.Fatal("响应里出现了 API Key —— 严重安全问题")
	}
	if !strings.Contains(body, `"hasKey":true`) {
		t.Errorf("应通过 hasKey 表明密钥已配置，实际响应: %s", body)
	}
	if !strings.Contains(body, "DeepSeek") {
		t.Errorf("应包含 Provider 标签，实际响应: %s", body)
	}
}

func TestConfigGetAndPut(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)
	changed := false
	s.opts.OnConfigChanged = func(cfg *config.Config) { changed = true }

	status, body := getJSON(t, ts.URL+"/api/config")
	if status != 200 || body["ok"] != true {
		t.Fatalf("GET config = %d %v", status, body)
	}
	cfg := body["data"].(map[string]any)
	if cfg["target_language"] != "zh-CN" {
		t.Errorf("target_language = %v", cfg["target_language"])
	}

	// PUT 新配置
	newCfg := map[string]any{
		"target_language": "en", "max_messages": 80,
		"send_hotkey": "shift+y", "llm_providers": []any{},
	}
	payload, _ := json.Marshal(newCfg)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/config", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT config 状态 = %d", resp.StatusCode)
	}
	if !changed {
		t.Error("PUT 后应触发 OnConfigChanged（用于热应用）")
	}
	if s.Config().TargetLanguage != "en" || s.Config().MaxMessages != 80 {
		t.Errorf("内存配置未更新: %+v", s.Config())
	}
}

// 预设的内容来自 resources/providers.json，但**必须包在统一信封里**。
//
// 这个用例原来检查的是顶层直接出现 schemaVersion（即裸响应），
// 那正是导致「点预设就崩」的写法——前端按 {ok,data} 解析，读到 ok 缺失
// 就进错误分支，再读 body.error.code 抛 TypeError。现已改为检查信封。
func TestPresetsServedFromResource(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	_, body := getJSON(t, ts.URL+"/api/presets")
	if body["ok"] != true {
		t.Fatalf("预设响应应带统一信封，实际: %v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 段: %v", body)
	}
	if data["schemaVersion"] == nil {
		t.Errorf("data 应保留 providers.json 的原始字段，实际: %v", data)
	}
	if _, has := data["presets"]; !has {
		t.Errorf("data 缺少 presets: %v", data)
	}
}

func TestLogsGetAndDelete(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	status, body := getJSON(t, ts.URL+"/api/logs")
	if status != 200 {
		t.Fatalf("GET logs = %d", status)
	}
	data := body["data"].(map[string]any)
	if _, ok := data["lines"]; !ok {
		t.Errorf("应返回 lines，实际: %v", data)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("DELETE logs 状态 = %d", resp.StatusCode)
	}
}

func TestTranslateEndpoint(t *testing.T) {
	_, ts, ft, _, _ := newTestServer(t)

	payload := `{"text":"hello world","mode":"receive"}`
	resp, err := http.Post(ts.URL+"/api/translate", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if body["ok"] != true {
		t.Fatalf("translate 响应 = %v", body)
	}
	data := body["data"].(map[string]any)
	if data["translation"] != "【译】hello world" {
		t.Errorf("translation = %v", data["translation"])
	}
	if data["cache"] != "llm" || data["detected"] != "英语" {
		t.Errorf("附加字段不正确: %v", data)
	}
	if ft.lastText != "hello world" {
		t.Errorf("引擎收到的文本 = %q", ft.lastText)
	}
}

func TestTranslateRejectsEmptyText(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/translate", "application/json", strings.NewReader(`{"text":"   "}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空文本应返回 400，实际 %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] == nil {
		t.Error("错误响应应包含 error 字段")
	}
}

func TestProviderTestEndpoint(t *testing.T) {
	_, ts, _, fp, _ := newTestServer(t)

	payload := `{"id":"p","label":"A","endpoint":"https://x/v1/chat/completions","api_key":"k","model":"m","enabled":true,"timeout":8}`
	resp, err := http.Post(ts.URL+"/api/providers/test", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	data := body["data"].(map[string]any)
	if data["success"] != true || !strings.Contains(data["message"].(string), "连通成功") {
		t.Errorf("连通测试结果 = %v", data)
	}
	if fp.tested.Label != "A" {
		t.Errorf("应把请求体解析成 Provider，实际 label = %q", fp.tested.Label)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("health 只应接受 GET，实际状态 = %d", resp.StatusCode)
	}
}

// ── SSE 事件推送 ──

func TestSSEDeliversPublishedEvents(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)

	// 第一帧应是 hello（告知连接已建立）
	first := readSSEFrame(t, reader)
	if first["type"] != "hello" {
		t.Errorf("首帧 type = %v，期望 hello", first["type"])
	}

	// 等服务端登记订阅者（handleEvents 在写 hello 之后才 subscribe 完成）
	deadline := time.Now().Add(2 * time.Second)
	for s.hub.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	s.Publish("message.translated", map[string]any{"speaker": "Player1", "translated": "你好"})

	second := readSSEFrame(t, reader)
	if second["type"] != "message.translated" {
		t.Fatalf("第二帧 type = %v，期望 message.translated", second["type"])
	}
	payload := second["payload"].(map[string]any)
	if payload["speaker"] != "Player1" || payload["translated"] != "你好" {
		t.Errorf("payload = %v", payload)
	}
}

// 没有订阅者时推送不应阻塞（前端未打开时后端照常工作）。
func TestPublishWithoutSubscribersDoesNotBlock(t *testing.T) {
	s, _, _, _, _ := newTestServer(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Publish("x", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("无订阅者时 Publish 阻塞了")
	}
}

// readSSEFrame 读一帧 `data: {...}\n\n`，跳过注释行（心跳）。
func readSSEFrame(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 SSE 帧失败: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行分隔符 / 心跳注释
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out); err != nil {
			t.Fatalf("SSE 数据不是合法 JSON: %q", line)
		}
		return out
	}
}
