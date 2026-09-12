// Package ingest 数据源层（Source 接口）与 chatlog 实现。
//
// V1 来源：monitor.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · V1 的双 glob 回退顺序不可改变：chat_*_log.txt → chat_*_log_*.txt → chat_*.txt
//   · 按 mtime 取最新；支持切档检测与增量读取
//   · 启动时跳过历史消息（只读增量）
//   · 去重键 = 玩家 + 内容 + 时间戳
//   · 必须识别系统消息、服务器名、玩家昵称
//   · V1 此层零测试覆盖，V2 必须补齐（见迁移对照表 D5）
package ingest
