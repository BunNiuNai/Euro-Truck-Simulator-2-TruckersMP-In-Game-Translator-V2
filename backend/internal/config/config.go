// Package config 配置模型、DPAPI 密钥加密、原子写、热重载。
//
// V1 来源：config.py（401 行）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - 密钥用 DPAPI 加密存储，配置文件中不得出现明文（V1 的 _SECRET_FIELDS）
//   - DPAPI 解密失败时**保留原密文值，绝不写空串**（防止密钥被永久覆盖）
//   - 一律原子写：临时文件 + rename，中断不产生半截配置
//   - 配置损坏时备份为 .corrupted.{timestamp} 后重建默认值
//   - 主目录不可写时回退到 %LOCALAPPDATA%
//   - 热重载：按 mtime 判断，调用方负责轮询（V1 为 3 秒）
//
// 与 V1 的差异（ADR-008，已确认）：
//   - V2 **不读 V1 配置**，用户重新填写 Provider 与 API Key
//   - 因此不保留 api_endpoint / api_key / api_model 三个兼容字段
//   - 目录用「文档\ETS2 Translator V2\」：与 V1 同处「文档」下（用户找得到），
//     但互不覆盖，V1 可继续使用
package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// ConfigVersion 是 V2 配置版本。
const ConfigVersion = 1

// DirName 是配置与日志目录名（位于「文档」下，与 V1 并列不冲突）。
const DirName = "ETS2 Translator V2"

// Provider 是单个 LLM 供应商配置。
// 字段与 V1 的 ProviderConfig **逐一对齐（13 个）**，否则用户配置无法迁移表达。
type Provider struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Enabled  bool   `json:"enabled"`

	PresetID  string `json:"preset_id"`
	Icon      string `json:"icon"`
	APIFormat string `json:"api_format"`
	Weight    int    `json:"weight"`

	ExtraHeaders map[string]string `json:"extra_headers"`
	ExtraBody    map[string]any    `json:"extra_body"`

	Timeout int `json:"timeout"`
}

// Config 是应用配置（26 个字段，对应 resources/config.json 的 defaults）。
type Config struct {
	TargetLanguage     string `json:"target_language"`
	SendTargetLanguage string `json:"send_target_language"`

	WindowOpacity float64 `json:"window_opacity"`
	FontSize      int     `json:"font_size"`
	MaxMessages   int     `json:"max_messages"`

	PlayerName string `json:"player_name"`

	WindowMode   string `json:"window_mode"`
	ClickThrough bool   `json:"click_through"`
	WinX         int    `json:"win_x"`
	WinY         int    `json:"win_y"`
	WinW         int    `json:"win_w"`
	WinH         int    `json:"win_h"`

	// 设置窗口的几何。W/H 是 V1 就有的；X/Y 是 V2 补的——
	// V1 只记尺寸不记位置，所以它的设置窗口每次都在屏幕同一处打开。
	SettingsWinX int `json:"settings_win_x"`
	SettingsWinY int `json:"settings_win_y"`
	SettingsWinW int `json:"settings_win_w"`
	SettingsWinH int `json:"settings_win_h"`

	ChatHotkey  string `json:"chat_hotkey"`
	CopyHotkey  string `json:"copy_hotkey"`
	PasteHotkey string `json:"paste_hotkey"`
	EnterHotkey string `json:"enter_hotkey"`
	SendHotkey  string `json:"send_hotkey"`

	DebugLog           bool   `json:"debug_log"`
	ShowLanguageLabel  bool   `json:"show_language_label"`
	ShowOriginalText   bool   `json:"show_original_text"`
	AccentColor        string `json:"accent_color"`

	// Theme 是界面主题："dark" | "light" | "system"。
	//
	// **V2 新增字段（V1 无此配置）**：V1 的悬浮窗把深色写死，设置页另有一套
	// VS Code 配色（缺陷 D1）。V2 引入统一切换，并且需要 native 侧配合切换
	// 窗口的沉浸式深色模式——否则浅色主题下 Mica 材质仍是深色的。
	Theme string `json:"theme"`

	AdMessages  []string `json:"ad_messages"`
	AdCountdown string   `json:"ad_countdown"`

	LLMProviders []Provider `json:"llm_providers"`
}

// Clone 返回一份与配置本体**完全无共享**的深拷贝。
//
// 为什么必须有：`c := *other` 这样的浅拷贝会与本体共享 LLMProviders 的
// 底层数组和 ExtraHeaders/ExtraBody 这两个 map，调用方一改就改到了真实配置。
// 举例：调用方把快照里的 `LLMProviders[0].Model` 改掉，浅拷贝会让它改到本体，
// 随后一次落盘就把这个改动永久写进磁盘——而调用方以为自己只是在改临时副本。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.AdMessages = append([]string(nil), c.AdMessages...)
	out.LLMProviders = make([]Provider, len(c.LLMProviders))
	for i, p := range c.LLMProviders {
		out.LLMProviders[i] = p.clone()
	}
	return &out
}

