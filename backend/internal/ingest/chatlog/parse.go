// 聊天日志解析 —— V1 monitor.py 61~137 行的逐行移植。
//
// 日志格式（TruckersMP 官方格式）：
//   [Channel] [HH:MM:SS] PlayerName (ServerLetter TMP_ID): Message
//   [System]  [HH:MM:SS] Server restarting...
package chatlog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// 文件名模式 —— 与 V1 完全一致（三种 glob 依次回退）。
const (
	LogGlob    = "chat_*_log.txt"
	LogGlobHex = "chat_*_log_*.txt"
	LogGlobAny = "chat_*.txt"
)

var (
	// [Channel] [HH:MM:SS] PlayerName (ServerLetter ID): Message
	chatLineRe = regexp.MustCompile(
		`^\[(?P<channel>.+?)\]\s+\[(?P<time>\d{2}:\d{2}:\d{2})\]\s+` +
			`(?P<player>.+?)\s+\([A-Z]?\s*\d+\):\s+(?P<text>.+)$`)

	// [Channel] [HH:MM:SS] Message（无玩家名 / TMP ID）
	systemLineRe = regexp.MustCompile(
		`^\[(?P<channel>.+?)\]\s+\[(?P<time>\d{2}:\d{2}:\d{2})\]\s+(?P<text>.+)$`)

	// "Connecting to Simulation 1 server..."
	serverConnectRe = regexp.MustCompile(`Connecting to (.+?) server\.\.\.`)
)

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// FindLatestLog 对应 V1 find_latest_log：
// 主模式 chat_*_log.txt → 次模式 chat_*_log_*.txt → 兜底 chat_*.txt，
// 按修改时间取最新。目录不存在或无文件时返回空串。
func FindLatestLog(dir string) string {
	if !dirExists(dir) {
		return ""
	}
	for _, pat := range []string{LogGlob, LogGlobHex, LogGlobAny} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		if len(files) > 0 {
			sort.Slice(files, func(i, j int) bool {
				ti, ei := os.Stat(files[i])
				tj, ej := os.Stat(files[j])
				if ei != nil || ej != nil {
					return files[i] > files[j]
				}
				return ti.ModTime().After(tj.ModTime())
			})
			return files[0]
		}
	}
	return ""
}

// ParseLine 对应 V1 parse_line：
// 玩家消息 → Event（IsSystem=false）；系统消息（无玩家名/ID）→ Event（IsSystem=true，
// Speaker 为 "[Channel]"）；非聊天行 → nil。
// selfName 非空且与玩家名相同 → IsSelf=true。
func ParseLine(line string, selfName string) *domain.Event {
	stripped := strings.TrimSpace(line)

	if m := chatLineRe.FindStringSubmatch(stripped); m != nil {
		player := m[chatLineRe.SubexpIndex("player")]
		text := m[chatLineRe.SubexpIndex("text")]
		ts := m[chatLineRe.SubexpIndex("time")]
		return &domain.Event{
			ID:        player + "|" + text + "|" + ts,
			Timestamp: ts,
			Speaker:   player,
			Text:   text,
			Origin: "chatlog",
			// ⚠️ 大小写不敏感。
			//
			// V1 `compose_sender.py:172` 判自己发的那条用的是
			// `msg_player.lower() == player_name.lower()`。V2 原来写的是精确
			// 相等，于是玩家名大小写与日志里不一致时（TruckersMP 上很常见：
			// 配置里填 "alice"，日志里是 "Alice"）——
			//   · 自己的消息不会被跳过翻译，白花 API
			//   · 发送确认永远匹配不上，只得到「已发送（未确认）」
			IsSelf: selfName != "" && strings.EqualFold(player, selfName),
		}
	}

	if sm := systemLineRe.FindStringSubmatch(stripped); sm != nil {
		channel := sm[systemLineRe.SubexpIndex("channel")]
		text := sm[systemLineRe.SubexpIndex("text")]
		ts := sm[systemLineRe.SubexpIndex("time")]
		return &domain.Event{
			ID:        "[" + channel + "]|" + text + "|" + ts,
			Timestamp: ts,
			Speaker:   "[" + channel + "]",
			Text:      text,
			Origin:    "chatlog",
			IsSystem:  true,
		}
	}
	return nil
}

// DetectServerName 对应 V1 detect_server_name：
// 从 "Connecting to X server..." 提取服务器名，失败返回空串。
func DetectServerName(text string) string {
	m := serverConnectRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// LogDirStatus 对应 V1 log_dir_status：三种诊断文案。
func LogDirStatus(dir string) string {
	if !dirExists(dir) {
		return "目录不存在: " + dir
	}
	files, _ := filepath.Glob(filepath.Join(dir, "chat_*"))
	if len(files) == 0 {
		return "目录存在但无聊天日志文件: " + dir
	}
	sort.Slice(files, func(i, j int) bool {
		ti, _ := os.Stat(files[i])
		tj, _ := os.Stat(files[j])
		if ti == nil || tj == nil {
			return files[i] > files[j]
		}
		return ti.ModTime().After(tj.ModTime())
	})
	fi, err := os.Stat(files[0])
	if err != nil {
		return "目录存在但无聊天日志文件: " + dir
	}
	return fmt.Sprintf("最新日志: %s (%d bytes, 修改于 %s)",
		filepath.Base(files[0]), fi.Size(), fi.ModTime().Format("2006-01-02 15:04:05"))
}
