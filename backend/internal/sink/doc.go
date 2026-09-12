// Package sink 输出层（Sink 接口）：webview、log。
//
// V1 来源：overlay.py（显示部分）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 投递失败不得阻塞 Engine；队列满时丢弃最旧消息并计数
//   · native 不可用时 Available() 返回 false，消息进入待重放缓冲（ADR-012）
//   · 只呈现，不做业务决策
package sink
