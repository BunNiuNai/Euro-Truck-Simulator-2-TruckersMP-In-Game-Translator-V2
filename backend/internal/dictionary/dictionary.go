// Package dictionary 五层术语处理与 resources/dictionary.json 加载校验。
//
// V1 来源：chat_dictionary.py（399 行）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - Lookup 的匹配顺序不可调换，见下方注释（与 V1 源码逐行对齐）
//   - pauseAfter 必须保留，它决定结构化短语的拼接边界
//   - 加载校验为条目级：非法项跳过并记 WARN，不使整个词库失效
//   - 三层合并优先级：用户覆盖 > 在线更新 > 内置
package dictionary

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Entry 是俚语条目。V1 的 SLANG_TOKENS 值为 tuple[译文, pauseAfter]。
type Entry struct {
	Text string `json:"text"`

	// PauseAfter 为指针以区分「显式 false」与「字段缺失」。
	// 缺失时记 WARN 并按 false 处理——不跳过条目，否则会丢掉一个译文。
	PauseAfter *bool `json:"pauseAfter"`
}

// Pause 返回 pauseAfter，缺失时为 false。
func (e Entry) Pause() bool { return e.PauseAfter != nil && *e.PauseAfter }

// SystemMessage 对应 V1 的 SYSTEM_MESSAGE_FALLBACK 条目（key 为关键词元组）。
type SystemMessage struct {
	Patterns []string `json:"patterns"`
	Text     string   `json:"text"`
}

// Bundle 是 resources/dictionary.json 的映射。
type Bundle struct {
	SchemaVersion int `json:"schemaVersion"`
	Meta          struct {
		GeneratedFrom string `json:"generatedFrom"`
		GeneratedAt   string `json:"generatedAt"`
		Note          string `json:"note"`
	} `json:"meta"`

	Slang             map[string]Entry  `json:"slang"`
	Phrases           map[string]string `json:"phrases"`
	StructuredActions map[string]string `json:"structuredActions"`
	SystemMessages    []SystemMessage   `json:"systemMessages"`
	ETS2Terms         map[string]string `json:"ets2Terms"`
	PromptMapping     string            `json:"promptMapping"`
}

// Dictionary 是加载完成、可直接查询的词库。
type Dictionary struct {
	bundle Bundle

	// squeezedPhrases 预计算「压缩重复字母后」的短语索引，对应 V1 的
	// _squeeze_repeated 线性扫描；预计算避免每次查询遍历全表。
	squeezedPhrases map[string]string

	// Warnings 记录加载期的条目级问题（非法项、缺字段等），不阻断加载。
	Warnings []string
}

// Load 按 ADR-006 的三层顺序加载并合并词库：后者覆盖前者。
//
// 传入顺序即优先级，例如：
//
//	Load("resources/dictionary.json",          // 内置（只读）
//	     "<data>/resources/dictionary.json",   // 在线更新
//	     "<data>/resources/override/a.json")   // 用户覆盖，永不参与更新
func Load(paths ...string) (*Dictionary, error) {
	d := &Dictionary{
		squeezedPhrases: map[string]string{},
		bundle: Bundle{
			Slang:             map[string]Entry{},
			Phrases:           map[string]string{},
			StructuredActions: map[string]string{},
			ETS2Terms:         map[string]string{},
		},
	}

	loaded := 0
	for _, p := range paths {
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 可选层缺失是正常的
			}
			return nil, fmt.Errorf("读取词库 %s: %w", p, err)
		}
		var b Bundle
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("解析词库 %s: %w", p, err)
		}
		if b.SchemaVersion == 0 {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: 缺少 schemaVersion，已跳过", p))
			continue
		}
		d.merge(&b, p)
		loaded++
	}

	if loaded == 0 {
		return nil, fmt.Errorf("未加载到任何词库（检查 resources/dictionary.json 是否存在）")
	}
	if d.bundle.SchemaVersion == 0 {
		return nil, fmt.Errorf("词库 schemaVersion 无效")
	}

	for token, e := range d.bundle.Slang {
		if e.PauseAfter == nil {
			d.Warnings = append(d.Warnings,
				fmt.Sprintf("slang[%s] 缺少 pauseAfter，按 false 处理", token))
		}
	}
	d.buildSqueezeIndex()
	return d, nil
}

