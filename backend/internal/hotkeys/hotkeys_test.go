package hotkeys

import (
	"path/filepath"
	"testing"
)

func load(t *testing.T) *Keymap {
	t.Helper()
	km, err := LoadKeymap(filepath.Join("..", "..", "..", "resources", "keymap.json"))
	if err != nil {
		t.Fatalf("加载键表失败: %v", err)
	}
	return km
}

// 键表完整性：内容由 V1 的 win32_constants.py 生成，条目数应与之一致。
func TestKeymapIntegrity(t *testing.T) {
	km := load(t)

	if len(km.Keys) != 34 {
		t.Errorf("keys 条目数 = %d，期望 34（V1 SPECIAL_VK 的条目数）", len(km.Keys))
	}
	if len(km.Modifiers) != 7 {
		t.Errorf("modifiers 条目数 = %d，期望 7", len(km.Modifiers))
	}

	// modFlags 取值必须与 Win32 RegisterHotKey 一致
	want := map[string]uint32{"shift": 0x0004, "control": 0x0002, "alt": 0x0001, "win": 0x0008}
	for name, v := range want {
		if km.ModFlags[name] != v {
			t.Errorf("modFlags[%s] = 0x%02X，期望 0x%02X", name, km.ModFlags[name], v)
		}
	}

	// 抽样校验几个关键 VK
	samples := map[string]uint32{"enter": 13, "esc": 27, "tab": 9, "space": 32, "f1": 112, "f12": 123, "up": 38}
	for name, v := range samples {
		if km.Keys[name] != v {
			t.Errorf("keys[%s] = %d，期望 %d", name, km.Keys[name], v)
		}
	}
}

// V1 各处一致的部分：单字符主键 + 修饰键。
func TestParseSingleCharWithModifiers(t *testing.T) {
	km := load(t)
	cases := []struct {
		in   string
		mods uint32
		vk   uint32
	}{
		{"shift+y", ModShift, 'Y'},        // 默认的 send_hotkey
		{"ctrl+c", ModControl, 'C'},       // 默认的 copy_hotkey
		{"ctrl+v", ModControl, 'V'},       // 默认的 paste_hotkey
		{"shift+alt+k", ModShift | ModAlt, 'K'},
		{"y", 0, 'Y'},
		{"1", 0, '1'},
	}
	for _, c := range cases {
		got := km.Parse(c.in)
		if got.Mods != c.mods || got.VK != c.vk {
			t.Errorf("Parse(%q) = {Mods:0x%02X VK:%d}，期望 {0x%02X %d}",
				c.in, got.Mods, got.VK, c.mods, c.vk)
		}
		if !got.Valid() {
			t.Errorf("Parse(%q) 应有效", c.in)
		}
	}
}

// 大小写与空白容错（V1 的 .lower().strip()）。
func TestParseNormalization(t *testing.T) {
	km := load(t)
	for _, in := range []string{"SHIFT+Y", "  shift + y  ", "\tShift+Y\n"} {
		got := km.Parse(in)
		if got.Mods != ModShift || got.VK != 'Y' {
			t.Errorf("Parse(%q) = {0x%02X %d}，期望 {0x%02X %d}", in, got.Mods, got.VK, ModShift, 'Y')
		}
	}
}

// 修饰键的**全部别名**都必须被识别——漏一个就会让某类配置静默失效。
//
// 这条测试来自一个真实踩过的坑：生成器最初只写了 shift/control/alt/win 四个规范名，
// 于是默认的 "ctrl+c" 解析出来 Mods=0（V1 的三个解析器都接受 ctrl/control 别名）。
func TestAllModifierAliases(t *testing.T) {
	km := load(t)
	aliases := map[string]uint32{
		"shift": ModShift, "shft": ModShift,
		"ctrl": ModControl, "control": ModControl,
		"alt": ModAlt,
		"win": ModWin, "windows": ModWin,
	}
	for name, want := range aliases {
		got := km.Parse(name + "+y")
		if got.Mods != want {
			t.Errorf("Parse(%s+y).Mods = 0x%02X，期望 0x%02X", name, got.Mods, want)
		}
	}

	// 组合修饰键
	if got := km.Parse("ctrl+shift+y"); got.Mods != ModControl|ModShift {
		t.Errorf("组合修饰键 = 0x%02X，期望 0x%02X", got.Mods, ModControl|ModShift)
	}
}

