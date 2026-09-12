// Command pipedbg 命名管道联调诊断工具。
//
// 为什么单独写一个而不是用 nativeprobe：nativeprobe 走正常路径，
// 一旦卡在**不可中断的系统调用**里（Windows 的同步 ReadFile/WriteFile 就是），
// context 超时和任何 Go 层的超时都救不回来——只能等外部把进程杀掉。
// 这个工具把 I/O 放在单独 goroutine，主线程到点就打印「卡在哪一步」并退出，
// 所以它**永远不会挂住**。
//
// 用法：
//
//	pipedbg <管道名> [超时秒数]
//	pipedbg -spawn <native.exe> [超时秒数]   # 由本程序启动 native（复现测试环境）
//
// 退出码：0 全通；1 有失败；2 超时。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ipc"
)

// tracker 记录当前步骤，供超时后报告进度。
type tracker struct {
	mu   sync.Mutex
	step string
}

func (t *tracker) set(s string) {
	t.mu.Lock()
	t.step = s
	t.mu.Unlock()
}

func (t *tracker) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.step
}

var failed bool

func ok(format string, args ...any) {
	fmt.Printf("  ✓ %s\n", fmt.Sprintf(format, args...))
}

func bad(format string, args ...any) {
	failed = true
	fmt.Printf("  ✗ %s\n", fmt.Sprintf(format, args...))
}

// dialSync 用**同步**方式打开管道（attrs=0，不带 FILE_FLAG_OVERLAPPED）。
func dialSync(name string) (syscall.Handle, error) {
	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		path,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
}

// writeAll 循环写满，避免短写。
func writeAll(h syscall.Handle, b []byte) error {
	total := 0
	for total < len(b) {
		var n uint32
		if err := syscall.WriteFile(h, b[total:], &n, nil); err != nil {
			fmt.Printf("    WriteFile 失败于第 %d 字节: %v\n", total, err)
			return err
		}
		if n == 0 {
			return fmt.Errorf("写入返回 0 字节")
		}
		total += int(n)
	}
	return nil
}

// readAll 循环读满。
func readAll(h syscall.Handle, b []byte) error {
	total := 0
	for total < len(b) {
		var n uint32
		if err := syscall.ReadFile(h, b[total:], &n, nil); err != nil {
			fmt.Printf("    ReadFile 失败于第 %d 字节: %v\n", total, err)
			return err
		}
		if n == 0 {
			return fmt.Errorf("读到 0 字节（对端关闭）")
		}
		total += int(n)
	}
	return nil
}

func run(pipe string, tr *tracker) {
	tr.set("连接管道")
	fmt.Printf("  连接 %s …\n", pipe)
	h, err := dialSync(pipe)
	if err != nil {
		bad("连接失败: %v", err)
		return
	}
	defer syscall.CloseHandle(h)
	ok("连接成功 handle=%d", h)
	fmt.Printf("     （若上一行是 ✗ 且 native 日志说「Go 已连接」，" +
		"说明连接建立了但后续 I/O 不合，问题在 I/O 而非连接）\n")

	// 构造 native.hello 帧
	body := `{"type":"native.hello","id":1,"payload":{"protocolVersion":1}}`
	frame := make([]byte, 4+len(body))
	n := uint32(len(body))
	frame[0] = byte(n)
	frame[1] = byte(n >> 8)
	frame[2] = byte(n >> 16)
	frame[3] = byte(n >> 24)
	copy(frame[4:], body)

	tr.set("写帧头+帧体")
	fmt.Printf("  写 %d 字节帧 …\n", len(frame))
	if err := writeAll(h, frame); err != nil {
		bad("写帧失败: %v", err)
		return
	}
	ok("写帧成功")

	tr.set("读帧头")
	fmt.Println("  读响应帧头 …")
	hdr := make([]byte, 4)
	if err := readAll(h, hdr); err != nil {
		bad("读帧头失败: %v", err)
		return
	}
	respLen := uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16 | uint32(hdr[3])<<24
	ok("帧头读到，响应长度 %d", respLen)

	if respLen == 0 || respLen > 8<<20 {
		bad("响应长度不合理: %d", respLen)
		return
	}

	tr.set("读帧体")
	resp := make([]byte, respLen)
	if err := readAll(h, resp); err != nil {
		bad("读帧体失败: %v", err)
		return
	}
	ok("响应: %s", string(resp))
}

// spawnNative 用与 ipc 测试**完全相同**的方式启动 native 子进程，
// 好复现「由 Go 启动」与「由外部启动」的差异。
//
// 关键点与测试一致：stdout/stderr 重定向到**文件**而不是 bytes.Buffer
// ——后者会让 Go 创建匿名管道。
func spawnNative(exe, pipeName string) (*exec.Cmd, string, error) {
	logPath := os.Getenv("PIPEDBG_LOG")
	if logPath == "" {
		logPath = "pipedbg-native.log"
	}
	f, err := os.Create(logPath)
	if err != nil {
		return nil, "", err
	}

	cmd := exec.Command(exe, "--pipe", pipeName)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, logPath, err
	}
	return cmd, logPath, nil
}

