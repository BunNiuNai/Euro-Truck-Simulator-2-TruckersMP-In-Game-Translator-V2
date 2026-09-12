// Package hotkeys 统一的热键字符串解析。
//
// 存在的理由（缺陷 D7）：V1 把同一件事实现了 4 遍，且行为不一致——
//
//	win32_constants.vk_code          支持特殊键（enter/esc/f1…）  ← 活代码（输入模拟用）
//	hotkey_manager._parse_hotkey_vk  支持特殊键                    ← 死代码（整个文件未被引用）
//	hotkey_manager._parse_hotkey     不支持特殊键                  ← 死代码
//	overlay._parse_hotkey            不支持特殊键                  ← **活代码（全局热键实际在用）**
//
// 于是 V1 出现一个静默失效：配置界面（HotkeyCapture）允许你按 F1 这种特殊键，
// 但真正生效的 overlay 轮询解析器只接受单字符，vk=0 就直接 return——**配了不生效，也不报错**。
//
// V2 统一为「单一 keymap.json + 单一解析器」，取并集语义：
//   - 修饰键：shift/shft、ctrl/control、alt、win/windows（win 是新增，见下）
//   - 主键：特殊键表优先，其次单字符（取大写 ASCII 码，与 V1 一致）
//
// 与 V1 的两处**有意差异**（都属于修复 D7，需在验收时知晓）：
//  1. 特殊键（enter/esc/tab/f1-f12/方向键等）现在**真正生效**——V1 配了不生效。
//  2. win 修饰键被接受（V1 的键表里定义了 win，但没有一个解析器用它）。
//
// 技术常量来源统一为 resources/keymap.json（由 V1 的 win32_constants.py 生成），
// C++ 侧也用同一份表，避免两个语言各写一份而漂移。
package hotkeys

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MOD_* 标志，取值与 Win32 RegisterHotKey 一致（V1 的 MOD_SHIFT/MOD_CONTROL/MOD_ALT 相同）。
const (
	ModAlt     uint32 = 0x0001
	ModControl uint32 = 0x0002
	ModShift   uint32 = 0x0004
	ModWin     uint32 = 0x0008
)

// Keymap 是 resources/keymap.json 的映射。
type Keymap struct {
	SchemaVersion int               `json:"schemaVersion"`
	Modifiers     map[string]uint32 `json:"modifiers"` // 修饰键名 → VK（用于模拟按键）
	Keys          map[string]uint32 `json:"keys"`      // 键名 → VK
	ModFlags      map[string]uint32 `json:"modFlags"`  // 修饰键名 → MOD_* 标志
}

// LoadKeymap 从 JSON 加载键表。缺失或损坏时返回错误——
// resources/ 是程序必备资源（词库也在那里），不应静默降级。
func LoadKeymap(path string) (*Keymap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取键表 %s: %w", path, err)
	}
	var k Keymap
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("解析键表 %s: %w", path, err)
	}
	if k.SchemaVersion == 0 {
		return nil, fmt.Errorf("键表 %s 缺少 schemaVersion", path)
	}
	if len(k.Keys) == 0 {
		return nil, fmt.Errorf("键表 %s 的 keys 为空", path)
	}
	return &k, nil
}

// Spec 是一个解析结果。可直接喂给 RegisterHotKey(mods, vk)。
type Spec struct {
	Mods uint32 // MOD_* 位掩码
	VK   uint32 // 虚拟键码
}

// Valid 表示主键是否解析成功。V1 的调用方在 vk==0 时直接放弃注册/轮询。
func (s Spec) Valid() bool { return s.VK != 0 }

// Parse 解析 "shift+y" / "ctrl+enter" / "f1" / "y" 这类字符串。
//
// 与 V1 一致的细节：
//   - 按 '+' 切分，最后一段是主键，前面都是修饰键
//   - 未知修饰键被忽略（不报错）
//   - 单字符主键取大写后的码点（"y" → 89），不做 VK 合法性校验——
//     非 ASCII 单字符会得到无意义的码点，这是 V1 原有行为，调用方需自行校验 VK ≤ 0xFF
func (k *Keymap) Parse(hotkey string) Spec {
	var spec Spec
	parts := strings.Split(strings.ToLower(strings.TrimSpace(hotkey)), "+")
	if len(parts) == 0 {
		return spec
	}

	for _, p := range parts[:len(parts)-1] {
		p = strings.TrimSpace(p)
		if flag, ok := k.ModFlags[p]; ok {
			spec.Mods |= flag
		}
		// 不在 ModFlags 里的名字一律忽略（V1 行为：未知修饰键不报错）
	}

	key := strings.TrimSpace(parts[len(parts)-1])
	if key == "" {
		return spec
	}
	if vk, ok := k.Keys[key]; ok {
		spec.VK = vk
		return spec
	}
	if utf8.RuneCountInString(key) == 1 {
		spec.VK = uint32(unicode.ToUpper([]rune(key)[0]))
	}
	return spec
}

// Format 返回显示用的规范化写法：V1 hotkey_manager.format_hotkey / overlay._format_hotkey
// 的等价实现（"shift+y" → "Shift+Y"）。
func Format(hotkey string) string {
	parts := strings.Split(strings.TrimSpace(hotkey), "+")
	for i, p := range parts {
		parts[i] = titlePart(p)
	}
	return strings.Join(parts, "+")
}

// titlePart 等价于 Python str.title() 对单个短词的效果：
// 首字母大写、其余小写（"f1" → "F1"、"ctRl" → "Ctrl"）。
func titlePart(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return ""
	}
	return string(unicode.ToUpper(r[0])) + strings.ToLower(string(r[1:]))
}

// KeyName 返回主键的规范名（用于日志与 UI）。找不到则返回空串。
func (k *Keymap) KeyName(vk uint32) string {
	for name, v := range k.Keys {
		if v == vk {
			return name
		}
	}
	return ""
}
