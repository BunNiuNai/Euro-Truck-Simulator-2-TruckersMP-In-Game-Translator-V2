// Package engine 翻译编排：批处理、混合语言拆分、语言检测、出口后处理。
//
// V1 来源：translator.py（Translator.run / _flush / _flush_llm / split_mixed_text / reassemble_mixed / detect_language）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 批处理窗口 0.3s，满 8 条立即 flush
//   · 批量分隔符为 \n---\n；LLM 未按分隔符回显时必须逐条回退翻译（不可回退此修复）
//   · 混合语言拆分：中文片段保留不译
//   · 非文字内容（标点/数字/emoji）直接跳过
//   · 出口后处理顺序：补译缩写 → 保留 @玩家名 → 未翻译校验（失败则回退下一家）
package engine
