// Package api REST 与 WebSocket 接口（前端唯一入口，ADR-009）。
//
// V1 来源：新增（替代 V1 的 UI 直连）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 契约见 docs/architecture.md 第 9 节
//   · 错误码与文案必须与 V1 一致
//   · 前端不做本地持久化，配置/日志/统计唯一事实源在 Go
package api
