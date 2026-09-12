package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// 这个文件集中覆盖密钥回显与配置深拷贝。
//
// 为什么单独成文件：这两件事都属于「看起来能用、错起来很贵」的类型——
// 密钥泄露不会报错，浅拷贝污染也不会报错，只会在某天把用户密钥弄丢。

func putJSON(t *testing.T, url string, payload any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// 测试用配置里的密钥（见 newTestServer 写入的 defaults）
const testAPIKey = "sk-secret"

// GET /api/config 直接返回**真实密钥**——与 V1 一致。
//
// V1 里密钥就是配置里的一个普通字段：读出来、显示在设置窗口、保存时原样写回，
// 只在落盘那一刻加密（`config.py:333-361`）。V2 曾自创「`__UNCHANGED__` 哨兵 +
// 按 id 还原」，那套机制带来了一连串**只在它身上才可能发生**的缺陷：
//   · 设置页新建的 Provider 没有 id → 还原成空串 → 「测试说密钥无效」
//   · `byID[""]` 冲突 → 两个 Provider 共用同一个密钥 → 「所有密钥都失效」
//   · 前端把哨兵当空值 → 界面显示「尚未配置」，与实际相反
// 已整套删除。这条测试钉住「不要再加回来」。
func TestGetConfigReturnsRealKey(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	_, body := getJSON(t, ts.URL+"/api/config")
	cfg := body["data"].(map[string]any)

	providers, ok := cfg["llm_providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("配置里应有 Provider，实际 %v", cfg["llm_providers"])
	}
	if got := providers[0].(map[string]any)["api_key"]; got != testAPIKey {
		t.Errorf("GET 应原样返回密钥 %q，实际 %v", testAPIKey, got)
	}
}

// 前端最常见的动作是「改个字号然后保存」：它把整份配置（含真实密钥）回传。
// 往返之后密钥必须一字不差——既不能被写空，也不能变成别的东西。
func TestPutConfigKeepsKeyRoundTrip(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	_, body := getJSON(t, ts.URL+"/api/config")
	cfg := body["data"].(map[string]any)
	cfg["font_size"] = float64(16)

	status, _ := putJSON(t, ts.URL+"/api/config", cfg)
	if status != 200 {
		t.Fatalf("PUT 状态码 = %d", status)
	}

	after := s.Config()
	if after.LLMProviders[0].APIKey != testAPIKey {
		t.Errorf("往返之后密钥变成 %q，期望 %q", after.LLMProviders[0].APIKey, testAPIKey)
	}
	if after.FontSize != 16 {
		t.Errorf("其它字段未保存：font_size = %d", after.FontSize)
	}
}

// 用户填了新密钥就该更新。
func TestPutConfigUpdatesKey(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	_, body := getJSON(t, ts.URL+"/api/config")
	cfg := body["data"].(map[string]any)
	cfg["llm_providers"].([]any)[0].(map[string]any)["api_key"] = "sk-brand-new"

	status, _ := putJSON(t, ts.URL+"/api/config", cfg)
	if status != 200 {
		t.Fatalf("PUT 状态码 = %d", status)
	}

	if got := s.Config().LLMProviders[0].APIKey; got != "sk-brand-new" {
		t.Errorf("密钥 = %q，期望 sk-brand-new", got)
	}
}

// 用户清空密钥（空串）应当真的清空。
// 这是「空串即清空」最容易搞错的地方，所以单独测。
func TestPutConfigClearsKey(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	_, body := getJSON(t, ts.URL+"/api/config")
	cfg := body["data"].(map[string]any)
	cfg["llm_providers"].([]any)[0].(map[string]any)["api_key"] = ""

	status, _ := putJSON(t, ts.URL+"/api/config", cfg)
	if status != 200 {
		t.Fatalf("PUT 状态码 = %d", status)
	}

	if got := s.Config().LLMProviders[0].APIKey; got != "" {
		t.Errorf("密钥 = %q，期望空串", got)
	}
}

// Config() 必须返回深拷贝：调用方改了返回值不能影响服务器本体。
// 浅拷贝会让调用方的改动直接落到内存里的真实配置上。
func TestConfigReturnsDeepCopy(t *testing.T) {
	s, _, _, _, _ := newTestServer(t)

	snap := s.Config()
	snap.TargetLanguage = "tampered"
	snap.LLMProviders[0].APIKey = "tampered"
	snap.AdMessages = append(snap.AdMessages, "tampered")

	again := s.Config()
	if again.TargetLanguage == "tampered" {
		t.Error("TargetLanguage 被污染：Config() 不是深拷贝")
	}
	if again.LLMProviders[0].APIKey == "tampered" {
		t.Error("LLMProviders 被污染：切片与本体共享底层数组")
	}
	for _, m := range again.AdMessages {
		if m == "tampered" {
			t.Error("AdMessages 被污染：切片与本体共享底层数组")
		}
	}
}

// 反复 GET 之后服务器上的密钥必须完好——这是「GET 一次配置就丢密钥」那个事故的回归测试。
func TestGetConfigDoesNotCorruptServerState(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	// 先 GET 若干次
	for i := 0; i < 3; i++ {
		getJSON(t, ts.URL+"/api/config")
	}

	if got := s.Config().LLMProviders[0].APIKey; got != testAPIKey {
		t.Fatalf("GET /api/config 污染了服务器配置：密钥变成 %q", got)
	}
}

// Provider 的两个 map 字段也必须是深拷贝，否则改一个 Provider 会串到别处。
func TestConfigDeepCopiesProviderMaps(t *testing.T) {
	s, _, _, _, _ := newTestServer(t)

	snap := s.Config()
	if snap.LLMProviders[0].ExtraHeaders == nil {
		// 测试配置没给 map，补一个再验证
		snap.LLMProviders[0].ExtraHeaders = map[string]string{"X-Test": "1"}
		inner := s.Config()
		if len(inner.LLMProviders[0].ExtraHeaders) != 0 {
			t.Error("给快照的 Provider 加 map 影响到了本体")
		}
		return
	}
	snap.LLMProviders[0].ExtraHeaders["X-Test"] = "1"
	if _, ok := s.Config().LLMProviders[0].ExtraHeaders["X-Test"]; ok {
		t.Error("ExtraHeaders 被污染：map 与本体共享引用")
	}
}
