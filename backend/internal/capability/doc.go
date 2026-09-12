// Package capability 期望状态注册表与崩溃恢复重放（ADR-012）。
//
// V1 来源：新增（C++ 分层带来的必要配套，属内部机制）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · Go 持期望状态（热键/托盘/显示），native 持实际状态
//   · native 重启后由 Replay 全量重放，幂等
//   · 与实际状态差异记 WARN 日志，不在 UI 新增展示（P1）
package capability
