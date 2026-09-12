package dictionary

import (
	"path/filepath"
	"strings"
	"testing"
)

// V1 test_chat_dictionary.py 的 13 个用例逐条移植。
// 任何一条失败都意味着术语处理出现了功能回归（首要原则 P1）。
//
// 词库路径：本测试在 backend/internal/dictionary 下运行，
// ../../../resources 即 Translator-V2/resources。

func loadTestDict(t *testing.T) *Dictionary {
	t.Helper()
	path := filepath.Join("..", "..", "..", "resources", "dictionary.json")
	d, err := Load(path)
	if err != nil {
		t.Fatalf("加载词库失败: %v", err)
	}
	for _, w := range d.Warnings {
		t.Logf("词库警告: %s", w)
	}
	return d
}

// ── 第 1 层：俚语 token 词典 ──

func TestSlangTokenHit(t *testing.T) {
	d := loadTestDict(t)
	cases := map[string]string{
		"wtf": "什么鬼",
		"sry": "抱歉",
		"rec": "已录屏",
		"ram": "撞人",
	}
	for in, want := range cases {
		got, ok := d.Lookup(in)
		if !ok || got != want {
			t.Errorf("Lookup(%q) = (%q, %v)，期望 %q", in, got, ok, want)
		}
	}
}

func TestSlangTokenMissReturnsEmpty(t *testing.T) {
	d := loadTestDict(t)
	// 长句需要 LLM，本地字典必须返回未命中
	if got, ok := d.Lookup("this is a long sentence that needs llm"); ok {
		t.Errorf("长句不应命中本地字典，却返回 (%q, true)", got)
	}
}

// ── 第 2 层：完整短语精确匹配 ──

func TestPhraseFallback(t *testing.T) {
	d := loadTestDict(t)
	if got, ok := d.Lookup("rec ban"); !ok || got != "已录屏，等封禁" {
		t.Errorf("Lookup(rec ban) = (%q, %v)，期望 已录屏，等封禁", got, ok)
	}
	if got, ok := d.Lookup("thank you"); !ok || got != "谢谢" {
		t.Errorf("Lookup(thank you) = (%q, %v)，期望 谢谢", got, ok)
	}
}

func TestMultilingualPhrase(t *testing.T) {
	d := loadTestDict(t)
	if got, ok := d.Lookup("gute reise"); !ok || got != "一路顺风" {
		t.Errorf("Lookup(gute reise) = (%q, %v)，期望 一路顺风", got, ok)
	}
	if got, ok := d.Lookup("сам виноват"); !ok || got != "是你自己的错" {
		t.Errorf("Lookup(сам виноват) = (%q, %v)，期望 是你自己的错", got, ok)
	}
}

// ── 第 3 层：结构化短语 ──

func TestStructuredPhraseWithName(t *testing.T) {
	d := loadTestDict(t)
	// "someplayer" 长度 ≥ 6，被识别为玩家名并保留原文
	if got, ok := d.Lookup("report someplayer"); !ok || got != "举报 someplayer" {
		t.Errorf("Lookup(report someplayer) = (%q, %v)，期望 举报 someplayer", got, ok)
	}
}

// ── ETS2 词汇保留 ──

func TestETS2TermPresent(t *testing.T) {
	d := loadTestDict(t)
	want := map[string]string{"truck": "卡车", "trailer": "挂车", "convoy": "车队"}
	for term, exp := range want {
		got, ok := d.ETSTerm(term)
		if !ok || got != exp {
			t.Errorf("ETSTerm(%q) = (%q, %v)，期望 %q", term, got, ok, exp)
		}
	}
}

func TestETS2TermSingleWordHit(t *testing.T) {
	d := loadTestDict(t)
	if got, ok := d.Lookup("truck"); !ok || got != "卡车" {
		t.Errorf("Lookup(truck) = (%q, %v)，期望 卡车", got, ok)
	}
	if got, ok := d.Lookup("convoy"); !ok || got != "车队" {
		t.Errorf("Lookup(convoy) = (%q, %v)，期望 车队", got, ok)
	}
}

func TestETS2TermMultiwordHit(t *testing.T) {
	d := loadTestDict(t)
	if got, ok := d.Lookup("gas station"); !ok || got != "加油站" {
		t.Errorf("Lookup(gas station) = (%q, %v)，期望 加油站", got, ok)
	}
	if got, ok := d.Lookup("speed limit"); !ok || got != "限速" {
		t.Errorf("Lookup(speed limit) = (%q, %v)，期望 限速", got, ok)
	}
}

// ── 后处理补译 ──

func TestFixLeftoverShorthand(t *testing.T) {
	d := loadTestDict(t)
	// LLM 返回里残留未翻译的 "wtf"
	got := d.FixLeftoverShorthand("wtf 你在干嘛")
	if !contains(got, "什么鬼") {
		t.Errorf("FixLeftoverShorthand 未补译 wtf，得到 %q", got)
	}
}

// ── @mention 保留 ──

func TestPreserveMentionPrefix(t *testing.T) {
	if got := PreserveMentionPrefix("@Player123 hello", "你好"); got != "@Player123 你好" {
		t.Errorf("PreserveMentionPrefix = %q，期望 @Player123 你好", got)
	}
}

// ── 未翻译校验（竞速跳过无效结果的判据）──

