// Package dictionary 五层术语处理与 resources/dictionary.json 加载校验。
//
// V1 来源：chat_dictionary.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · Lookup 的匹配顺序**必须**与 V1 一致，不可调换（逐条见 Lookup 上方注释）：
//     ① 完整短语精确匹配 phrases → ets2Terms
//     ② 压缩重复字母后再匹配一次 phrases（srry→sry 这类）
//     ③ 单词级匹配（仅当恰好一个词）slang → ets2Terms
//     ④ 结构化短语（前 3 个词位尝试 rec ban / report X / ban X …）
//     ⑤ 系统消息关键词包含匹配
//     ⑥ 全 token 可译（2~5 个词且每个词都在 slang 中）→ 用「，」连接
//     ⚠️ 这里此前写的是"slang → phrases → structuredActions → promptMapping → ets2Terms"，
//     那是**五层术语处理的分类**，不是 Lookup 的匹配顺序；且 promptMapping 是嵌进
//     LLM prompt 里的映射，Lookup 根本不经手。两者混淆过一次，别再改回去。
//   · pauseAfter 必须保留，它决定结构化短语的拼接边界
//   · 加载校验为条目级：非法项跳过并记 WARN，不使整个词库失效
//   · 三层合并优先级：用户覆盖 > 在线更新 > 内置
package dictionary
