package engine

import (
	"reflect"
	"strings"
	"testing"
)

// V1 test_language_detect.py（14 例）与 test_mixed_lang.py（21 例）的逐条移植。
// 这两个文件覆盖的行为是「混合语言拆分」「已是目标语言跳过」的验收标准。

// ── test_language_detect.py ──

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"hello world how are you": "英语",
		"Hallo wie geht's dir schön": "德语",
		"Bonjour ça va bien":       "法语",
		"Hola cómo estás":          "西班牙语",
		"Привет как дела":          "俄语",
		"こんにちは元気ですか":            "日语",
		"안녕하세요":                   "韩语",
		"你好世界这是一个测试":               "中文",
		"where are you going":      "英语",
		"Olá como vai você não":    "葡萄牙语",
		"Ciao come stai però":      "意大利语",
		"สวัสดีครับ":                 "泰语",
	}
	for in, want := range cases {
		if got := DetectLanguage(in); got != want {
			t.Errorf("DetectLanguage(%q) = %q，期望 %q", in, got, want)
		}
	}

	if got := DetectLanguage(""); got != "未知" {
		t.Errorf("DetectLanguage(空) = %q，期望 未知", got)
	}
	if got := DetectLanguage("   "); got != "未知" {
		t.Errorf("DetectLanguage(空白) = %q，期望 未知", got)
	}
}

// ── test_mixed_lang.py: TestDetectLanguage ──

func TestDetectLanguageBasic(t *testing.T) {
	cases := map[string]string{
		"hello world": "英语",
		"你好世界":        "中文",
		"Grüße":      "德语",
		"привет":     "俄语",
	}
	for in, want := range cases {
		if got := DetectLanguage(in); got != want {
			t.Errorf("DetectLanguage(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ── test_mixed_lang.py: TestSplitMixedText ──

func TestSplitMixedText(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		target string
		want  []Segment
	}{
		{"纯外来", "hello world", "zh-CN", []Segment{{"hello world", false}}},
		{"纯目标", "你好世界", "zh-CN", []Segment{{"你好世界", true}}},
		{"混合三段", "你好where are you我的朋友", "zh-CN", []Segment{
			{"你好", true}, {"where are you", false}, {"我的朋友", true}}},
		{"交替四段", "我hello你world", "zh-CN", []Segment{
			{"我", true}, {"hello", false}, {"你", true}, {"world", false}}},
		{"外来开头", "hello你好", "zh-CN", []Segment{{"hello", false}, {"你好", true}}},
		{"外来结尾", "你好world", "zh-CN", []Segment{{"你好", true}, {"world", false}}},
		{"韩语目标保留韩语", "안녕 hello 안녕", "ko", []Segment{
			{"안녕", true}, {" hello ", false}, {"안녕", true}}},
		{"日语目标", "こんにちはhello", "ja", []Segment{{"こんにちは", true}, {"hello", false}}},
		{"俄语目标", "приветhello", "ru", []Segment{{"привет", true}, {"hello", false}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitMixedText(tc.text, tc.target)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitMixedText(%q, %q)\n  got  %#v\n  want %#v", tc.text, tc.target, got, tc.want)
			}
		})
	}

	// 纯标点数字夹在中间：被吸收（只断言段数与第二段非空，与 V1 测试一致）
	segs := SplitMixedText("你好!!!ok???", "zh-CN")
	if len(segs) != 2 || segs[1].Text == "" {
		t.Errorf("SplitMixedText(你好!!!ok???) = %#v，期望 2 段且第二段非空", segs)
	}

	// 空串
	if got := SplitMixedText("", "zh-CN"); len(got) != 0 {
		t.Errorf("SplitMixedText(空) = %#v，期望空", got)
	}
	if got := SplitMixedText("   ", "zh-CN"); len(got) != 0 {
		t.Errorf("SplitMixedText(空白) = %#v，期望空", got)
	}

	// 无中文、目标中文 → 全外来
	if got := SplitMixedText("where are you from", "zh-CN"); !reflect.DeepEqual(got, []Segment{{"where are you from", false}}) {
		t.Errorf("全外来拆分为 %#v", got)
	}
}

// ── test_mixed_lang.py: TestReassemble ──

func TestReassembleMixed(t *testing.T) {
	if got := ReassembleMixed([]Segment{{"hello world", false}}, map[string]string{"hello world": "你好世界"}); got != "你好世界" {
		t.Errorf("纯外来重组 = %q", got)
	}
	if got := ReassembleMixed([]Segment{{"你好", true}}, nil); got != "你好" {
		t.Errorf("纯目标重组 = %q", got)
	}
	if got := ReassembleMixed(
		[]Segment{{"你好", true}, {"where are you", false}, {"我的朋友", true}},
		map[string]string{"where are you": "你在哪里"},
	); got != "你好你在哪里我的朋友" {
		t.Errorf("混合重组 = %q", got)
	}
	if got := ReassembleMixed([]Segment{{"hello", false}}, map[string]string{}); got != "hello" {
		t.Errorf("无译文回退 = %q", got)
	}
}

// ── _should_skip_internal（61.4% 的来源，最关键的一条）──

func TestShouldSkip(t *testing.T) {
	skip := []string{
		"你好世界", "这是中文消息", "哈哈",
	}
	for _, s := range skip {
		if !ShouldSkip(s, "zh-CN") {
			t.Errorf("ShouldSkip(%q, zh-CN) 应为 true", s)
		}
	}
	keep := []string{
		"hello world", "привет", "こんにちは", "안녕하세요",
	}
	for _, s := range keep {
		if ShouldSkip(s, "zh-CN") {
			t.Errorf("ShouldSkip(%q, zh-CN) 应为 false", s)
		}
	}
	// 空文本跳过
	if !ShouldSkip("", "zh-CN") || !ShouldSkip("   ", "zh-CN") {
		t.Error("空文本应跳过")
	}
	// 目标语言前缀匹配：zh-CN.startswith("zh")
	if !ShouldSkip("你好", "zh-TW") {
		t.Error("zh-TW 目标下中文应跳过")
	}
}

// ── prompt / token ──

func TestMaxOutputTokens(t *testing.T) {
	// clamp 64~160（V1: max(64, min(160, 56+len/4))）
	if got := MaxOutputTokens("hi", "\n---\n"); got != 64 {
		t.Errorf("短文本 max_tokens = %d，期望 64", got)
	}
	if got := MaxOutputTokens("", "\n---\n"); got != 64 {
		t.Errorf("空文本 max_tokens = %d，期望 64", got)
	}
	long := ""
	for i := 0; i < 500; i++ {
		long += "字"
	}
	if got := MaxOutputTokens(long, "\n---\n"); got != 160 {
		t.Errorf("长文本 max_tokens = %d，期望 160", got)
	}
	// 批处理分隔符分支：500*n+500
	batch := "a\n---\nb\n---\nc"
	if got := MaxOutputTokens(batch, "\n---\n"); got != 1500 {
		t.Errorf("批处理 max_tokens = %d，期望 1500", got)
	}
}

func TestReceiveSystemPrompt(t *testing.T) {
	p := ReceiveSystemPrompt("简体中文", "sry=抱歉")
	// 与 V1 的加固 prompt 关键语义一致
	for _, want := range []string{"简体中文", "output the direct translation", "sry=抱歉", "Keep names, IDs, tags, URLs and emoji unchanged"} {
		if !contains(p, want) {
			t.Errorf("ReceiveSystemPrompt 缺少 %q\n完整 prompt: %s", want, p)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
