// Package logger 文件日志（按天 + 按大小轮转）与内存环形缓冲。
//
// V1 来源：logger.py（238 行）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - 文件命名 `translator_YYYY-MM-DD.log`，位于 <配置目录>/logs/
//   - 普通行格式：`YYYY-MM-DD HH:MM:SS [tag] [LEVEL] message`
//   - 翻译行格式：`YYYY-MM-DD HH:MM:SS - 厂商-模型名 - 原文 - 译文`（V1 translation_log）
//   - 单文件超过 2MB → 重命名为 `translator_YYYY-MM-DD_N.log`（N 从 1 起找第一个空位）
//   - **保留策略是按时间不是按个数**：启动时、以及之后每 6 小时，
//     删除 mtime 超过 7 天的 translator_*.log
//     （V1 的 max_files 字段从未被读取——是残留配置，V2 不实现"保留 N 个文件"）
//   - 内存缓冲最多 500 行，供 UI 展示
//   - 删除日志前必须先关闭文件句柄（Windows 上文件被占用无法删除）
//   - 日期跨天时自动换文件
//   - 线程安全；日志目录不可用时降级为「仅内存」，不影响主流程
package logger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// retentionDays 对应 V1 _cleanup_old_logs 的 7 天（按 mtime，不是按文件个数）
	retentionDays = 7

	// MaxFileSize 对应 V1 MAX_FILE_SIZE = 2MB
	MaxFileSize = 2 * 1024 * 1024

	// BufferSize 对应 V1 BUFFER_SIZE = 500
	BufferSize = 500

	filePrefix = "translator_"
	fileSuffix = ".log"
)

// Logger 是线程安全的文件日志器。
type Logger struct {
	mu      sync.Mutex
	dir     string
	file    *os.File
	curDate string
	buffer  []string

	bufSize  int
	maxSize  int64
	dirOK    bool
}

// New 创建日志器并做一次历史清理（V1 在构造函数里调用 _cleanup_old_logs）。
func New(dir string) *Logger {
	l := &Logger{
		dir:     dir,
		bufSize: BufferSize,
		maxSize: MaxFileSize,
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		l.dirOK = true
	}
	l.cleanupOld()
	return l
}

// CleanupInterval 是定期清理的间隔（ADR-007：每 6 小时一次，与 UI 生命周期无关）。
const CleanupInterval = 6 * time.Hour

// StartPeriodicCleanup 每 CleanupInterval 清一次历史日志，直到 ctx 结束。
//
// ⚠️ 为什么不能只在 New 里清一次：
// 这个程序是常驻的，用户可能几天不重启。只在构造时清理的话，
// `retentionDays` 的实际语义就从「日志最多留 7 天」变成了
// 「启动那一刻超过 7 天的那些」——第 8 天产生的日志会一直躺在磁盘上，
// 直到下一次重启。logger/doc.go 早就写着「每 6 小时一次」，
// 但那个定时任务一直没有实现，属于文档说了、代码没做的事。
//
// 回调由调用方传 ctx 控制生命周期，Logger 自己不持有 goroutine 的取消权：
// 日志器可能在测试里被反复创建，自己起协程又没人能停掉它。
func (l *Logger) StartPeriodicCleanup(ctx context.Context) {
	go func() {
		t := time.NewTicker(CleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.cleanupOld()
			}
		}
	}()
}

// Dir 返回日志目录（供 UI 的「打开日志目录」使用）。
func (l *Logger) Dir() string { return l.dir }

// Close 关闭文件句柄。
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeFile()
}

// Info / Warn / Error 写入一条普通日志。
func (l *Logger) Info(tag, msg string)  { l.write(tag, "INFO", msg) }
func (l *Logger) Warn(tag, msg string)  { l.write(tag, "WARN", msg) }
func (l *Logger) Error(tag, msg string) { l.write(tag, "ERROR", msg) }

