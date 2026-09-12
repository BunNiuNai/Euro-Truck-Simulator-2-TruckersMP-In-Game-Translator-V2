// Package ipc Named Pipe 客户端、帧编解码、心跳、请求超时（ADR-004）。
//
// V1 来源：新增
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 帧格式：4 字节小端长度 + UTF-8 JSON payload，上限 8MB
//   · 三种模式：请求/响应、服务端推送事件、心跳 ping/pong
//   · 心跳 1 秒一次，连续 3 次无 pong 判定 native 失联
//   · 单次调用超时不得阻塞 Engine
package ipc
