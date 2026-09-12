// 命名管道的客户端实现（Windows）。
//
// ══ 为什么必须用 overlapped I/O ═════════════════════════════
// Windows 对以**同步**方式打开的句柄会**序列化其上的 I/O 操作**。而本包的
// 结构是「常驻读循环 + 调用方写请求」：
//
//	goroutine A: ReadFile(handle, …)   // 阻塞等待对端发数据
//	goroutine B: WriteFile(handle, …)  // 想发请求 —— 但被排到 A 后面
//
// 于是双方互等：A 等数据，数据要等 B 的写完成，而 B 要等 A 让出句柄。
// 整套看起来像「连上了但一句话都说不通」，而且因为阻塞发生在系统调用内部，
// Go 的 context 超时完全无效（它只能中断 channel 等待，中断不了 syscall）。
//
// 定位过程（记下来免得重走）：
//   · 用 .NET 的同步 NamedPipeClientStream 手工握手 → 成功
//   · 用 syscall.CreateFile 同步打开、单线程读写 → 成功
//   · 在同一句柄上先起一个阻塞读，再写 → **卡死**
// 最后一步就锁定了「同步句柄序列化 I/O」这个根因。
//
// 改成 overlapped 后读写真正并发，而且等待可以带超时——
// 顺带解决了「不可中断」的问题。
package ipc

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ioWaitMs 是单次重叠操作的等待上限。
// 给得足够宽（一帧的读写是毫秒级的事），只在真出问题时把调用捞回来。
const ioWaitMs = 30000

// CreateEvent 与 GetOverlappedResult 不在标准库的 syscall 包里
// （它们在 golang.org/x/sys/windows）。本项目坚持**零第三方依赖**，
// 所以直接按名取 kernel32 的导出——internal/config 的 DPAPI 也是这个模式。
var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
)

// createAutoResetEvent 创建一个自动重置、初始未置位的匿名事件。
// 自动重置的含义：系统在 I/O 完成时置位，WaitForSingleObject 取走后自动复位。
func createAutoResetEvent() (syscall.Handle, error) {
	r, _, err := procCreateEventW.Call(0, 0, 0, 0)
	if r == 0 {
		return 0, err
	}
	return syscall.Handle(r), nil
}

// overlappedResult 取一次重叠操作的最终结果（含实际传输字节数）。
func overlappedResult(h syscall.Handle, ov *syscall.Overlapped, n *uint32) error {
	r, _, err := procGetOverlappedResult.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(ov)),
		uintptr(unsafe.Pointer(n)),
		0, // 不等待：事件已经被等待过了
	)
	if r == 0 {
		return err
	}
	return nil
}

// pipeConn 是到 native 的命名管道连接。
type pipeConn struct {
	handle     syscall.Handle
	readEvent  syscall.Handle
	writeEvent syscall.Handle

	// 同一个方向只应有一个 OVERLAPPED 在飞，锁用来保证这点。
	// ⚠️ readMu 会被**长期持有**——读循环阻塞在 Read 里时就持着它。
	// 所以 Close 绝不能去抢它，否则就是「Close 等读退出、读等数据」的死锁。
	// 关闭标记因此用独立的原子变量。
	readMu  sync.Mutex
	writeMu sync.Mutex

	closed atomic.Bool
}

// openPipe 以 overlapped 方式打开命名管道，与服务端的实例模式匹配。
func openPipe(name string) (*pipeConn, error) {
	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	h, err := syscall.CreateFile(
		path,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // 不共享：同一时刻只有一个客户端
		nil,
		syscall.OPEN_EXISTING,
		// 服务端用 PIPE_ACCESS_DUPLEX|FILE_FLAG_OVERLAPPED 创建，
		// 客户端必须同样是 overlapped，否则一边重叠一边同步会出怪问题。
		syscall.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return nil, err
	}

	// 自动重置事件：系统在 I/O 完成时置位，WaitForSingleObject 取走后自动复位。
	re, err := createAutoResetEvent()
	if err != nil {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("创建读事件失败: %w", err)
	}
	we, err := createAutoResetEvent()
	if err != nil {
		syscall.CloseHandle(re)
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("创建写事件失败: %w", err)
	}

	return &pipeConn{handle: h, readEvent: re, writeEvent: we}, nil
}

// waitEvent 等待事件置位，带超时。
func waitEvent(ev syscall.Handle, what string) error {
	r, err := syscall.WaitForSingleObject(ev, ioWaitMs)
	if err != nil {
		return fmt.Errorf("等待%s完成失败: %w", what, err)
	}
	if r != syscall.WAIT_OBJECT_0 {
		// WAIT_TIMEOUT 或 WAIT_FAILED：调用方应把这次连接判为不可用
		return fmt.Errorf("等待%s完成超时（%dms）", what, ioWaitMs)
	}
	return nil
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()

	if c.closed.Load() {
		return 0, io.EOF
	}

	ov := &syscall.Overlapped{HEvent: c.readEvent}
	var n uint32

	err := syscall.ReadFile(c.handle, p, &n, ov)
	if err == syscall.ERROR_IO_PENDING {
		if werr := waitEvent(c.readEvent, "读"); werr != nil {
			return 0, werr
		}
		err = overlappedResult(c.handle, ov, &n)
	}
	if err != nil {
		return 0, err
	}
	if n == 0 {
		// 管道读到 0 字节即对端已关闭
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	total := 0
	for total < len(p) {
		ov := &syscall.Overlapped{HEvent: c.writeEvent}
		var n uint32

		err := syscall.WriteFile(c.handle, p[total:], &n, ov)
		if err == syscall.ERROR_IO_PENDING {
			if werr := waitEvent(c.writeEvent, "写"); werr != nil {
				return total, werr
			}
			err = overlappedResult(c.handle, ov, &n)
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		total += int(n)
	}
	return total, nil
}

// Close 关闭连接。
//
// ⚠️ 这里**不能**去抢 readMu：读循环阻塞在 Read 里时正持着它，
// 而读在等对端发数据——抢锁就变成「Close 等读退出、读等数据」的死锁。
// 正确顺序：置关闭标记 → CancelIo 唤醒挂起的重叠操作 → 关句柄。
// 被唤醒的读会从 overlappedResult 拿到错误并自行退出。
func (c *pipeConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil // 只关一次
	}

	h := c.handle
	c.handle = 0

	if h != 0 {
		// 唤醒可能正挂着的读/写；没有挂起操作时它返回 FALSE，无害
		_ = syscall.CancelIo(h)
	}

	// 给被唤醒的一方一点时间走完 overlappedResult，
	// 免得句柄先被回收导致它拿到一个莫名其妙的错误码。
	time.Sleep(time.Millisecond)

	var err error
	if h != 0 {
		err = syscall.CloseHandle(h)
	}
	for _, ev := range []syscall.Handle{c.readEvent, c.writeEvent} {
		if ev != 0 {
			syscall.CloseHandle(ev)
		}
	}
	c.readEvent, c.writeEvent = 0, 0
	return err
}
