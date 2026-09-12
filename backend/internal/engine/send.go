// 发送方向的翻译。对应 V1 `translator.py:806-927` 的 translate_for_send 一族。
//
// ⚠️ 为什么不能拿接收方向的 Translator 冒充：
//   - **目标语言不同**：这里是 `cfg.send_target_language`（默认 en），
//     接收方向是 `cfg.target_language`
//   - **system prompt 不同**：这里是
//     "You translate into {语言名}. Output only the translation, no quotes,
//      no explanations."（V1 translator.py:839）
//   - **处理也不同**：发送方向**不做**混合语言分段、不做本地词典拦截、
//     不查缓存——V1 的 translate_for_send 里这几样都没有
//
// 用错了的后果很具体：玩家打一句中文想发出去，结果被「翻译」成中文发进聊天框。
package engine

import (
	"context"
	"strings"
	"sync"
)

// SendLangNames 是「语言代码 → 中文语言名」，与 V1 translator.py:812 的
// `_SEND_LANG_NAMES` 一字不差。发送方向的 prompt 是中文写的，所以这里要中文名。
var SendLangNames = map[string]string{
	"en": "英语", "ja": "日语", "ko": "韩语",
	"de": "德语", "fr": "法语", "es": "西班牙语",
	"ru": "俄语", "pt": "葡萄牙语", "it": "意大利语",
}

// SendLangName 返回目标语言的中文名。
// 未知代码与 V1 一样退回「英语」（`_SEND_LANG_NAMES.get(target_lang, "英语")`）。
func SendLangName(code string) string {
	if name, ok := SendLangNames[strings.ToLower(strings.TrimSpace(code))]; ok {
		return name
	}
	return "英语"
}

// SendSystemPrompt 是发送方向的 system prompt，逐字对齐 V1 translator.py:839。
func SendSystemPrompt(target string) string {
	return "You translate into " + SendLangName(target) +
		". Output only the translation, no quotes, no explanations."
}

// SendTranslator 是发送方向的翻译器。
//
// 刻意与 Translator 分开而不是加个 mode 参数：两者的目标语言、prompt 与
// 整条处理链路都不同，混在一个类型里迟早会有人用错那半边。
type SendTranslator struct {
	// Target 对应 cfg.send_target_language
	//
	// ⚠️ 运行期一律走 SetTarget / targetNow，不要直接读写这个字段。
	// 与接收方向 Translator.Target 同一条约束：字符串是两个机器字
	// （指针 + 长度），一边改一边读会读到「旧指针 + 新长度」这种撕裂值，
	// 随后就是越界访问或崩溃。
	Target string
	LLM    LLMCaller

	targetMu sync.Mutex
}

// SetTarget 在锁保护下切换发送方向的目标语言（配置热重载时调用）。
//
// 发送方向的目标语言与接收方向是**两套**（V1 translator.py:906-927），
// 用户改了 `send_target_language` 必须同步推到这里，否则要重启才生效——
// 界面上写着「已保存」，发出去的还是旧语言的译文。
func (t *SendTranslator) SetTarget(lang string) {
	t.targetMu.Lock()
	t.Target = lang
	t.targetMu.Unlock()
}

// targetNow 读取当前目标语言（与 SetTarget 配对）。
func (t *SendTranslator) targetNow() string {
	t.targetMu.Lock()
	defer t.targetMu.Unlock()
	return t.Target
}

// Translate 把玩家输入译成要发出去的文本。
//
// 空文本或已经就是目标语言的文本原样返回（V1 translator.py:909-913）。
// LLM 失败时返回 error——对应 V1 抛出的「所有 Provider 发送翻译失败」，
// 由 compose 转成 FAIL_TRANSLATION。
func (t *SendTranslator) Translate(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}
	if ShouldSkip(text, t.targetNow()) {
		return text, nil
	}
	if t.LLM == nil {
		return "", errNoLLM
	}

	out, _, _, err := t.LLM.Call(ctx, text)
	if err != nil {
		return "", err
	}
	// V1 对每个 Provider 的返回都做 strip（translator.py:877），照抄：
	// 模型常会带上首尾空白或换行，直接发进游戏会多一个空格。
	return strings.TrimSpace(out), nil
}

// noLLMError 是没有可用 Provider 时的错误。
//
// 与 V1 的行为对齐：V1 在没有启用 Provider 时会退到 legacy 单 Provider 路径，
// 而 V2 已按 ADR 移除了那条路径（README 里记的「V1 v2.2.0 起回归纯 LLM」）。
// 所以这里如实报错，而不是悄悄发一句没翻译的中文出去。
type noLLMError struct{}

func (noLLMError) Error() string { return "没有可用的 Provider，无法翻译要发送的内容" }

var errNoLLM = noLLMError{}