// runViaIPC 用**真实的生产代码**（internal/ipc）走一遍握手。
//
// 与上面的 run 对比：两者都用 syscall.CreateFile + WriteFile，
// 差别只在「有没有并发的读循环」和「帧是分两次写还是一起写」。
// 如果这个模式挂而 run 不挂，问题就锁定在 ipc 包内部。
func runViaIPC(pipe string, tr *tracker) {
	tr.set("ipc.Dial")
	fmt.Printf("  ipc.Dial(%s) …\n", pipe)
	client, err := ipc.Dial(pipe, 4*time.Second)
	if err != nil {
		bad("Dial 失败: %v", err)
		return
	}
	defer client.Close()
	ok("Dial 成功")

	tr.set("ipc.Handshake")
	fmt.Println("  Handshake（context 超时 4s）…")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	caps, match, err := client.Handshake(ctx)
	if err != nil {
		bad("Handshake 失败: %v", err)
		return
	}
	ok("Handshake 成功 capabilities=%v versionMatch=%v", caps, match)

	tr.set("ipc.Ping")
	fmt.Println("  Ping …")
	if err := client.Ping(ctx, 1); err != nil {
		bad("Ping 失败: %v", err)
		return
	}
	ok("Ping 成功")
}

// runConcurrent 验证一个假设：**同步句柄上的并发读会挡住写**。
//
// Windows 对以同步方式打开的句柄会序列化其上的 I/O 操作。如果真是这样，
// 「后台线程阻塞在 ReadFile」+「主线程要 WriteFile」就会死锁：
// 写等读让位，读等数据到来，而数据要等写完成。
//
// 这正是 internal/ipc 的结构（readLoop 常驻读 + Call 写请求）。
func runConcurrent(pipe string, tr *tracker) {
	tr.set("连接管道")
	fmt.Printf("  连接 %s …\n", pipe)
	h, err := dialSync(pipe)
	if err != nil {
		bad("连接失败: %v", err)
		return
	}
	defer syscall.CloseHandle(h)
	ok("连接成功")

	// 起一个后台读（模仿 readLoop），故意让它先阻塞住
	readerStarted := make(chan struct{})
	go func() {
		buf := make([]byte, 4)
		var n uint32
		close(readerStarted)
		_ = syscall.ReadFile(h, buf, &n, nil) // 阻塞，直到有数据
	}()
	<-readerStarted
	time.Sleep(200 * time.Millisecond) // 让读先真正进入阻塞
	fmt.Println("  后台读已进入阻塞，现在尝试写 …")

	body := `{"type":"native.hello","id":1,"payload":{"protocolVersion":1}}`
	frame := make([]byte, 4+len(body))
	n := uint32(len(body))
	frame[0], frame[1], frame[2], frame[3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	copy(frame[4:], body)

	tr.set("并发读存在时写帧")
	if err := writeAll(h, frame); err != nil {
		bad("写失败: %v", err)
		return
	}
	ok("写成功 —— 同步句柄**没有**被序列化，假设不成立")
}

func main() {
	spawn := flag.String("spawn", "", "native 可执行文件路径；给出时由本程序启动它")
	viaIPC := flag.Bool("ipc", false, "改走 internal/ipc 的生产代码路径")
	concurrent := flag.Bool("concurrent", false, "验证「同步句柄并发读挡住写」的假设")
	flag.Parse()
	rest := flag.Args()

	timeout := 8
	var pipe string
	var nativeExe string

	if *spawn != "" {
		nativeExe = *spawn
		pipe = fmt.Sprintf(`\\.\pipe\pipedbg-%d`, os.Getpid())
		if len(rest) >= 1 {
			if v, err := strconv.Atoi(rest[0]); err == nil && v > 0 {
				timeout = v
			}
		}
	} else {
		if len(rest) < 1 {
			fmt.Println("用法: pipedbg <管道名> [超时秒数]")
			fmt.Println("      pipedbg -spawn <native.exe> [超时秒数]")
			fmt.Println("      pipedbg -spawn <native.exe> -ipc [超时秒数]")
			os.Exit(1)
		}
		pipe = rest[0]
		if len(rest) >= 2 {
			if v, err := strconv.Atoi(rest[1]); err == nil && v > 0 {
				timeout = v
			}
		}
	}

	fmt.Println("=== 管道诊断 ===")

	if nativeExe != "" {
		cmd, logPath, err := spawnNative(nativeExe, pipe)
		if err != nil {
			fmt.Printf("  ✗ 启动 native 失败: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
			if b, err := os.ReadFile(logPath); err == nil {
				fmt.Printf("=== native 日志（%s）===\n%s\n", logPath, string(b))
			}
		}()
		fmt.Printf("  已启动 native PID=%d，管道 %s\n", cmd.Process.Pid, pipe)
		time.Sleep(900 * time.Millisecond) // 等它把管道建起来
	}

	tr := &tracker{step: "启动"}
	done := make(chan struct{})

	go func() {
		defer close(done)
		switch {
		case *concurrent:
			runConcurrent(pipe, tr)
		case *viaIPC:
			runViaIPC(pipe, tr)
		default:
			run(pipe, tr)
		}
	}()

	select {
	case <-done:
		fmt.Println("=== 结束 ===")
		if failed {
			os.Exit(1)
		}
	case <-time.After(time.Duration(timeout) * time.Second):
		fmt.Printf("=== 超时（%d 秒）===\n", timeout)
		fmt.Printf("卡在: %s\n", tr.get())
		// 这里**不能**等 goroutine：它阻塞在不可中断的系统调用里。
		// 直接退出，让操作系统回收。
		os.Exit(2)
	}
}
