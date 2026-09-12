package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 假 native ────────────────────────────────────────────────
//
// 发送链路里最容易出错的是**步骤顺序**（先藏窗、再按键、最后必须把窗放回来），
// 而不是系统调用本身。所以这里记录顺序，断言顺序。

type fakeNative struct {
	mu      sync.Mutex
	calls   []string
	sendErr error
	block   chan struct{} // 非 nil 时 SendChat 会等它，用来构造并发场景
}

func (f *fakeNative) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeNative) SendChat(_ context.Context, english string) error {
	// 先记录再阻塞：记录的是「这次调用发生了」，而测试要靠它判断并发场景
	// 已经进到 SendChat 里了。放在阻塞之后会永远等不到。
	f.record("send:" + english)
	if f.block != nil {
		<-f.block
	}
	return f.sendErr
}

func (f *fakeNative) SetOverlayVisible(_ context.Context, visible bool) error {
	f.record(fmt.Sprintf("visible:%v", visible))
	return nil
}

func (f *fakeNative) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newSender(t *testing.T, n NativeOps, translate func(string) (string, error), confirm func(string) bool) *Sender {
	t.Helper()
	return New(Options{
		Translate: translate,
		Native:    n,
		Confirm:   confirm,
		// 自测里把等待压到 0，否则每个用例都要真等 250ms
		HideDelay: time.Millisecond,
	})
}

// ── Validate：三条规则照抄 V1 compose_sender.py:57-71 ──

func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		chinese  string
		english  string
		want     bool
		why      string
	}{
		{"正常英文", "你好", "Hello", true, ""},
		{"空译文", "你好", "", false, "空译文发出去就是一条空消息"},
		{"只有空白", "你好", "   ", false, "同上，trim 后为空"},
		{"与原文相同", "你好", "你好", false, "压根没翻"},
		{"仍然是中文", "你好", "你好朋友", false, "模型没翻，发出去队友只看到中文"},
		{"中英混合但英文为主", "你好", "Hello 你好 world friends here", true, "CJK 占比低于 30% 可接受"},
		{"中英混合且中文为主", "你好", "你好 你好 world", false, "CJK 占比超过 30%"},
	}
	for _, c := range cases {
		if got := Validate(c.chinese, c.english); got != c.want {
			t.Errorf("%s: Validate(%q,%q)=%v，期望 %v（%s）",
				c.name, c.chinese, c.english, got, c.want, c.why)
		}
	}
}

// 30% 这条线的边界要稳：V1 用的是严格大于（`> 0.3`）。
func TestMostlyChineseBoundary(t *testing.T) {
	// 正好 10 个字符里 3 个 CJK = 30%，不大于 30% → 不算「大部分是中文」
	if mostlyChinese("一二三abcdefg") {
		t.Error("正好 30% 不该判为大部分是中文")
	}
	// 9 个里 3 个 ≈ 33.3% > 30% → 算
	if !mostlyChinese("一二三abcdef") {
		t.Error("33% 应判为大部分是中文")
	}
}

// ── 发送顺序 ──

func TestSendHidesThenSendsThenRestores(t *testing.T) {
	n := &fakeNative{}
	s := newSender(t, n, func(string) (string, error) { return "Hello", nil }, func(string) bool { return true })

	out := s.Send(context.Background(), "你好")
	if out.Result != ResultOKConfirmed {
		t.Fatalf("结果 = %s，期望 %s（原因: %s）", out.Result, ResultOKConfirmed, out.Message)
	}

	got := strings.Join(n.order(), " → ")
	want := "visible:false → send:Hello → visible:true"
	if got != want {
		t.Errorf("调用顺序 = %s，期望 %s", got, want)
	}
}

// 发送失败时窗口**必须**照样放回来。
//
// 这条比看上去重要：漏了它用户眼前就少一个窗口，而且他自己没法找回来。
func TestWindowIsRestoredWhenSendFails(t *testing.T) {
	n := &fakeNative{sendErr: errors.New("按键失败")}
	s := newSender(t, n, func(string) (string, error) { return "Hello", nil }, nil)

	out := s.Send(context.Background(), "你好")
	if out.Result != ResultFailSend {
		t.Fatalf("结果 = %s，期望 %s", out.Result, ResultFailSend)
	}

	order := n.order()
	if len(order) == 0 || order[len(order)-1] != "visible:true" {
		t.Errorf("发送失败后窗口没被放回来: %v", order)
	}
}

