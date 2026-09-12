// Package dictionary 五层术语处理与 resources/dictionary.json 加载校验。
//
// V1 来源：chat_dictionary.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 五层顺序不可调换：slang → phrases → structuredActions → promptMapping → ets2Terms
//   · pauseAfter 必须保留，它决定结构化短语的拼接边界
//   · 加载校验为条目级：非法项跳过并记 WARN，不使整个词库失效
//   · 三层合并优先级：用户覆盖 > 在线更新 > 内置
package dictionary