// Translation 写入一条翻译日志（V1 translation_log 的逐字格式）。
func (l *Logger) Translation(providerLabel, model, original, translated string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%s - %s-%s - %s - %s", ts, providerLabel, model, original, translated)
	l.appendLine(line)
}

func (l *Logger) write(tag, level, msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%s [%s] [%s] %s", ts, tag, level, msg)
	l.appendLine(line)
}

// appendLine 是写入的核心：内存缓冲 + 轮转 + 落盘。
func (l *Logger) appendLine(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.buffer = append(l.buffer, line)
	if len(l.buffer) > l.bufSize {
		copy(l.buffer, l.buffer[len(l.buffer)-l.bufSize:])
		l.buffer = l.buffer[:l.bufSize]
	}

	if !l.dirOK {
		return // 目录不可用：仅保留内存日志，不影响主流程
	}

	l.rotateIfNeeded()
	l.ensureFileOpen()
	if l.file != nil {
		// 写失败静默忽略（与 V1 一致：日志不该拖垮主流程）。
		// 注意：Go 的 os.File 是无缓冲的，WriteString 返回即已交给内核，
		// 因此不需要 V1 里那句 file.flush()。
		_, _ = l.file.WriteString(line + "\n")
	}
}

// currentPath 返回今天的日志文件路径（V1 _current_log_path）。
func (l *Logger) currentPath() string {
	return filepath.Join(l.dir,
		filePrefix+time.Now().Format("2006-01-02")+fileSuffix)
}

// closeFile 关闭并清空句柄（Windows 上删除/重命名前必须先关）。
func (l *Logger) closeFile() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// ensureFileOpen 按需打开今天的日志文件；跨天时换文件（V1 _ensure_file_open）。
func (l *Logger) ensureFileOpen() {
	today := time.Now().Format("2006-01-02")
	if l.file != nil && today == l.curDate {
		return
	}
	if l.file != nil {
		l.closeFile()
	}
	l.curDate = today

	f, err := os.OpenFile(l.currentPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.file = nil // 降级为仅内存
		return
	}
	l.file = f
}

// rotateIfNeeded 超过上限时把当前文件改名（V1 _rotate_if_needed）。
func (l *Logger) rotateIfNeeded() {
	path := l.currentPath()
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= l.maxSize {
		return
	}
	// Windows 上文件被占用时改名会失败，必须先关闭句柄
	l.closeFile()

	base := strings.TrimSuffix(path, fileSuffix)
	seq := 1
	for {
		candidate := fmt.Sprintf("%s_%d%s", base, seq, fileSuffix)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			_ = os.Rename(path, candidate)
			return
		}
		seq++
	}
}

// cleanupOld 删除 mtime 超过 retentionDays 的日志（V1 _cleanup_old_logs）。
//
// 注意保留策略是**按时间**，不是"保留 N 个文件"。
func (l *Logger) cleanupOld() {
	if !l.dirOK {
		return
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-retentionDays * 24 * time.Hour)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// Recent 返回最近 max 行；max<=0 表示返回全部。
//
// 与 V1 的差异：V1 的 get_recent(0) 返回空列表（没人用到的边界），
// Go 里用 max<=0 表示「全部」更自然。
func (l *Logger) Recent(max int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.buffer))
	copy(out, l.buffer)
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// Files 返回日志目录下的日志文件名（按修改时间倒序）。
func (l *Logger) Files() []string {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	type item struct {
		name string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{e.Name(), fi.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	return names
}

// DeleteAll 删除全部日志文件并清空内存缓冲（V1 delete_all_logs / delete_translator_logs）。
//
// 返回删除的文件数与遇到的错误（与 V1 一致：单个文件删除失败不中断整体）。
func (l *Logger) DeleteAll() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.closeFile() // 必须先关句柄，否则 Windows 删不掉
	l.buffer = nil

	var errs []string
	deleted := 0
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return 0, []string{err.Error()}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, name)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		deleted++
	}
	// 文件句柄按需惰性重开（V1 注释：lazily reopened on next _log() call）
	return deleted, errs
}
