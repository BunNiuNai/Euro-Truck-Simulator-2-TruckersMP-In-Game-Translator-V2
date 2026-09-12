// Package domain 领域对象与错误码。
//
// V1 来源：message_types.py（DisplayMessage / TranslationStats）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · Event / RenderedMessage / Stats 是 Source→Engine→Sink 之间唯一的通信载体
//   · 错误码文案必须与 V1 translator.py:_format_error 逐字一致
//   · 不得引入 V1 没有的用户可见字段（首要原则 P1）
package domain
