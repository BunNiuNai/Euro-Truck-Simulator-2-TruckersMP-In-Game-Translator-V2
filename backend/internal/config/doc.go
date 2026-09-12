// Package config 配置模型、DPAPI 密钥加密、原子写、热重载。
//
// V1 来源：config.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · V2 使用全新配置目录，不读 V1 配置（ADR-008）
//   · 密钥用 DPAPI 加密，配置文件中不得出现明文
//   · DPAPI 解密失败时保留原密文值，绝不写空串（防止密钥永久丢失）
//   · 一律原子写；损坏时备份为 .corrupted.{timestamp} 后重建
//   · 主目录不可写时回退 %LOCALAPPDATA%\ETS2 Translator\
//   · 热重载：3 秒 mtime 轮询
package config
