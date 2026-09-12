/**
 * 语言映射表。
 *
 * 来源：V1 main.py:654-659 的 `_lang_map`（硬编码在设置对话框里）。
 * V2 抽成独立模块，因为悬浮窗的目标语言提示也要用同一份，不能再各写一遍。
 *
 * 顺序即下拉框的显示顺序，与 V1 保持一致。
 */
export const LANGUAGES: ReadonlyArray<{ label: string; code: string }> = [
  { label: '简体中文', code: 'zh-CN' },
  { label: '英语', code: 'en' },
  { label: '日语', code: 'ja' },
  { label: '韩语', code: 'ko' },
  { label: '法语', code: 'fr' },
  { label: '德语', code: 'de' },
  { label: '西班牙语', code: 'es' },
  { label: '俄语', code: 'ru' },
  { label: '葡萄牙语', code: 'pt' },
  { label: '意大利语', code: 'it' },
]

/** 语言代码 → 中文名。找不到时回退为代码本身（V1 用 `.get(code, "简体中文")`）。 */
export function labelOf(code: string): string {
  return LANGUAGES.find((l) => l.code === code)?.label ?? '简体中文'
}