func TestLooksUntranslatedZhTarget(t *testing.T) {
	if !LooksUntranslated("hello world", "hello world", "zh-CN") {
		t.Error("原文=译文 应判定为未翻译")
	}
	if !LooksUntranslated("hello world", "   ", "zh-CN") {
		t.Error("空译文 应判定为未翻译")
	}
	if LooksUntranslated("hello world", "你好世界", "zh-CN") {
		t.Error("已译为中文 不应判定为未翻译")
	}
	// 目标不是中文时，中文缺失不构成失败
	if LooksUntranslated("hello", "bonjour", "fr") {
		t.Error("法语目标不应因缺中文而判定失败")
	}
}

// ── 源语言检测 ──

func TestGuessSourceLanguage(t *testing.T) {
	cases := map[string]string{
		"привет как дела": "ru",
		"merhaba nasılsın": "tr",
		"hello there":      "en",
	}
	for in, want := range cases {
		if got := GuessSourceLanguage(in); got != want {
			t.Errorf("GuessSourceLanguage(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ── 非文字内容跳过 ──

func TestIsNonTranslatable(t *testing.T) {
	skip := []string{".", "...", "123", "", "!?", "😀😀", "-"}
	for _, s := range skip {
		if !NonTranslatable(s) {
			t.Errorf("NonTranslatable(%q) 应为 true（跳过）", s)
		}
	}
	keep := []string{"hello", "你好", "1 sec", "wtf"}
	for _, s := range keep {
		if NonTranslatable(s) {
			t.Errorf("NonTranslatable(%q) 应为 false（需翻译）", s)
		}
	}
}

// ── 以下为 V2 新增：覆盖 V1 未测但已确认存在的行为 ──

// 重复字母压缩（_squeeze_repeated）的真实边界——已用 V1 实测逐条确认：
//
//	作用于「短语表」：  thank youuu / goood luck / have fuuun  → 命中
//	不作用于俚语 token：sryyy 压缩后是 sry，但 sry 只在 slang 表里 → **未命中**
//	                    而 srry / srrry 本身就是 slang 的直接键 → 命中
//
// 这个不对称是 V1 的真实行为，移植时必须原样保留。若"顺手修好"让 sryyy 命中，
// 就偏离了首要原则 P1（功能对齐）——用户换到 V2 后行为会变。
func TestSqueezeRepeatedLetters(t *testing.T) {
	d := loadTestDict(t)

	// slang 直接键
	for _, in := range []string{"sry", "srry", "srrry"} {
		if got, ok := d.Lookup(in); !ok || got != "抱歉" {
			t.Errorf("Lookup(%q) = (%q, %v)，期望 抱歉", in, got, ok)
		}
	}

	// V1 同样未命中的边界（压缩不覆盖 slang 表）
	for _, in := range []string{"sryyy", "srryy"} {
		if got, ok := d.Lookup(in); ok {
			t.Errorf("Lookup(%q) = (%q, true)：V1 此处未命中，V2 不应命中", in, got)
		}
	}

	// 压缩层在短语表上确实生效
	phraseCases := map[string]string{
		"thank you":   "谢谢",
		"thank youuu": "谢谢",
		"goood luck":  "祝好运",
		"have fuuun":  "玩得开心",
	}
	for in, want := range phraseCases {
		if got, ok := d.Lookup(in); !ok || got != want {
			t.Errorf("Lookup(%q) = (%q, %v)，期望 %q（压缩重复字母）", in, got, ok, want)
		}
	}
}

// 全 token 可译（2~5 个词且每个词都在俚语表里）→ 用「，」连接。
func TestAllTokensTranslatable(t *testing.T) {
	d := loadTestDict(t)
	got, ok := d.Lookup("ty bro")
	if !ok || got != "谢谢，兄弟" {
		t.Errorf("Lookup(ty bro) = (%q, %v)，期望 谢谢，兄弟", got, ok)
	}
}

// 规范化：全角字母、零宽字符、首尾标点都应被正确处理。
func TestNormalization(t *testing.T) {
	d := loadTestDict(t)
	for _, in := range []string{"WTF", "wtf!", "  wtf  ", "ｗｔｆ", "wtf\u200b"} {
		if got, ok := d.Lookup(in); !ok || got != "什么鬼" {
			t.Errorf("Lookup(%q) = (%q, %v)，期望 什么鬼", in, got, ok)
		}
	}
}

func TestPauseAfterIsPresent(t *testing.T) {
	// pauseAfter 决定结构化短语的拼接边界，缺失会让译文错位（对照表 10.1）
	d := loadTestDict(t)
	e, ok := d.SlangEntry("wtf")
	if !ok {
		t.Fatal("slang 缺少 wtf")
	}
	if e.PauseAfter == nil {
		t.Fatal("wtf 缺少 pauseAfter 字段——词库文件被改坏了")
	}
	if !e.Pause() {
		t.Error("wtf 的 pauseAfter 应为 true")
	}
	if e2, ok := d.SlangEntry("pls"); ok && e2.Pause() {
		t.Error("pls 的 pauseAfter 应为 false")
	}
}

// 系统消息关键词回退。
func TestSystemMessageFallback(t *testing.T) {
	d := loadTestDict(t)
	got, ok := d.Lookup("cannot connect to server")
	if !ok {
		t.Fatal("系统消息未命中关键词回退")
	}
	if !contains(got, "无法连接") {
		t.Errorf("系统消息译文异常: %q", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
