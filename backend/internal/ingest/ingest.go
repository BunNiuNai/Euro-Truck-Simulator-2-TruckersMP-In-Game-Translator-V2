// Package ingest 数据源层（Source 接口）。
//
// V1 来源：monitor.py 的「数据源」概念（V2 把它抽象成接口）。
//
// 不变量：
//   - Engine 不感知具体来源，只消费 domain.Event（G1）
//   - 队列满时丢弃而不是阻塞（V1 queue.put(timeout=0.1) 的语义）
//   - 当前只实现 chatlog（Q3 已决「都不做」）
package ingest

import (
	"context"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// SourceStatus 是数据源的运行状态，供前端 source.status 事件展示。
// State 的取值沿用 V1 monitor 的 status 字符串。
type SourceStatus struct {
	State  string // "未启动" | "运行中" | "已找到日志: x" | "已切换日志: x (旧: y)" | "已停止"
	Detail string // 补充诊断（如 log_dir_status 的结果）
}

// Source 产出统一领域事件。新增数据源 = 实现这个接口，Engine 不用改。
type Source interface {
	ID() string
	Start(ctx context.Context, out chan<- domain.Event) error
	Stop()
	Status() SourceStatus
}