// Clone 返回 Provider 的深拷贝（含两个 map 字段）。
func (p Provider) clone() Provider {
	out := p
	if p.ExtraHeaders != nil {
		out.ExtraHeaders = make(map[string]string, len(p.ExtraHeaders))
		for k, v := range p.ExtraHeaders {
			out.ExtraHeaders[k] = v
		}
	}
	if p.ExtraBody != nil {
		out.ExtraBody = make(map[string]any, len(p.ExtraBody))
		for k, v := range p.ExtraBody {
			out.ExtraBody[k] = v
		}
	}
	return out
}

// ── 目录解析 ──────────────────────────────────────────────────

// DocumentsPath 从注册表读取「文档」目录（V1 config.py:get_documents_path 的等价实现）。
//
// 注意：注册表值通常是 REG_EXPAND_SZ（如 %USERPROFILE%\Documents），
// 而 Go 的 os.ExpandEnv 只认 $VAR 语法、不认 %VAR%——必须自己展开，
// 否则会得到未展开的路径，日志永远找不到（静默失效）。
func DocumentsPath() string {
	var h winregKey
	sub := utf16Ptr(`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`)
	if regOpenKeyEx(hkeyCurrentUser, sub, 0, keyRead, &h) != nil {
		return fallbackDocumentsPath()
	}
	defer regCloseKey(h)

	name := utf16Ptr("Personal")
	var typ uint32
	var buf [1024]uint16
	n := uint32(len(buf) * 2)
	if regQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return fallbackDocumentsPath()
	}
	raw := filepath.Clean(expandEnvVars(utf16ToString(buf[:n/2])))
	if raw != "" && dirExists(raw) {
		return raw
	}
	return fallbackDocumentsPath()
}

func fallbackDocumentsPath() string {
	up := os.Getenv("USERPROFILE")
	if up == "" {
		up = os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
	}
	return filepath.Join(up, "Documents")
}

// EnvDataDir 可覆盖配置目录。用途：单元测试（不污染真实用户目录）、
// 便携模式（配置放程序旁边）、以及受限环境下的验证。
const EnvDataDir = "ETS2_TRANSLATOR_DATA_DIR"

// ConfigDir 返回配置与日志目录。优先级：
//
//	1. 环境变量 ETS2_TRANSLATOR_DATA_DIR
//	2. 「文档\ETS2 Translator V2\」（与 V1 同处文档下，用户找得到，且互不覆盖）
//	3. %LOCALAPPDATA%\ETS2 Translator V2\（文档目录不可写时）
func ConfigDir() string {
	if d := os.Getenv(EnvDataDir); d != "" {
		return d
	}
	primary := filepath.Join(DocumentsPath(), DirName)
	if dirWritable(primary) {
		return primary
	}
	fallback := filepath.Join(
		firstNonEmpty(os.Getenv("LOCALAPPDATA"), os.Getenv("USERPROFILE"), "."), DirName)
	if dirWritable(fallback) {
		return fallback
	}
	return primary
}

// ConfigPath 返回配置文件完整路径。
func ConfigPath() string { return filepath.Join(ConfigDir(), "config.json") }

// LogDir 返回日志目录（V1 为 <配置目录>\logs）。
func LogDir() string { return filepath.Join(ConfigDir(), "logs") }

func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── 加载 / 保存 ───────────────────────────────────────────────

// Defaults 是内置默认值，由 resources/config.json 的 defaults 段提供。
type Defaults map[string]json.RawMessage

// Load 读取配置。文件不存在时用 defaults 生成并落盘。
//
// 返回的 Warnings 记录解密失败等非致命问题，调用方应展示给用户。
func Load(defaultsPath string) (*Config, []string, error) {
	var warnings []string

	cfg := &Config{}
	if defaultsPath != "" {
		if err := applyDefaults(cfg, defaultsPath); err != nil {
			return nil, warnings, err
		}
	}

	path := ConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := Save(cfg); err != nil {
				return nil, warnings, err
			}
			return cfg, warnings, nil
		}
		return nil, warnings, fmt.Errorf("读取配置 %s: %w", path, err)
	}

	if err := json.Unmarshal(raw, cfg); err != nil {
		// 配置损坏：备份后重建（V1 行为）
		backup := fmt.Sprintf("%s.corrupted.%s", path, time.Now().Format("20060102-150405"))
		if renameErr := os.Rename(path, backup); renameErr == nil {
			warnings = append(warnings, "配置损坏，已备份为 "+filepath.Base(backup))
		}
		if err := Save(cfg); err != nil {
			return nil, warnings, err
		}
		return cfg, warnings, nil
	}

	// 解密敏感字段：失败时**保留原密文**，绝不写空串（V1 关键行为）
	for i := range cfg.LLMProviders {
		p := &cfg.LLMProviders[i]
		if dec, ok := maybeDecrypt(p.APIKey); ok {
			p.APIKey = dec
		} else {
			warnings = append(warnings,
				fmt.Sprintf("Provider %q 的 API Key 解密失败，请在设置中重新填写", p.Label))
		}
	}
	return cfg, warnings, nil
}

