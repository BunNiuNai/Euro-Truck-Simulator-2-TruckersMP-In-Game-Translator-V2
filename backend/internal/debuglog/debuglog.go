// Package debuglog 是 V1 `input_sender._debug_log` 与 `overlay._debug_log`
// 的等价物：把疑难排查用的细粒度步骤写进 `%TEMP%/ets2_translator_debug.log`。
//
// V1 来源：input_sender.py:27-35、overlay.py:63-67。
//
// 为什么不直接用 internal/logger：
//   · **目的地不同**。这里固定写 `%TEMP%`，用户按 V1 的文档就知道去哪找；
//     而 logger 写 `<配置目录>/logs/*.log`，排查「发不出去」这类问题时
//     还得先问一句日志在哪。
//   · **追加、不轮转**。它跨多次运行累积，于是一次「按了发送没反应」
//     能连着上一次成功的那次一起看——这正是它存在的意义。
//   · **默认关闭**。一次发送就写十几行，一直开着会把文件迅速刷爆，
//     反而看不出哪次是哪次（V1 config.py:213 的默认值也是 False）。
//   · **绝不影响主流程**。写不进去就静默放弃，V1 用 `except: pass` 兜住；
//     调试输出把功能搞挂是最不可接受的失败方式。
//
// ⚠️ 每次写都重新打开文件、写完就关，不长期持有句柄：native 是**另一个进程**，
// 它也会往同一个文件写（V1 里输入模拟与窗口在同一个进程，V2 拆成了两个）。
// 保持「打开-追加-关闭」意味着两个进程不会互相截断对方的输出。
package debuglog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// fileName 与 V1 完全一致（input_sender.py:31 的同名字面量）。
//
// 故意不加版本后缀：用户的排查习惯是按 V1 文档找这个文件，
// 换个名字就等于让他们猜。
const fileName = "ets2_translator_debug.log"

// enabled 是全局开关，由配置的 `debug_log` 驱动（V1 是模块级全局，
// 见 input_sender.py:19 的 `_debug_enabled`）。用 atomic 是因为它可能在
// 配置热重载时被改，而读它的是各个工作协程。
var enabled atomic.Bool

var mu sync.Mutex

// SetEnabled 开关调试日志。
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled 报告当前是否开启。
func Enabled() bool { return enabled.Load() }

// Path 返回调试日志的完整路径（供界面/文档显示「日志在哪」）。
func Path() string {
	return filepath.Join(os.TempDir(), fileName)
}

// nowStamp 生成与 V1 一致的时间戳 `HH:MM:SS.mmm`。
//
// Go 的 `15:04:05.000` 会输出毫秒并补零，与 V1 的
// `strftime('%H:%M:%S.%f')[:-3]`（截断到毫秒）等价。
func nowStamp() string {
	return time.Now().Format("15:04:05.000")
}

// Logf 写一条调试记录；未开启时是空操作。
//
// 格式与 V1 逐字一致：`HH:MM:SS.mmm [pid] msg`
// （input_sender.py:33 的 `strftime('%H:%M:%S.%f')[:-3]` 加 `[{pid}]`）。
// 毫秒是必须的——这一族的用途就是看「哪一步慢/哪一步没发生」，
// 精确到秒的话整个剪贴板序列会挤在同一秒里。
func Logf(format string, args ...any) {
	if !enabled.Load() {
		return
	}

	line := fmt.Sprintf("%s [%d] %s\n",
		nowStamp(), os.Getpid(), fmt.Sprintf(format, args...))

	// 加锁是防同进程内多个协程交错写；跨进程由「每次开-写-关」保证。
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // 静默：调试输出不该影响功能
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}
