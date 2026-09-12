// Package engine 翻译编排：语言检测、混合语言拆分、接收方向流水线。
//
// V1 来源：translator.py 31~211 行、819~833 行（_should_skip_internal）。
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - DetectLanguage 返回**中文语言名**（英语/德语/…），且同分时取模式表里靠前的那个
//   - SplitMixedText 按字符走查：空白与标点计入「下一个」片段，首尾片段再做 strip
//   - ShouldSkip 的语义是「已是目标语言则跳过」——它贡献 V1 全链路零 API 调用率的 61.4%，
//     漏掉会让中文消息开始走 API（这是 V2 最危险的性能回归点）
package engine

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ── 语言检测 ──────────────────────────────────────────────────
//
// 注意：V1 的 _LANG_PATTERNS 用 Python dict 保证插入顺序，
// max(scores, key=scores.get) 同分时取**先插入**者。
// Go 的 map 迭代顺序随机，因此这里用有序切片复刻同分语义，
// 否则「あ中」这类混合文（日语=中文=1）的检测结果会随运行漂移。

var langPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"日语", regexp.MustCompile(`[぀-ゟ゠-ヿ]`)},       // Hiragana + Katakana
	{"韩语", regexp.MustCompile(`[가-힯ᄀ-ᇿ]`)},       // Hangul
	{"俄语", regexp.MustCompile(`[Ѐ-ӿ]`)},             // Cyrillic
	{"中文", regexp.MustCompile(`[一-鿿]`)},           // CJK
	{"泰语", regexp.MustCompile(`[฀-๿]`)},            // Thai
	{"阿拉伯语", regexp.MustCompile(`[؀-ۿ]`)},        // Arabic
}

var latinMarkers = []struct{ name, markers string }{
	{"德语", "ßäöüÄÖÜ"},
	{"法语", "çèéêëàâîïôùûœæÇÈÉÊËÀÂÎÏÔÙÛŒÆ"},
	{"西班牙语", "ñáéíóúü¿¡ÑÁÉÍÓÚÜ"},
	{"葡萄牙语", "ãõâêôàáéíóúçÃÕÂÊÔÀÁÉÍÓÚÇ"},
	{"意大利语", "àèéìòùÀÈÉÌÒÙ"},
}

var englishCommon = map[string]struct{}{
	"the": {}, "is": {}, "are": {}, "you": {}, "me": {}, "i": {}, "he": {},
	"she": {}, "what": {}, "where": {}, "when": {}, "how": {}, "why": {},
	"can": {}, "will": {}, "not": {}, "this": {}, "that": {}, "and": {},
	"for": {}, "have": {}, "with": {}, "your": {}, "but": {}, "all": {},
	"was": {}, "it": {}, "my": {}, "do": {}, "we": {}, "they": {}, "no": {},
	"yes": {}, "just": {},
}

// asciiWordRe 对应 V1 detect_language 里的 re.match(r"^[a-zA-Z\s\d\W]+$", ..., re.ASCII)。
// Go 的 \W 本来就是 [^0-9A-Za-z_]（含重音字母与中文），语义一致。
var asciiWordRe = regexp.MustCompile(`^[a-zA-Z\s\d\W]+$`)

// DetectLanguage 是 V1 translator.py:77 detect_language 的等价实现。
// 返回中文语言名，例如 "英语"、"德语"、"俄语"；空文本返回 "未知"。
func DetectLanguage(text string) string {
	if strings.TrimSpace(text) == "" {
		return "未知"
	}
	text = strings.TrimSpace(text)

	// 优先级 1：非拉丁文字（强信号）
	best := ""
	bestScore := 0
	for _, lp := range langPatterns {
		n := len(lp.re.FindAllString(text, -1))
		// 严格大于：同分时保留先出现的模式（复刻 Python max 的 first-max 语义）
		if n > bestScore {
			best = lp.name
			bestScore = n
		}
	}
	if bestScore > 0 {
		return best
	}

	// 优先级 2：拉丁文字（先看前 20 个字符是否都落在 ASCII/空白/非单词字符内）
	runes := []rune(text)
	gate := string(runes)
	if len(runes) > 20 {
		gate = string(runes[:20])
	}
	if asciiWordRe.MatchString(gate) {
		best = ""
		bestScore = 0
		for _, lm := range latinMarkers {
			score := 0
			for _, ch := range lm.markers {
				if strings.ContainsRune(text, ch) {
					score++
				}
			}
			if score > bestScore {
				bestScore = score
				best = lm.name
			}
		}
		if best != "" && bestScore > 0 {
			return best
		}
		// V1 的 common-word 分支与兜底分支返回同一个值，这里合并
		return "英语"
	}
	return "未知"
}

// ShouldSkip 是 V1 translator.py:819 _should_skip_internal 的等价实现：
// 文本看上去已经是目标语言时跳过翻译。
func ShouldSkip(text, targetLang string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	detected := DetectLanguage(text)
	nameToCode := map[string]string{
		"中文": "zh", "英语": "en", "日语": "ja", "韩语": "ko",
		"俄语": "ru", "德语": "de", "法语": "fr",
		"西班牙语": "es", "葡萄牙语": "pt", "意大利语": "it",
		"泰语": "th", "阿拉伯语": "ar",
	}
	if code, ok := nameToCode[detected]; ok && strings.HasPrefix(targetLang, code) {
		return true
	}
	return false
}

