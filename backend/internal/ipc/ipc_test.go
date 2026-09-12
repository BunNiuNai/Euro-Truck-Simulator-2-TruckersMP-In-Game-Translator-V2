package ipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 帧编解码（纯内存，不依赖 native） ──

func TestFrameRoundTrip(t *testing.T) {
	cases := []Message{
		{Type: "ping", ID: 1, Payload: []byte(`{"seq":1}`)},
		{Type: "native.hello", ID: 42, Payload: []byte(`{"protocolVersion":1}`)},
		{Type: "input.send", ID: 7,
			Payload: []byte(`{"text":"你好\n\"引号\"\t制表","hotkey":"y"}`)},
		{Type: "event.error", Payload: []byte(`{"code":"X","message":"含换行的\n文本"}`)},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, want); err != nil {
			t.Fatalf("WriteFrame(%s): %v", want.Type, err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame(%s): %v", want.Type, err)
		}
		if got.Type != want.Type || got.ID != want.ID {
			t.Errorf("往返后 = {%s %d}，期望 {%s %d}", got.Type, got.ID, want.Type, want.ID)
		}
		if string(got.Payload) != string(want.Payload) {
			t.Errorf("payload 往返不一致:\n  got  %s\n  want %s", got.Payload, want.Payload)
		}
	}
}

func TestFrameMultipleInOneStream(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 5; i++ {
		if err := WriteFrame(&buf, Message{Type: "ping", ID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		m, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("第 %d 帧: %v", i, err)
		}
		if m.ID != int64(i+1) {
			t.Errorf("第 %d 帧 id = %d", i, m.ID)
		}
	}
}

func TestReadFrameRejectsOversizedLength(t *testing.T) {
	// 手工构造一个声明巨大长度的帧头
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxFrameBytes+1)
	buf.Write(hdr[:])

	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("超限长度应被拒绝（否则会尝试分配巨量内存）")
	}
}

func TestReadFrameHandlesTruncatedHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x01, 0x02}) // 只有 2 字节
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("截断的帧头应报错")
	}
}

func TestEmptyFrameIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 0)
	buf.Write(hdr[:])

	m, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("零长度帧不应报错: %v", err)
	}
	if m.Type != "" {
		t.Errorf("零长度帧应为空消息，得到 %+v", m)
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	big := strings.Repeat("x", MaxFrameBytes+10)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Message{Type: "x", Payload: []byte(`"` + big + `"`)}); err == nil {
		t.Fatal("超大 payload 应被拒绝")
	}
}

// ── 跨语言端到端：Go ↔ C++ ──

// nativeExePath 在常见位置寻找编译好的 native 程序。
func nativeExePath() string {
	candidates := []string{
		filepath.Join("..", "..", "..", "dist", "translator_native.exe"),
		filepath.Join("..", "..", "..", "..", "dist", "translator_native.exe"),
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

// startNative 启动 native 进程。
//
// 注意：**不要**用 bytes.Buffer 接子进程输出——那会让 Go 创建匿名管道，
// 而受限环境禁止命名/匿名管道，会导致进程启动异常或测试挂起。
// 这里改为写文件，既不依赖管道，输出也能用于失败诊断。
func startNative(t *testing.T, exe, pipeName string) (*exec.Cmd, func() string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "native.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("创建日志文件失败: %v", err)
	}
	t.Cleanup(func() { logFile.Close() })

	cmd := exec.Command(exe, "--pipe", pipeName)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 native 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	readLog := func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}
	return cmd, readLog
}

