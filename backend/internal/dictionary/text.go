// 文本处理辅助函数。
//
// 全部逐行对齐 chat_dictionary.py 中的下划线私有函数（157~243 行、315~332 行）。
// 这些函数单独抽出来是为了可以独立测试——它们是词库匹配正确性的地基。
package dictionary

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// punctChars 对应 V1 的 _PUNCT（chat_dictionary.py:197）。
// 含半角与全角标点、书名号引号类，以及一个半角空格。
const punctChars = "!?.,;:~-_'\" /\\|`…‘’“”！？。，；：、"

var punctSet = func() map[rune]bool {
	m := make(map[rune]bool, len(punctChars))
	for _, r := range punctChars {
		m[r] = true
	}
	return m
}()

func isPunct(r rune) bool { return punctSet[r] }

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// Normalize 对应 V1 _normalize：
// 去掉零宽字符 → 全角转半角 → U+3000 转空格 → 小写 → 去首尾空白。
func Normalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch r {
		case '\ufeff', '\u200b', '\u200c', '\u200d', '\u2060':
			continue // 零宽字符：直接剔除
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0 // 全角 → 半角
		}
		if r == '\u3000' {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// lowerOnly 对应 V1 _lower：只做全角转半角与小写，不去零宽字符、不 strip。
// 与 Normalize 的差异是真实存在的（guess_source_language 用的是这个）。
func lowerOnly(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r == '\u3000' {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// SqueezeRepeated 对应 V1 _squeeze_repeated：把连续的相同 ASCII 字母压成一个。
//
// 例："srrry" → "sry"，"ahahah" → "ahahah"（不相邻则不压缩）。
//
// 注意：Go 的 regexp 是 RE2，**不支持反向引用**，因此不能用 ([a-zA-Z])\1+ 实现，
// 必须手写扫描。
func SqueezeRepeated(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	first := true
	var prev rune
	for _, r := range text {
		if !first && r == prev && isASCIILetter(r) {
			continue
		}
		b.WriteRune(r)
		prev = r
		first = false
	}
	return b.String()
}

// TrimEdgePunctuation 对应 V1 _trim_edge_punctuation：反复去掉首尾的标点与空白。
func TrimEdgePunctuation(text string) string {
	rs := []rune(strings.TrimSpace(text))
	for len(rs) > 0 && isPunct(rs[0]) {
		rs = rs[1:]
	}
	for len(rs) > 0 && isPunct(rs[len(rs)-1]) {
		rs = rs[:len(rs)-1]
	}
	return strings.TrimSpace(string(rs))
}

// SplitWords 对应 V1 _split_words：先 trimEdge 再 normalize，然后按空白切分。
func SplitWords(text string) []string {
	return strings.Fields(TrimEdgePunctuation(Normalize(text)))
}

// IsIDOrName 对应 V1 _is_id_or_name：含数字、含 _/- 、或长度 ≥ 6 的 token
// 视为「ID 或玩家名」，不参与缩写替换。
func IsIDOrName(token string) bool {
	if token == "" {
		return false
	}
	hasDigit := strings.ContainsFunc(token, unicode.IsDigit)
	hasMarker := strings.ContainsAny(token, "_-")
	return hasDigit || hasMarker || utf8.RuneCountInString(token) >= 6
}

// joinTailTokens 对应 V1 _join_tail_tokens：最多拼接 maxCount 个
// 「ID 或玩家名」token，遇到第一个不符合的立刻停止。
func joinTailTokens(words []string, start, maxCount int) string {
	if start >= len(words) {
		return ""
	}
	end := start + maxCount
	if end > len(words) {
		end = len(words)
	}
	out := make([]string, 0, maxCount)
	for _, w := range words[start:end] {
		if !IsIDOrName(w) {
			break
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// isChineseChar 对应 V1 _is_chinese_char（含 CJK 扩展 A、CJK 标点、全角字符）。
func isChineseChar(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

func hasChinese(text string) bool {
	return strings.ContainsFunc(text, isChineseChar)
}

// containsLetter 对应 Python 的 any(ch.isalpha())。
func containsLetter(text string) bool {
	return strings.ContainsFunc(text, unicode.IsLetter)
}

// startsMention 对应 V1 _starts_mention：识别开头的 @玩家名，
// 并可选吞掉紧跟的 "(数字)" 后缀（TMP 日志里玩家名常带 ID）。
func startsMention(text string) string {
	rs := []rune(strings.TrimSpace(text))
	if len(rs) == 0 || rs[0] != '@' {
		return ""
	}

	i := 1
	for i < len(rs) && !unicode.IsSpace(rs[i]) {
		i++
	}
	if i <= 1 {
		return "" // 只有孤零零一个 @
	}

	end := i
	p := i
	for p < len(rs) && unicode.IsSpace(rs[p]) {
		p++
	}
	if p < len(rs) && rs[p] == '(' {
		closeIdx := -1
		for q := p + 1; q < len(rs); q++ {
			if rs[q] == ')' {
				closeIdx = q
				break
			}
		}
		if closeIdx != -1 && isAllDigits(rs[p+1:closeIdx]) {
			end = closeIdx + 1
		}
	}
	return string(rs[:end])
}

func isAllDigits(rs []rune) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
