package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 这个文件锁住一条契约：**每个 /api/* 响应都必须带统一信封**。
//
// 为什么值得单独成文件：曾经 /api/presets 直接把 providers.json 的文件字节
// 写出去，没有 {ok, data} 外套。后端测试当时全绿——因为根本没测这个端点；
// 而前端按统一契约解析，读到 body.ok 是 undefined 就进了错误分支，
// 再去读 body.error.code 就抛 TypeError，表现为「点预设就崩」。
// 契约不一致被伪装成了前端崩溃，排查成本远高于写这个测试。

// envelopeOK 断言一个 GET 端点返回 {ok:true, data:…}。
func assertEnvelopeOK(t *testing.T, base, path string) map[string]any {
	t.Helper()

	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应: %v", path, err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("%s 的响应不是 JSON 对象: %v\n%s", path, err, truncate(raw))
	}

	if _, ok := top["ok"]; !ok {
		t.Fatalf("%s 的响应缺少 ok 字段（后端未使用统一信封）——\n"+
			"前端会在 body.error.code 上抛 TypeError。响应片段:\n%s", path, truncate(raw))
	}

	var okVal bool
	_ = json.Unmarshal(top["ok"], &okVal)
	if !okVal {
		t.Fatalf("%s 返回 ok=false（本用例期望成功）: %s", path, truncate(raw))
	}
	if _, hasData := top["data"]; !hasData {
		t.Fatalf("%s 的 ok=true 响应缺少 data 字段: %s", path, truncate(raw))
	}

	var data map[string]any
	_ = json.Unmarshal(top["data"], &data)
	return data
}

func truncate(b []byte) string {
	const n = 300
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// 所有 GET 端点都必须使用统一信封。
func TestAllGetEndpointsUseEnvelope(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	for _, path := range []string{
		"/api/health",
		"/api/config",
		"/api/providers",
		"/api/presets",
		"/api/logs",
		"/api/stats",
	} {
		assertEnvelopeOK(t, ts.URL, path)
	}
}

// /api/presets 曾经是裸响应；这个用例盯住它的内容仍然可用。
func TestPresetsAreWrappedAndUsable(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	data := assertEnvelopeOK(t, ts.URL, "/api/presets")

	presets, ok := data["presets"].([]any)
	if !ok {
		t.Fatalf("presets 不是数组: %T", data["presets"])
	}
	if len(presets) == 0 {
		t.Fatal("presets 为空——测试用的 providers.json 里写了一个预设")
	}

	cats, ok := data["categories"].([]any)
	if !ok {
		t.Fatalf("categories 不是数组: %T", data["categories"])
	}
	// 这里不断言非空：测试用的 providers.json 里 categories 就是空数组，
	// 真实资源里才有 4 个分类。只要求它是数组、前端不会拿到 undefined。
	_ = cats
}

// 错误响应同样要有信封（否则前端解析失败路径也会崩）。
func TestErrorResponsesUseEnvelope(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	// 对只支持 GET 的端点发 PUT → 405
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/presets", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("405 响应不是 JSON: %v (%s)", err, truncate(raw))
	}
	if _, ok := top["ok"]; !ok {
		t.Fatalf("405 响应缺少 ok 字段: %s", truncate(raw))
	}
	var okVal bool
	_ = json.Unmarshal(top["ok"], &okVal)
	if okVal {
		t.Error("405 响应的 ok 应为 false")
	}
	if _, hasErr := top["error"]; !hasErr {
		t.Fatalf("405 响应缺少 error 字段: %s", truncate(raw))
	}
}

// 能力不可用时的错误也必须走统一信封，前端才能把原因显示给用户。
//
// ⚠️ 这条原本叫 TestNotImplementedEndpointsUseEnvelope，断言 /api/send 返回
//    501（那时发送链路还没实现）。现在它已经实现了，而测试服务器没有配置
//    RawSend，所以走的是 503 分支——要断言的性质一点没变（错误响应也要走
//    信封、也要带人类可读的原因），只是状态码跟着实现进度变了。
func TestUnavailableEndpointUsesEnvelope(t *testing.T) {
	_, ts, _, _, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/send", "application/json",
		strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503（测试服务器未配置发送能力）", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	var top struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if top.OK || top.Error == nil {
		t.Fatalf("响应信封不完整: %s", truncate(raw))
	}
	if top.Error.Message == "" {
		t.Error("错误信息为空——前端无从告诉用户为什么发不出去")
	}
}
