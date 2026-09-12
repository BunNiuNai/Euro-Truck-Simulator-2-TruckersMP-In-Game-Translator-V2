package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setup 把配置目录指向临时目录（绝不污染真实用户目录），
// 并把「默认配置模板」写在**另一个**临时目录里——
// 模板属于 resources/（只读），用户配置属于数据目录，两者不能同路径。
//
// 这个坑真实踩过：最初测试把两者写成同一个路径，于是「写损坏配置」的用例
// 顺手把默认模板也覆盖掉了，报出与真实原因无关的「缺少 defaults 段」。
func setup(t *testing.T) (dataDir, defaultsPath string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv(EnvDataDir, dataDir)
	defaultsPath = writeDefaults(t, t.TempDir())
	return dataDir, defaultsPath
}

func writeDefaults(t *testing.T, dir string) string {
	t.Helper()
	p := dir + string(os.PathSeparator) + "config.json"
	content := `{
  "configVersion": 1,
  "defaults": {
    "target_language": "zh-CN",
    "send_target_language": "en",
    "window_opacity": 0.8,
    "font_size": 12,
    "max_messages": 50,
    "window_mode": "standalone",
    "win_w": 620,
    "win_h": 360,
    "chat_hotkey": "y",
    "copy_hotkey": "ctrl+c",
    "paste_hotkey": "ctrl+v",
    "enter_hotkey": "enter",
    "send_hotkey": "shift+y",
    "llm_providers": []
  }
}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── DPAPI ──

func TestDPAPIRoundTrip(t *testing.T) {
	for _, plain := range []string{"sk-abc123", "中文密钥", "a", strings.Repeat("x", 2000)} {
		enc, err := dpapiProtect(plain)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		if enc == plain {
			t.Error("密文不应等于明文")
		}
		dec, err := dpapiUnprotect(enc)
		if err != nil {
			t.Fatalf("解密失败: %v", err)
		}
		if dec != plain {
			t.Errorf("往返后 = %q，期望 %q", dec, plain)
		}
	}
}

func TestMaybeEncryptSkipsEmptyAndAlreadyEncrypted(t *testing.T) {
	if got := maybeEncrypt(""); got != "" {
		t.Errorf("空值不应加密，得到 %q", got)
	}
	once := maybeEncrypt("sk-secret")
	if !IsEncrypted(once) {
		t.Fatalf("加密后应带 dpapi: 前缀，得到 %q", once)
	}
	if twice := maybeEncrypt(once); twice != once {
		t.Error("已加密的值不应二次加密")
	}
}

func TestMaybeDecryptPlaintextPassesThrough(t *testing.T) {
	// 用户手工填写的明文应原样返回（V1 行为）
	got, ok := maybeDecrypt("sk-plain")
	if !ok || got != "sk-plain" {
		t.Errorf("明文应原样通过，得到 (%q, %v)", got, ok)
	}
}

func TestMaybeDecryptCorruptCiphertextFails(t *testing.T) {
	if _, ok := maybeDecrypt(encPrefix + "这不是合法base64!!!"); ok {
		t.Error("损坏的密文应返回 ok=false，让调用方保留原值")
	}
}

// ── 加载 / 保存 ──

func TestLoadCreatesConfigWithDefaults(t *testing.T) {
	_, defPath := setup(t)

	cfg, warnings, err := Load(defPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("首次加载不应有警告: %v", warnings)
	}
	if cfg.TargetLanguage != "zh-CN" || cfg.MaxMessages != 50 || cfg.SendHotkey != "shift+y" {
		t.Errorf("默认值未生效: %+v", cfg)
	}
	if _, err := os.Stat(ConfigPath()); err != nil {
		t.Errorf("首次加载应把默认配置落盘: %v", err)
	}
}

// 关键：API Key 落盘必须是密文，文件里不得出现明文。
func TestSaveEncryptsAPIKeyOnDisk(t *testing.T) {
	_, defPath := setup(t)

	cfg, _, err := Load(defPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LLMProviders = []Provider{{
		ID: "p1", Label: "DeepSeek",
		Endpoint: "https://api.deepseek.com/v1/chat/completions",
		APIKey:   "sk-super-secret-value", Model: "deepseek-chat",
		Enabled: true, Timeout: 8,
	}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-super-secret-value") {
		t.Fatal("配置文件里出现了明文 API Key —— 这是严重安全问题")
	}
	if !strings.Contains(string(raw), encPrefix) {
		t.Error("配置文件中应存在 dpapi: 密文")
	}

	// 内存中的明文不应被加密流程污染
	if cfg.LLMProviders[0].APIKey != "sk-super-secret-value" {
		t.Errorf("Save 污染了调用方的明文: %q", cfg.LLMProviders[0].APIKey)
	}

	// 重新加载应能解回明文
	cfg2, warn2, err := Load(defPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(warn2) != 0 {
		t.Errorf("正常往返不应有警告: %v", warn2)
	}
	if cfg2.LLMProviders[0].APIKey != "sk-super-secret-value" {
		t.Errorf("重新加载后 API Key = %q，期望明文", cfg2.LLMProviders[0].APIKey)
	}
}

// 关键：DPAPI 解密失败时必须保留密文，绝不能写空串。
// 否则用户换电脑/换用户后，密钥会被永久抹掉。
func TestLoadKeepsCiphertextWhenDecryptFails(t *testing.T) {
	_, defPath := setup(t)

	corrupt := encPrefix + "Zm9vYmFy" // 合法 base64 但不是有效 DPAPI 密文
	content := `{"configVersion":1,"target_language":"zh-CN","llm_providers":[
		{"id":"p1","label":"Broken","api_key":"` + corrupt + `","enabled":true}]}`
	if err := os.WriteFile(ConfigPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := Load(defPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("解密失败应产生警告，提示用户重填")
	}
	if cfg.LLMProviders[0].APIKey != corrupt {
		t.Errorf("解密失败时必须保留原密文，得到 %q", cfg.LLMProviders[0].APIKey)
	}

	// 再次保存时，损坏的密文也不应被覆盖成空串
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(ConfigPath())
	if !strings.Contains(string(raw), corrupt) {
		t.Error("保存后密文丢失了 —— 用户密钥会被永久抹掉")
	}
}

func TestCorruptedConfigBackedUp(t *testing.T) {
	_, defPath := setup(t)
	if err := os.WriteFile(ConfigPath(), []byte("{ 这不是合法 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, warnings, err := Load(defPath)
	if err != nil {
		t.Fatalf("损坏配置应被重建而不是报错: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("应产生「已备份」警告")
	}
	entries, _ := os.ReadDir(ConfigDir())
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupted.") {
			found = true
		}
	}
	if !found {
		t.Errorf("应存在 .corrupted.* 备份文件，实际: %v", entries)
	}
}

// 原子写：保存后不应残留临时文件。
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	setup(t)
	if err := Save(&Config{TargetLanguage: "zh-CN"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
	if len(names) != 1 || names[0] != "config.json" {
		t.Errorf("目录应只含 config.json，实际: %v", names)
	}
}

// Provider 字段必须与 V1 的 13 个字段一一对应，否则用户配置无法无损表达。
func TestProviderHas13Fields(t *testing.T) {
	raw, err := json.Marshal(Provider{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 13 {
		t.Errorf("Provider 字段数 = %d，期望 13: %v", len(m), keys(m))
	}
	want := []string{"id", "label", "endpoint", "api_key", "model", "enabled",
		"preset_id", "icon", "api_format", "weight", "extra_headers", "extra_body", "timeout"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少字段 %q", k)
		}
	}
}

// Config 的 JSON 字段应与 resources/config.json 的 defaults **双向一致**。
//
// 这里刻意不硬编码字段数：硬编码只能测出「数字变了」，
// 却测不出真正危险的漂移——「生成器和结构体只改了一边」。
// 直接读生成物比对，两边任何一边多/少字段都会被抓到。
func TestConfigFieldCount(t *testing.T) {
	raw, _ := json.Marshal(Config{})
	var fromStruct map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fromStruct)

	path := filepath.Join("..", "..", "..", "resources", "config.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 %s（%v），跳过与生成物的一致性检查", path, err)
	}
	var doc struct {
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}

	for k := range fromStruct {
		if _, ok := doc.Defaults[k]; !ok {
			t.Errorf("Config 有字段 %q，但 config.json 的 defaults 没有——"+
				"改了结构体就要重新跑 tools/gen_resources.py", k)
		}
	}
	for k := range doc.Defaults {
		if _, ok := fromStruct[k]; !ok {
			t.Errorf("config.json 的 defaults 有字段 %q，但 Config 结构体没有——"+
				"改了生成器就要同步 internal/config", k)
		}
	}
}

// ── 热重载 ──

func TestWatcherDetectsChange(t *testing.T) {
	_, defPath := setup(t)

	if err := Save(&Config{TargetLanguage: "zh-CN"}); err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(defPath)

	if _, changed, _, _ := w.Poll(); changed {
		t.Error("未修改时不应报告变化")
	}

	time.Sleep(1100 * time.Millisecond) // 文件系统 mtime 分辨率
	if err := Save(&Config{TargetLanguage: "en", MaxMessages: 80}); err != nil {
		t.Fatal(err)
	}

	loaded, changed, _, err := w.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("修改后应报告变化")
	}
	if loaded.TargetLanguage != "en" || loaded.MaxMessages != 80 {
		t.Errorf("热重载结果不正确: %+v", loaded)
	}
}

// ── 目录解析 ──

func TestDocumentsPathNonEmpty(t *testing.T) {
	p := DocumentsPath()
	if p == "" {
		t.Fatal("文档目录不应为空")
	}
	// 必须已展开 %VAR%——否则会得到字面量 "%USERPROFILE%\Documents"
	if strings.Contains(p, "%") {
		t.Errorf("文档目录含未展开的环境变量: %q（os.ExpandEnv 不认 %%VAR%% 语法）", p)
	}
}

func TestConfigDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, dir)
	if got := ConfigDir(); got != dir {
		t.Errorf("ConfigDir() = %q，期望环境变量覆盖值 %q", got, dir)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