// merge 把 b 合并进 d，b 的值优先。systemMessages 采用「后加载者前置」，
// 使覆盖层的规则先被匹配到（否则内置规则永远先命中，覆盖就失效了）。
func (d *Dictionary) merge(b *Bundle, path string) {
	d.bundle.SchemaVersion = b.SchemaVersion
	if b.Meta.GeneratedFrom != "" {
		d.bundle.Meta = b.Meta
	}

	for k, v := range b.Slang {
		d.bundle.Slang[k] = v
	}
	for k, v := range b.Phrases {
		d.bundle.Phrases[k] = v
	}
	for k, v := range b.StructuredActions {
		d.bundle.StructuredActions[k] = v
	}
	for k, v := range b.ETS2Terms {
		d.bundle.ETS2Terms[k] = v
	}
	if len(b.SystemMessages) > 0 {
		prepended := make([]SystemMessage, 0, len(b.SystemMessages)+len(d.bundle.SystemMessages))
		prepended = append(prepended, b.SystemMessages...)
		prepended = append(prepended, d.bundle.SystemMessages...)
		d.bundle.SystemMessages = prepended
	}
	if b.PromptMapping != "" {
		d.bundle.PromptMapping = b.PromptMapping
	}

	// 条目级校验：patterns 为空的系统消息会被丢弃（它永远匹配不到，留着只会误导）
	kept := d.bundle.SystemMessages[:0]
	for i, sm := range d.bundle.SystemMessages {
		if len(sm.Patterns) == 0 || sm.Text == "" {
			d.Warnings = append(d.Warnings,
				fmt.Sprintf("%s: systemMessages[%d] 结构不合法，已跳过", path, i))
			continue
		}
		kept = append(kept, sm)
	}
	d.bundle.SystemMessages = kept
}

// buildSqueezeIndex 预计算压缩索引。
//
// 注意：Go 的 map 迭代顺序是随机的，而 V1 的 Python dict 是有序的，因此当两条
// 短语压缩后相同时（如 haha/hahaha），结果必须显式确定。这里按 key 排序后取第一个，
// 保证跨进程、跨平台结果一致。
func (d *Dictionary) buildSqueezeIndex() {
	keys := make([]string, 0, len(d.bundle.Phrases))
	for k := range d.bundle.Phrases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sq := SqueezeRepeated(k)
		if _, exists := d.squeezedPhrases[sq]; exists {
			continue
		}
		d.squeezedPhrases[sq] = d.bundle.Phrases[k]
	}
}

// Counts 返回各层条目数，供启动日志与测试使用。
func (d *Dictionary) Counts() (slang, phrases, structured, system, ets2 int) {
	return len(d.bundle.Slang), len(d.bundle.Phrases), len(d.bundle.StructuredActions),
		len(d.bundle.SystemMessages), len(d.bundle.ETS2Terms)
}

// PromptMapping 对应 V1 的 PROMPT_MAPPING（第 4 层，内嵌进 prompt）。
func (d *Dictionary) PromptMapping() string { return d.bundle.PromptMapping }

// SlangEntry 按 token 查询俚语条目，供测试与未来的词库管理 UI 使用。
func (d *Dictionary) SlangEntry(token string) (Entry, bool) {
	e, ok := d.bundle.Slang[token]
	return e, ok
}

// ETSTerm 按词查询 ETS2 专有词汇，供测试与未来的词库管理 UI 使用。
func (d *Dictionary) ETSTerm(term string) (string, bool) {
	v, ok := d.bundle.ETS2Terms[term]
	return v, ok
}

// Meta 返回加载到的元信息，便于在启动日志里记录词库版本。
func (d *Dictionary) Meta() (from, at string) {
	return d.bundle.Meta.GeneratedFrom, d.bundle.Meta.GeneratedAt
}

// Lookup 是 V1 short_phrase_fallback 的等价实现：命中返回译文与 true，
// 未命中返回空串与 false（此时才需要调用 LLM）。
//
// 匹配顺序**必须**与 V1 一致，不可调换：
//
//	1) 完整短语精确匹配：phrases → ets2Terms
//	2) 压缩重复字母后再匹配一次 phrases（srry→sry 这类）
//	3) 单词级匹配（仅当恰好一个词）：slang → ets2Terms
//	4) 结构化短语（前 3 个词位尝试 rec ban / report X / ban X …）
//	5) 系统消息关键词包含匹配
//	6) 全 token 可译（2~5 个词且每个词都在 slang 中）→ 用「，」连接
func (d *Dictionary) Lookup(text string) (string, bool) {
	lower := Normalize(text)
	if lower == "" {
		return "", false
	}
	edge := TrimEdgePunctuation(lower)

	// 1) 完整短语精确匹配
	if v, ok := d.bundle.Phrases[edge]; ok {
		return v, true
	}
	if v, ok := d.bundle.ETS2Terms[edge]; ok {
		return v, true
	}

	// 2) 压缩重复字母后再匹配
	if v, ok := d.squeezedPhrases[SqueezeRepeated(edge)]; ok {
		return v, true
	}

	words := SplitWords(lower)

	// 3) 单词级匹配
	if len(words) == 1 {
		if e, ok := d.bundle.Slang[words[0]]; ok {
			return e.Text, true
		}
		if v, ok := d.bundle.ETS2Terms[words[0]]; ok {
			return v, true
		}
	}

	// 4) 结构化短语（V1 只在前 3 个词位尝试）
	limit := len(words)
	if limit > 3 {
		limit = 3
	}
	for start := 0; start < limit; start++ {
		if s := d.structuredPhrase(words, start); s != "" {
			return s, true
		}
	}

	// 5) 系统消息关键词
	for _, sm := range d.bundle.SystemMessages {
		for _, k := range sm.Patterns {
			if strings.Contains(lower, k) {
				return sm.Text, true
			}
		}
	}

	// 6) 全 token 可译（2~5 个词）
	if len(words) > 1 && len(words) <= 5 {
		trans := make([]string, 0, len(words))
		ok := true
		for _, w := range words {
			e, found := d.bundle.Slang[w]
			if !found {
				ok = false
				break
			}
			trans = append(trans, e.Text)
		}
		if ok && len(trans) > 0 {
			return strings.Join(trans, "，"), true
		}
	}

	return "", false
}