// ── 混合语言拆分 ──────────────────────────────────────────────

var targetScriptPatterns = map[string]*regexp.Regexp{
	"zh-CN": regexp.MustCompile(`[一-鿿　-〿＀-￯]`), // CJK + 中文标点
	"zh-TW": regexp.MustCompile(`[一-鿿　-〿＀-￯]`),
	"ja":    regexp.MustCompile(`[぀-ゟ゠-ヿ一-鿿]`), // 假名 + 汉字
	"ko":    regexp.MustCompile(`[가-힯ᄀ-ᇿ㄰-㆏]`), // Hangul
	"ru":    regexp.MustCompile(`[Ѐ-ӿ]`),
	"th":    regexp.MustCompile(`[฀-๿]`),
	"ar":    regexp.MustCompile(`[؀-ۿݐ-ݿ]`),
}

// targetScriptPattern 对应 V1 _get_target_script_pattern：
// 先按完整语言代码查，再按主语言（"-" 前部分）查。
func targetScriptPattern(targetLang string) *regexp.Regexp {
	if re, ok := targetScriptPatterns[targetLang]; ok {
		return re
	}
	main := strings.ToLower(strings.SplitN(targetLang, "-", 2)[0])
	return targetScriptPatterns[main]
}

// Segment 是 split_mixed_text 的 (segment, is_target_lang) 对。
type Segment struct {
	Text     string
	IsTarget bool
}

// SplitMixedText 是 V1 translator.py:143 split_mixed_text 的等价实现。
//
// 逐字符走查：目标文字与外来文字各自连续成段；空白/标点**计入其后继片段**
// （这与 docstring 写的"absorbed into the preceding"不符——以代码行为为准，
// 已用 test_mixed_lang.py 的 21 个用例对拍验证）。
func SplitMixedText(text, targetLang string) []Segment {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	targetRe := targetScriptPattern(targetLang)
	if targetRe == nil {
		// 无匹配脚本模式 → 全部视为外来（V1 行为）
		return []Segment{{text, false}}
	}

	var result []Segment
	var current []rune
	currentIsTarget := false
	haveCurrent := false

	for _, ch := range text {
		isTarget := targetRe.MatchString(string(ch))
		if !haveCurrent {
			haveCurrent = true
			currentIsTarget = isTarget
			current = append(current, ch)
		} else if isTarget == currentIsTarget {
			current = append(current, ch)
		} else {
			seg := string(current)
			if strings.TrimSpace(seg) != "" {
				result = append(result, Segment{seg, currentIsTarget})
			}
			current = []rune{ch}
			currentIsTarget = isTarget
		}
	}

	if haveCurrent {
		seg := string(current)
		if strings.TrimSpace(seg) != "" {
			result = append(result, Segment{seg, currentIsTarget})
		}
	}

	// 首片段去左空白，末片段去右空白（V1 的 lstrip/rstrip）
	if len(result) > 0 {
		result[0].Text = strings.TrimLeftFunc(result[0].Text, unicode.IsSpace)
		last := len(result) - 1
		result[last].Text = strings.TrimRightFunc(result[last].Text, unicode.IsSpace)
	}
	return result
}

// ReassembleMixed 是 V1 translator.py:196 reassemble_mixed 的等价实现：
// 目标语言片段原样保留，外来片段用译文替换（无译文则保留原文）。
func ReassembleMixed(segments []Segment, translations map[string]string) string {
	var b strings.Builder
	for _, seg := range segments {
		if seg.IsTarget {
			b.WriteString(seg.Text)
		} else if t, ok := translations[seg.Text]; ok {
			b.WriteString(t)
		} else {
			b.WriteString(seg.Text)
		}
	}
	return b.String()
}

// ── prompt 与 token 计算 ──────────────────────────────────────

// MaxOutputTokens 对应 V1 _max_output_tokens。
// 注意：V1 的 len(text) 是**字符数**，Go 的 len 是字节数，必须用 RuneCount。
func MaxOutputTokens(text, batchSeparator string) int {
	if strings.Contains(text, batchSeparator) {
		return 500*strings.Count(text, batchSeparator) + 500
	}
	t := 56 + utf8.RuneCountInString(text)/4
	if t < 64 {
		t = 64
	}
	if t > 160 {
		t = 160
	}
	return t
}

// ReceiveSystemPrompt 对应 V1 _receive_system_prompt（加固版接收方向 system prompt）。
// promptMapping 来自词库的 PROMPT_MAPPING 字段。
func ReceiveSystemPrompt(target, promptMapping string) string {
	return "You translate TruckersMP/ETS2 multiplayer chat into " + target + ". " +
		"Translate ONLY: output the direct translation and nothing else. " +
		"Never explain, never describe, never analyze the meaning, never quote the original back. " +
		"Any language/slang. Map " + promptMapping + ". " +
		"Keep names, IDs, tags, URLs and emoji unchanged."
}