// 校验不过时**绝不能**真的发出去——这是这条链路最重要的安全性质。
func TestInvalidTranslationNeverSends(t *testing.T) {
	n := &fakeNative{}
	// 模型没翻，原样返回中文
	s := newSender(t, n, func(string) (string, error) { return "你好", nil }, nil)

	out := s.Send(context.Background(), "你好")
	if out.Result != ResultFailTranslate {
		t.Fatalf("结果 = %s，期望 %s", out.Result, ResultFailTranslate)
	}
	if len(n.order()) != 0 {
		t.Errorf("校验不通过却调用了 native: %v", n.order())
	}
}

// 翻译抛异常同样不该发，而且结果要是 FAIL_TRANSLATION（前端据此回填输入框）。
func TestTranslateErrorDoesNotSend(t *testing.T) {
	n := &fakeNative{}
	s := newSender(t, n, func(string) (string, error) { return "", errors.New("网络错误") }, nil)

	out := s.Send(context.Background(), "你好")
	if out.Result != ResultFailTranslate {
		t.Fatalf("结果 = %s，期望 %s", out.Result, ResultFailTranslate)
	}
	if len(n.order()) != 0 {
		t.Errorf("翻译失败却调用了 native: %v", n.order())
	}
}

// 同一时刻只允许一条：并发的第二条返回 BUSY，不排队。
func TestConcurrentSendIsBusy(t *testing.T) {
	release := make(chan struct{})
	n := &fakeNative{block: release}
	s := newSender(t, n, func(string) (string, error) { return "Hello", nil }, nil)

	first := make(chan Outcome, 1)
	go func() { first <- s.Send(context.Background(), "你好") }()

	// 等第一条真的进到 SendChat 里，再去并发第二条
	waitForCall(t, n, "send:Hello")

	second := s.Send(context.Background(), "你好")
	if second.Result != ResultBusy {
		t.Errorf("并发发送结果 = %s，期望 %s", second.Result, ResultBusy)
	}

	close(release)
	select {
	case out := <-first:
		if out.Result == ResultBusy {
			t.Error("第一条不该被判成 BUSY")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第一条发送没有返回")
	}
}

func waitForCall(t *testing.T, n *fakeNative, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range n.order() {
			if c == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等不到调用 %s，实际: %v", want, n.order())
}

// ── 聊天日志确认 ──

func TestChatLogConfirmer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_2026_09_11_log.txt")
	if err := os.WriteFile(path, []byte("[ETS2MP] [10:00:00] Bob (1): 历史消息\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &ChatLogConfirmer{Dir: dir, SelfName: func() string { return "Alice" }}

	// 历史里没有的那条文本，超时后应返回 false
	if c.Wait("Hello there", 200*time.Millisecond) {
		t.Fatal("历史里没有的文本不该确认成功")
	}

	// 追加一条**别人**说的同样内容 → 不该算作自己发送成功
	appendLine(t, path, "[ETS2MP] [10:00:05] Bob (1): Hello there")

	done := make(chan bool, 1)
	go func() { done <- c.Wait("Hello there", 2*time.Second) }()
	time.Sleep(300 * time.Millisecond)
	appendLine(t, path, "[ETS2MP] [10:00:06] Alice (1): Hello there")

	select {
	case ok := <-done:
		if !ok {
			t.Error("自己发的那条出现在日志里，却没确认成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("确认超时")
	}
}

// 玩家名没配置时不做说话人校验（V1 compose_sender.py:171 的处理）。
func TestChatLogConfirmerWithoutSelfName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_2026_09_11_log.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	c := &ChatLogConfirmer{Dir: dir, SelfName: func() string { return "" }}

	done := make(chan bool, 1)
	go func() { done <- c.Wait("Hello there", 2*time.Second) }()
	time.Sleep(200 * time.Millisecond)
	appendLine(t, path, "[ETS2MP] [10:00:07] Bob (1): Hello there")

	select {
	case ok := <-done:
		if !ok {
			t.Error("没配玩家名时，任何说话人说了这句都该算确认成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("确认超时")
	}
}

// 日志目录不存在时要安静地返回 false，不能 panic 也不能卡住。
func TestChatLogConfirmerMissingDir(t *testing.T) {
	c := &ChatLogConfirmer{Dir: filepath.Join(t.TempDir(), "不存在"), SelfName: func() string { return "Alice" }}
	if c.Wait("Hello", 100*time.Millisecond) {
		t.Error("目录不存在时不该确认成功")
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