func applyDefaults(cfg *Config, defaultsPath string) error {
	raw, err := os.ReadFile(defaultsPath)
	if err != nil {
		return fmt.Errorf("读取默认配置 %s: %w", defaultsPath, err)
	}
	var wrapper struct {
		Defaults json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return fmt.Errorf("解析默认配置: %w", err)
	}
	if len(wrapper.Defaults) == 0 {
		return fmt.Errorf("默认配置缺少 defaults 段")
	}
	return json.Unmarshal(wrapper.Defaults, cfg)
}

// Save 原子写入配置，写入前加密敏感字段。
func Save(cfg *Config) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s: %w", dir, err)
	}

	// 复制副本再加密，避免污染调用方内存里的明文
	out := *cfg
	out.LLMProviders = make([]Provider, len(cfg.LLMProviders))
	copy(out.LLMProviders, cfg.LLMProviders)
	for i := range out.LLMProviders {
		out.LLMProviders[i].APIKey = maybeEncrypt(out.LLMProviders[i].APIKey)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(ConfigPath(), data)
}

// atomicWrite 写临时文件后 rename（V1 _atomic_save 的等价实现）。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "config_*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功后这里是无害的 no-op

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── 敏感字段加密 ──────────────────────────────────────────────

const encPrefix = "dpapi:"

// maybeEncrypt 对敏感值加密；空值与已加密值原样返回。
func maybeEncrypt(value string) string {
	if value == "" || strings.HasPrefix(value, encPrefix) {
		return value
	}
	enc, err := dpapiProtect(value)
	if err != nil {
		// 加密失败时不写入明文——宁可留空也不能泄露密钥
		return ""
	}
	return encPrefix + enc
}

// maybeDecrypt 解密；失败或非密文时返回 ("", false)。
//
// 关键：调用方在 false 时必须**保留原值**，不能写成空串——
// 否则一旦 DPAPI 密钥变化（换电脑/换用户），用户密钥会被永久抹掉。
func maybeDecrypt(value string) (string, bool) {
	if !strings.HasPrefix(value, encPrefix) {
		return value, true // 明文（例如用户手工编辑过）
	}
	plain, err := dpapiUnprotect(strings.TrimPrefix(value, encPrefix))
	if err != nil {
		return "", false
	}
	return plain, true
}

// IsEncrypted 报告某个值是否已加密（供测试与诊断使用）。
func IsEncrypted(value string) bool { return strings.HasPrefix(value, encPrefix) }

// ── 热重载 ────────────────────────────────────────────────────

// Watcher 按 mtime 感知配置变化（V1 为 3 秒轮询；调用方决定间隔）。
type Watcher struct {
	mu       sync.Mutex
	path     string
	lastMod  time.Time
	defPath  string
}

// NewWatcher 创建监视器。
func NewWatcher(defaultsPath string) *Watcher {
	w := &Watcher{path: ConfigPath(), defPath: defaultsPath}
	if fi, err := os.Stat(w.path); err == nil {
		w.lastMod = fi.ModTime()
	}
	return w
}

// Poll 检查配置是否被外部修改。返回 (新配置, 是否变化, 警告, 错误)。
func (w *Watcher) Poll() (*Config, bool, []string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	fi, err := os.Stat(w.path)
	if err != nil {
		return nil, false, nil, nil // 文件暂时不可读不算变化
	}
	if fi.ModTime().Equal(w.lastMod) {
		return nil, false, nil, nil
	}
	w.lastMod = fi.ModTime()

	cfg, warnings, err := Load(w.defPath)
	if err != nil {
		return nil, false, warnings, err
	}
	return cfg, true, warnings, nil
}

// ── DPAPI（Windows 数据保护 API）───────────────────────────────

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32DLL             = lazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32DLL.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32DLL.NewProc("CryptUnprotectData")
	kernel32DLL            = lazyDLL("kernel32.dll")
	procLocalFree          = kernel32DLL.NewProc("LocalFree")
)

// dpapiProtect 用当前用户凭据加密，返回 base64 密文。
// dwFlags 传 0，与 V1 一致（V1 未设 CRYPTPROTECT_UI_FORBIDDEN）。
func dpapiProtect(plaintext string) (string, error) {
	in := []byte(plaintext)
	var inBlob dataBlob
	if len(in) > 0 {
		inBlob.cbData = uint32(len(in))
		inBlob.pbData = &in[0]
	}
	var outBlob dataBlob

	ret, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return "", fmt.Errorf("CryptProtectData 失败: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))

	enc := unsafe.Slice(outBlob.pbData, outBlob.cbData)
	return base64.StdEncoding.EncodeToString(enc), nil
}

// dpapiUnprotect 解密 base64 密文。
func dpapiUnprotect(ciphertext string) (string, error) {
	enc, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	var inBlob dataBlob
	if len(enc) > 0 {
		inBlob.cbData = uint32(len(enc))
		inBlob.pbData = &enc[0]
	}
	var outBlob dataBlob

	ret, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return "", fmt.Errorf("CryptUnprotectData 失败: %w", callErr)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))

	return string(unsafe.Slice(outBlob.pbData, outBlob.cbData)), nil
}