// 特殊键 —— 这是 D7 的修复点。
//
// V1 的**活**实现（overlay._parse_hotkey）遇到特殊键会返回 vk=0，
// 于是配了 "f1" 的热键静默不生效；而配置界面允许用户按 F1。
// V2 统一后特殊键真正生效。这条测试**故意与 V1 行为不同**，属有意修复。
func TestParseSpecialKeys(t *testing.T) {
	km := load(t)
	cases := []struct {
		in   string
		mods uint32
		vk   uint32
	}{
		{"enter", 0, 13},
		{"ctrl+enter", ModControl, 13},
		{"esc", 0, 27},
		{"f1", 0, 112},
		{"shift+f12", ModShift, 123},
		{"up", 0, 38},
		{"shift+tab", ModShift, 9},
	}
	for _, c := range cases {
		got := km.Parse(c.in)
		if got.Mods != c.mods || got.VK != c.vk {
			t.Errorf("Parse(%q) = {0x%02X %d}，期望 {0x%02X %d}", c.in, got.Mods, got.VK, c.mods, c.vk)
		}
	}
}

// 无效输入：主键解析不出来时必须 Valid()==false，
// 让调用方放弃注册而不是拿一个错的热键去注册。
func TestParseInvalid(t *testing.T) {
	km := load(t)
	for _, in := range []string{"", "   ", "+", "shift+", "ctrl+nonexistentkey"} {
		got := km.Parse(in)
		if got.Valid() {
			t.Errorf("Parse(%q) 不应有效，却得到 VK=%d", in, got.VK)
		}
	}
	// 只有修饰键没有主键：V1 的 _parse_hotkey 会把 "Shift+Shift+Alt" 的最后一节当主键
	got := km.Parse("Shift+Shift+Alt")
	if got.Valid() {
		t.Logf("Parse(Shift+Shift+Alt) = VK:%d（与 V1 一致：最后一节被当作主键）", got.VK)
	}
}

// 未知修饰键被忽略（不报错），但主键仍要解析出来——V1 行为。
func TestParseUnknownModifierIgnored(t *testing.T) {
	km := load(t)
	got := km.Parse("hyper+y")
	if got.Mods != 0 || got.VK != 'Y' {
		t.Errorf("Parse(hyper+y) = {0x%02X %d}，期望 {0 %d}", got.Mods, got.VK, 'Y')
	}
}

// win 修饰键 —— D7 的第二个修复点（V1 定义了但没一个解析器用它）。
func TestParseWinModifier(t *testing.T) {
	km := load(t)
	got := km.Parse("win+y")
	if got.Mods != ModWin || got.VK != 'Y' {
		t.Errorf("Parse(win+y) = {0x%02X %d}，期望 {0x%02X %d}", got.Mods, got.VK, ModWin, 'Y')
	}
}

// 显示格式 —— V1 hotkey_manager.format_hotkey 的等价实现。
func TestFormat(t *testing.T) {
	cases := map[string]string{
		"shift+y":    "Shift+Y",
		"ctrl+c":     "Ctrl+C",
		"enter":      "Enter",
		"f1":         "F1",
		" SHIFT + y": "Shift+Y",
		"ctRl+v":     "Ctrl+V",
	}
	for in, want := range cases {
		if got := Format(in); got != want {
			t.Errorf("Format(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// V1 的 5 个默认热键必须全部解析成功——若有一个失败，
// 说明默认配置在 V2 里会静默失效。
func TestDefaultHotkeysAllParse(t *testing.T) {
	km := load(t)
	// 来自 config.py 的默认值
	defaults := map[string]string{
		"chat_hotkey":  "y",
		"copy_hotkey":  "ctrl+c",
		"paste_hotkey": "ctrl+v",
		"enter_hotkey": "enter",
		"send_hotkey":  "shift+y",
	}
	for name, hk := range defaults {
		got := km.Parse(hk)
		if !got.Valid() {
			t.Errorf("默认热键 %s=%q 解析失败（V1 下同样失效，但这是缺陷）", name, hk)
		}
	}
}
