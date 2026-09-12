// ChatLog 数据源 —— V1 monitor.py ChatMonitor（140~267 行）的等价实现。
//
// 必须保留的 V1 行为（每一条都有真实意义，删掉就会漏消息或漏翻译）：
//   - 启动时跳到文件末尾（跳过历史消息，只读增量）
//   - 每 0.5 秒轮询一次；每约 3 秒检查一次是否出现了更新的日志文件（TMP 重连会换文件）
//   - 切换文件时：新文件从头读、清空去重表（避免漏掉切换瞬间的消息）
//   - 文件被截断时（size 变小）：从头重读
//   - 二进制读取 + 字节偏移（Windows 上文本模式 seek 会因 CRLF 与多字节字符错位）
//   - 跨读取边界的半行缓冲（一行日志可能被读成两半）
//   - 去重键 = 玩家 + 内容 + 时间戳，FIFO 淘汰，上限 500
//   - 队列满时丢弃消息而不是阻塞
//   - 系统消息里识别服务器名并回调
package chatlog

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ingest"
)

const (
	pollInterval     = 500 * time.Millisecond
	switchCheckEvery = 6 // 6 * 0.5s = 3s
	seenLimit        = 500
)

// Source 是 chatlog 数据源实现。
type Source struct {
	dir string

	// selfName 返回当前配置的玩家昵称（可为空）。
	selfName func() string

	// onServerName 在系统消息里检测到服务器名时回调（V1 的 server_name_ref）。
	onServerName func(name string)

	mu          sync.Mutex
	logPath     string
	lastSize    int64
	status      string
	seen        map[string]struct{}
	seenOrder   []string
	// done 在扫描协程退出时关闭，供 Wait 使用（退出收尾要用它 join）。
	done chan struct{}
	partialLine []byte
	cancel      context.CancelFunc
}

// New 创建数据源。selfName/onServerName 可为 nil。
func New(dir string, selfName func() string, onServerName func(string)) *Source {
	return &Source{
		dir:         dir,
		selfName:    selfName,
		onServerName: onServerName,
		seen:        map[string]struct{}{},
		done:        make(chan struct{}),
		status:      "未启动",
	}
}

func (s *Source) ID() string { return "chatlog" }

// Start 启动后台轮询（非阻塞），事件写入 out。
func (s *Source) Start(ctx context.Context, out chan<- domain.Event) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.run(ctx, out)
	return nil
}

func (s *Source) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.setStatus("已停止")
}

// Wait 等扫描协程真正退出，最多等 d。
//
// V1 的退出路径 join 5 秒、超时告警（`main.py:_shutdown`）。不等待的话，
// 退出时已经读进内存但还没走完翻译的最后几条消息会被直接丢掉——
// 日志里明明有、界面上却没有，用户会以为程序漏读了。
func (s *Source) Wait(d time.Duration) error {
	if s.done == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(d):
		return fmt.Errorf("扫描协程在 %v 内未退出", d)
	}
}

// Status 返回状态快照。
func (s *Source) Status() ingest.SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ingest.SourceStatus{State: s.status}
}

func (s *Source) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

func (s *Source) run(ctx context.Context, out chan<- domain.Event) {
	defer close(s.done) // 供 Wait 判断协程是否真的退出了
	s.setStatus("运行中")
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	switchCheck := 0

	for {
		select {
		case <-ctx.Done():
			s.setStatus("已停止")
			return
		case <-ticker.C:
			s.step(out)
			switchCheck++
			if switchCheck >= switchCheckEvery {
				switchCheck = 0
				s.checkLogSwitch()
			}
		}
	}
}

// step 是每轮轮询的主逻辑：找日志 / 切日志 / tail 一次。
func (s *Source) step(out chan<- domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.logPath == "" || !fileExists(s.logPath) {
		s.logPath = FindLatestLog(s.dir)
		if s.logPath != "" {
			if fi, err := os.Stat(s.logPath); err == nil {
				s.lastSize = fi.Size() // 启动时跳到末尾，跳过历史消息
				s.status = "已找到日志: " + filepath.Base(s.logPath)
			}
		} else {
			s.status = LogDirStatus(s.dir)
		}
	}

	if s.logPath != "" && fileExists(s.logPath) {
		s.tailOnce(out)
	}
}

// checkLogSwitch 对应 V1 _check_log_switch：
// 出现更新的日志文件时切换过去，从头读新文件并清空去重表。
func (s *Source) checkLogSwitch() {
	latest := FindLatestLog(s.dir)
	if latest == "" || latest == s.logPath {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	old := filepath.Base(s.logPath)
	s.logPath = latest
	s.lastSize = 0 // 读整个新文件，避免漏掉切换瞬间的消息
	s.seen = map[string]struct{}{}
	s.seenOrder = nil
	s.status = fmt.Sprintf("已切换日志: %s (旧: %s)", filepath.Base(latest), old)
}

// tailOnce 对应 V1 _tail_once。
func (s *Source) tailOnce(out chan<- domain.Event) {
	current, err := fileSize(s.logPath)
	if err != nil {
		return
	}
	if current < s.lastSize {
		s.lastSize = 0 // 文件被截断 → 从头重读
	}
	if current <= s.lastSize {
		return
	}

	toRead := current - s.lastSize
	f, err := os.Open(s.logPath) // 二进制模式
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(s.lastSize, io.SeekStart); err != nil {
		return
	}
	raw := make([]byte, toRead)
	n, _ := io.ReadFull(f, raw)
	raw = raw[:n]
	s.lastSize = current // 与 V1 一致：先推进偏移，再处理（半行缓冲兜底）

	// 解码：Go 的 string(bytes) 对非法 UTF-8 自动替换为 U+FFFD，
	// 等价于 Python 的 decode("utf-8", errors="replace")
	newData := string(append(s.partialLine, raw...))
	s.partialLine = nil

	// V1 用 str.splitlines()；日志实际只有 \n 与 \r\n 两种，统一后按 \n 切
	newData = strings.ReplaceAll(newData, "\r\n", "\n")
	lines := strings.Split(newData, "\n")

	// 末尾不完整的行 → 缓冲到下一次读取
	if len(raw) > 0 && raw[len(raw)-1] != '\n' && len(lines) > 0 {
		s.partialLine = []byte(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		selfName := ""
		if s.selfName != nil {
			selfName = s.selfName()
		}
		ev := ParseLine(line, selfName)
		if ev == nil {
			continue
		}

		// 系统消息里识别服务器名
		if ev.IsSystem && s.onServerName != nil {
			if server := DetectServerName(ev.Text); server != "" {
				s.onServerName(server)
			}
		}

		// 去重：玩家 + 内容 + 时间戳
		key := ev.Speaker + "|" + ev.Text + "|" + ev.Timestamp
		if _, dup := s.seen[key]; dup {
			continue
		}
		s.seen[key] = struct{}{}
		s.seenOrder = append(s.seenOrder, key)
		for len(s.seen) > seenLimit {
			oldest := s.seenOrder[0]
			s.seenOrder = s.seenOrder[1:]
			delete(s.seen, oldest)
		}

		// 队列满 → 丢弃（V1 queue.put(timeout=0.1) 的语义）
		select {
		case out <- *ev:
		default:
		}
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func fileSize(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
