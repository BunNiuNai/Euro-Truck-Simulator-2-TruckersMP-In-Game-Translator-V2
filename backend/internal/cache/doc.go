// Package cache LRU 翻译缓存与同文本并发合并。
//
// V1 来源：translator.py（LRUCache / _in_flight / _in_flight_results）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 容量 1000 条
//   · 相同原文并发到达时合并为一次 API 调用（singleflight）
//   · 缓存命中与本地词典命中都必须计入 Stats，且不计为 translated
package cache