// dialOrSkip 连接管道；若因环境禁止管道而失败，跳过并说明如何正确运行。
//
// 受限沙箱会拦截所有 Named Pipe 打开操作（返回 Access is denied）。
// 那种情况是环境限制，不是代码问题——跳过而不是假装通过，也不误报失败。
func dialOrSkip(t *testing.T, pipeName string) *Client {
	t.Helper()
	client, err := Dial(pipeName, 5*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "Access is denied") ||
			strings.Contains(err.Error(), "拒绝访问") {
			t.Skipf("当前环境禁止打开命名管道（Access is denied）——"+
				"请在普通终端运行 `go test ./internal/ipc/...` 以获得权威结果。原始错误: %v", err)
		}
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// 这是 V2 第一个真正的跨语言验证：Go 通过 Named Pipe 与 C++ 进程对话。
//
// 若 native 程序未编译，测试会跳过并提示如何编译——不静默通过。
func TestCrossLanguageHelloPingShutdown(t *testing.T) {
	exe := nativeExePath()
	if exe == "" {
		t.Skip("未找到 dist/translator_native.exe；先执行 " +
			"cmake --build native/build --config Release")
	}

	// 每次用独立管道名，避免与其他测试/实例冲突
	pipeName := fmt.Sprintf(`\\.\pipe\ets2translator-test-%d-%d`,
		os.Getpid(), time.Now().UnixNano())

	cmd, readLog := startNative(t, exe, pipeName)
	client := dialOrSkip(t, pipeName)
	_ = cmd

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1) 握手：必须报告协议版本匹配与能力清单
	caps, versionMatch, err := client.Handshake(ctx)
	if err != nil {
		t.Fatalf("握手失败: %v\nnative 输出:\n%s", err, readLog())
	}
	if !versionMatch {
		t.Errorf("协议版本不匹配（Go=%d）", ProtocolVersion)
	}
	t.Logf("native 报告的能力: %v", caps)
	if len(caps) == 0 {
		t.Error("native 应至少报告一项能力")
	}
	hasPipe := false
	for _, c := range caps {
		if c == "pipe" {
			hasPipe = true
		}
	}
	if !hasPipe {
		t.Errorf("能力清单应包含 pipe，实际 %v", caps)
	}

	// 2) 心跳往返
	if err := client.Ping(ctx, 1); err != nil {
		t.Fatalf("心跳失败: %v", err)
	}
	if err := client.Ping(ctx, 2); err != nil {
		t.Fatalf("第二次心跳失败: %v", err)
	}

	// 3) 未实现的消息必须明确回报 UNSUPPORTED，而不是静默忽略
	//
	// ⚠️ 消息名取协议表里没有的 `bogus.*`——这是**命名约定，不是机制保证**：
	// 协议没禁止谁将来注册一个 bogus.* 消息，那样本用例会再次失效（fail-loud，
	// 修法与这次同型）。真正不能做的是拿真实能力名充当"未实现"。
	//
	// 本用例原先写的是 tray.set——写的时候托盘确实还没实现。后来托盘实现了，
	// 它开始返回 event.stateChanged，用例随之变成恒失败（而产品侧其实是对的）。
	// 真实能力名会随时间一个个失效，`bogus.*` 不会：dispatch.cpp 的兜底分支
	// 对任何未注册的消息类型都返回 UNSUPPORTED。
	resp, err := client.Call(ctx, "bogus.set", map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("调用未实现消息失败: %v", err)
	}
	if resp.Type != "event.error" {
		t.Errorf("未实现消息应回 event.error，实际 %s", resp.Type)
	}
	var errPayload struct {
		Code string `json:"code"`
	}
	_ = resp.DecodePayload(&errPayload)
	if errPayload.Code != "UNSUPPORTED" {
		t.Errorf("错误码 = %q，期望 UNSUPPORTED（静默忽略会让 Go 以为命令生效了）", errPayload.Code)
	}

	// 4) 出错的请求之后连接应仍可用
	if err := client.Ping(ctx, 3); err != nil {
		t.Fatalf("未实现消息之后连接应仍可用: %v", err)
	}

	// 5) 优雅退出
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := client.Call(shutdownCtx, "shutdown", nil); err != nil {
		t.Fatalf("shutdown 失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// 正常退出
	case <-time.After(5 * time.Second):
		t.Errorf("native 未在收到 shutdown 后退出\n输出:\n%s", readLog())
	}
}

// 连接建立后对端消失时，读循环应结束、后续调用应报错而不是永久挂起。
func TestClientDetectsPeerClose(t *testing.T) {
	exe := nativeExePath()
	if exe == "" {
		t.Skip("未找到 dist/translator_native.exe")
	}
	pipeName := fmt.Sprintf(`\\.\pipe\ets2translator-close-%d-%d`,
		os.Getpid(), time.Now().UnixNano())

	cmd, _ := startNative(t, exe, pipeName)
	client := dialOrSkip(t, pipeName)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := client.Handshake(ctx); err != nil {
		t.Fatalf("握手失败: %v", err)
	}

	// 杀死 native，模拟崩溃
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	// 后续调用必须在超时内失败，而不是永久阻塞
	shortCtx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := client.Call(shortCtx, "ping", map[string]int64{"seq": 9}); err == nil {
		t.Error("对端已消失，调用应失败")
	}
}