// structuredPhrase 对应 V1 _structured_truckers_phrase。
func (d *Dictionary) structuredPhrase(words []string, start int) string {
	if start >= len(words) {
		return ""
	}
	w := words[start]

	// 特例：rec ban → 已录屏，等封禁
	if w == "rec" && start+1 < len(words) && words[start+1] == "ban" {
		out := "已录屏，等封禁"
		if tail := joinTailTokens(words, start+2, 2); tail != "" {
			return out + " " + tail
		}
		return out
	}

	if action, ok := d.bundle.StructuredActions[w]; ok {
		if tail := joinTailTokens(words, start+1, 2); tail != "" {
			return action + " " + tail
		}
		return action
	}
	return ""
}

// NonTranslatable 对应 V1 is_non_translatable：
// 纯标点/数字/emoji 等「没有任何字母」的内容直接跳过，不调用 LLM。
func NonTranslatable(text string) bool {
	v := TrimEdgePunctuation(strings.TrimSpace(text))
	if v == "" {
		return true
	}
	return !containsLetter(v)
}

// LooksUntranslated 对应 V1 looks_untranslated：判断 LLM 返回是否失败。
//
// 这是多模型竞速「跳过无效结果」的判据，移植时必须逐字保留。
func LooksUntranslated(input, output, targetLang string) bool {
	out := strings.TrimSpace(output)
	if out == "" {
		return true
	}
	if strings.ToLower(strings.TrimSpace(input)) == strings.ToLower(out) {
		return true
	}
	switch targetLang {
	case "zh-CN", "zh", "zh-Hans":
		if !hasChinese(out) {
			return true
		}
	}
	return false
}

// PreserveMentionPrefix 对应 V1 preserve_mention_prefix：保留 @玩家名 前缀。
func PreserveMentionPrefix(input, output string) string {
	prefix := startsMention(input)
	if prefix == "" || strings.Contains(output, prefix) {
		return output
	}
	return prefix + " " + strings.TrimSpace(output)
}

// FixLeftoverShorthand 对应 V1 fix_leftover_shorthand：
// 后处理补译 LLM 漏译的英文缩写。整条未改动时返回原文（V1 语义）。
func (d *Dictionary) FixLeftoverShorthand(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	out := make([]string, 0, len(words))
	changed := false

	for _, w := range words {
		token := TrimEdgePunctuation(strings.ToLower(w))
		e, ok := d.bundle.Slang[token]
		if ok && !strings.HasPrefix(w, "@") && !IsIDOrName(token) {
			suffix := ""
			if e.Pause() {
				suffix = "，"
			}
			out = append(out, e.Text+suffix)
			changed = true
			continue
		}
		out = append(out, w)
	}

	if !changed {
		return text
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// GuessSourceLanguage 对应 V1 guess_source_language（多语言启发式检测）。
func GuessSourceLanguage(text string) string {
	lower := lowerOnly(text)

	var cyrillic, greek, latin int
	for _, r := range lower {
		switch {
		case r >= 0x0400 && r <= 0x04FF:
			cyrillic++
		case r >= 0x0370 && r <= 0x03FF:
			greek++
		case r < 128 && isASCIILetter(r):
			latin++
		case r >= 0x00C0 && r <= 0x024F:
			latin++
		}
	}

	if cyrillic > 0 && cyrillic >= latin {
		switch {
		case strings.ContainsAny(lower, "іїєґ"):
			return "uk"
		case strings.ContainsAny(lower, "ћђљњџ"):
			return "sr"
		case strings.Contains(lower, "ъ"):
			return "bg"
		}
		return "ru"
	}
	if greek > 0 && greek >= latin {
		return "el"
	}
	switch {
	case strings.ContainsAny(lower, "ığşç"):
		return "tr"
	case strings.ContainsAny(lower, "äöüß"):
		return "de"
	case strings.ContainsAny(lower, "ąęłńóśźż"):
		return "pl"
	case strings.ContainsAny(lower, "ěščřžýáíéďťňů"):
		return "cs"
	case strings.ContainsAny(lower, "ăâîșţț"):
		return "ro"
	case strings.ContainsAny(lower, "ñ¿¡"):
		return "es"
	case strings.ContainsAny(lower, "ãõ"):
		return "pt"
	}
	return "en"
}
