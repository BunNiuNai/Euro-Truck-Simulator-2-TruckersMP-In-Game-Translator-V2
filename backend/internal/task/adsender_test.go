package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 测试替身 ──────────────────────────────────────────────────

type fakeSender struct {
	mu    sync.Mutex
	calls []string
	err   error
	// block 让 SendChatMessage 挂住，用于验证 Stop 会等状态机退出
	block chan struct{}
}

func (f *fakeSender) SendChatMessage(_ context.Context, text string, vk, mods uint32, _ int) error {
	f.mu.Lock()
	f.calls = append(f.calls, text)
	err := f.err
	block := f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	return err
}

func (f *fakeSender) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeResolver 把任何热键都解析成同一个 VK（0 表示解析失败）。
type fakeResolver struct{ vk uint32 }

func (f fakeResolver) Resolve(string) (uint32, uint32) { return f.vk, 0 }

// statusLog 线程安全地收集状态推送。
//
// OnStatus 是在状态机协程里回调的，测试协程直接读切片会是数据竞争；
// 包一层锁，测试就从「怎么同步」里解放出来了。
type statusLog struct {
	mu   sync.Mutex
	list []Status
}

func (s *statusLog) add(st Status) {
	s.mu.Lock()
	s.list = append(s.list, st)
	s.mu.Unlock()
}

func (s *statusLog) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

func (s *statusLog) snapshot() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Status(nil), s.list...)
}

// fastSender 构造一个时间参数被压缩到毫秒级、间隔被压到秒级的发送器。
func fastSender(t *testing.T, sender Sender, messages []string, intervalSec int) (*AdSender, *statusLog) {
	t.Helper()

	seen := &statusLog{}

	a := New(Options{
		Sender:      sender,
		Resolver:    fakeResolver{vk: 89}, // 'Y'
		Hotkey:      "y",
		Tick:        5 * time.Millisecond,
		SendTimeout: time.Second,
		OnStatus:    seen.add,
	})
	// Config 的间隔单位是分钟；测试里直接设成秒级，免得等一分钟
	a.mu.Lock()
	a.messages = append([]string(nil), messages...)
	a.interval = time.Duration(intervalSec) * time.Second
	a.mu.Unlock()

	return a, seen
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时等待: %s", what)
}

// ── 启动校验 ──────────────────────────────────────────────────

func TestStartValidation(t *testing.T) {
	cases := []struct {
		name     string
		messages []string
		interval int
		resolver HotkeyResolver
		wantErr  string
	}{
		{"没有消息", []string{"", "  "}, 1, fakeResolver{vk: 89}, "至少填写一条"},
		{"全空消息", nil, 1, fakeResolver{vk: 89}, "至少填写一条"},
		{"间隔为 0", []string{"hello"}, 0, fakeResolver{vk: 89}, "大于 0 分钟"},
		{"热键解析失败", []string{"hello"}, 1, fakeResolver{vk: 0}, "无法解析呼出热键"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := New(Options{Sender: &fakeSender{}, Resolver: c.resolver, Hotkey: "y", Tick: time.Millisecond})
			a.mu.Lock()
			a.messages = c.messages
			a.interval = time.Duration(c.interval) * time.Minute
			a.mu.Unlock()

			err := a.Start()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误 = %q，期望包含 %q", err.Error(), c.wantErr)
			}
			if a.Status().Running {
				t.Error("校验失败时不应进入运行态")
			}
		})
	}
}

// ── 循环发送 ──────────────────────────────────────────────────

// 倒计时归零时发送当前条，然后前进到下一跳。
func TestSendsOnCountdownAndAdvances(t *testing.T) {
	sender := &fakeSender{}
	// 间隔 1 秒、tick 5ms → 约 200 次 tick 后发第一条；给足 3 秒观察两条
	a, _ := fastSender(t, sender, []string{"第一条", "第二条", "第三条"}, 1)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	waitFor(t, 3*time.Second, "发出两条", func() bool { return sender.count() >= 2 })

	got := sender.snapshot()
	if got[0] != "第一条" || got[1] != "第二条" {
		t.Errorf("发送顺序不对: %v", got)
	}

	st := a.Status()
	if st.Sent < 2 {
		t.Errorf("Sent = %d，期望 >= 2", st.Sent)
	}
	if st.CurrentIndex < 0 {
		t.Error("运行中 CurrentIndex 应为有效下标")
	}
}

// 空槽位必须被跳过——否则游戏里会收到一次空回车。
func TestSkipsEmptyMessages(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"A", "", "  ", "B"}, 1)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	waitFor(t, 3*time.Second, "发出两条", func() bool { return sender.count() >= 2 })

	for _, c := range sender.snapshot() {
		if strings.TrimSpace(c) == "" {
			t.Fatalf("发送了空消息: %q（全部调用: %v）", c, sender.snapshot())
		}
	}
	got := sender.snapshot()
	if got[0] != "A" || got[1] != "B" {
		t.Errorf("应只发非空且按序，实际: %v", got)
	}
}

// 从头开始：启动时 CurrentIndex 应从 -1 前进到第一条。
func TestStartBeginsAtFirstMessage(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"一", "二"}, 60) // 间隔很长，只观察起始状态

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	st := a.Status()
	if st.CurrentIndex != 0 {
		t.Errorf("CurrentIndex = %d，期望 0（第一条）", st.CurrentIndex)
	}
	if !st.Running {
		t.Error("应在运行态")
	}
	if st.RemainingSec != 60 {
		t.Errorf("RemainingSec = %d，期望 60", st.RemainingSec)
	}
}

// 倒计时应当在递减。
func TestCountdownDecrements(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"x"}, 60)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	first := a.Status().RemainingSec
	time.Sleep(60 * time.Millisecond) // tick=5ms → 至少十几次步进
	second := a.Status().RemainingSec

	if second >= first {
		t.Errorf("倒计时应递减：%d → %d", first, second)
	}
}

// ── 停止 ────────────────────────────────────────────────────

func TestStopIsIdempotentAndResets(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"x"}, 60)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a.Stop()
	a.Stop() // 重复调用不应 panic

	st := a.Status()
	if st.Running {
		t.Error("停止后不应在运行态")
	}
	if st.CurrentIndex != -1 {
		t.Errorf("停止后 CurrentIndex = %d，期望 -1", st.CurrentIndex)
	}
	if st.RemainingSec != 0 {
		t.Errorf("停止后 RemainingSec = %d，期望 0", st.RemainingSec)
	}
}

// Stop 之后**不再有新的发送**。
//
// 注意这里不等「在途的那一次」：V1 的 _ad_stop 也只停定时器，
// 已经在后台线程跑着的发送会自己跑完。等它反而会让停止操作被一次
// 1.5 秒的发送拖住。
func TestStopPreventsFurtherSends(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"x"}, 1)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "开始发送", func() bool { return sender.count() >= 1 })

	a.Stop()
	after := sender.count()

	// tick=5ms，给足时间让「如果没停住」的循环再发好几次
	time.Sleep(250 * time.Millisecond)

	if got := sender.count(); got != after {
		t.Errorf("Stop 之后又发了 %d 次（%d → %d）", got-after, after, got)
	}
	if a.Status().Running {
		t.Error("Stop 后不应在运行态")
	}
}

// Stop 必须等状态机协程真正退出再返回，否则紧随其后的 Start 会与旧协程打架。
func TestStopWaitsForLoopToExit(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"x"}, 60)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a.Stop()

	// loop 已退出（doneCh 已关闭）——重复 Stop 与重启都应安全
	a.Stop()
	if err := a.Start(); err != nil {
		t.Fatalf("停止后应能重新启动: %v", err)
	}
	defer a.Stop()

	if !a.Status().Running {
		t.Error("重新启动后应在运行态")
	}
}

// ── 失败记录 ──────────────────────────────────────────────────

func TestSendFailureIsRecorded(t *testing.T) {
	sender := &fakeSender{err: errors.New("按键模拟被系统拒绝")}
	a, _ := fastSender(t, sender, []string{"x"}, 1)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	waitFor(t, 3*time.Second, "记录失败", func() bool { return a.Status().Failed >= 1 })

	st := a.Status()
	if st.Sent != 0 {
		t.Errorf("失败时 Sent 应为 0，实际 %d", st.Sent)
	}
	if !strings.Contains(st.LastError, "按键模拟被系统拒绝") {
		t.Errorf("LastError = %q", st.LastError)
	}
}

// ── 配置变更 ──────────────────────────────────────────────────

// 运行中改配置不该打断循环。
func TestConfigureWhileRunningKeepsRunning(t *testing.T) {
	sender := &fakeSender{}
	a, _ := fastSender(t, sender, []string{"旧"}, 60)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	a.Configure([]string{"新一", "新二"}, 30, "shift+y")

	st := a.Status()
	if !st.Running {
		t.Fatal("改配置后应仍在运行")
	}
	if st.Total != 2 {
		t.Errorf("Total = %d，期望 2", st.Total)
	}
	if st.IntervalMin != 30 {
		t.Errorf("IntervalMin = %d，期望 30", st.IntervalMin)
	}
}

// 配置里的空热键不应覆盖已有值（避免把可用的热键清成空）。
func TestConfigureIgnoresEmptyHotkey(t *testing.T) {
	a := New(Options{Sender: &fakeSender{}, Resolver: fakeResolver{vk: 89}, Hotkey: "y"})
	a.Configure([]string{"x"}, 1, "")

	a.mu.Lock()
	got := a.hotkey
	a.mu.Unlock()
	if got != "y" {
		t.Errorf("hotkey = %q，期望保持 y", got)
	}
}

// ── 状态推送 ──────────────────────────────────────────────────

// 状态必须持续推送给 UI（剩余秒数要能实时显示）。
func TestStatusIsPublished(t *testing.T) {
	sender := &fakeSender{}
	a, seen := fastSender(t, sender, []string{"x"}, 60)

	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, "收到状态推送", func() bool { return seen.len() >= 2 })
	a.Stop()

	snap := seen.snapshot()
	if len(snap) == 0 {
		t.Fatal("没有任何状态推送")
	}
	// 至少有一次是运行态
	running := false
	for _, s := range snap {
		if s.Running {
			running = true
		}
	}
	if !running {
		t.Error("推送里没有运行态快照")
	}
}

// 状态快照的字段名是对前端的契约，改了就断了。
func TestStatusShapeIsStable(t *testing.T) {
	a := New(Options{Sender: &fakeSender{}, Resolver: fakeResolver{vk: 89}})
	st := a.Status()

	// 未启动时的零值语义
	if st.Running {
		t.Error("初始不应是运行态")
	}
	if st.CurrentIndex != -1 {
		t.Errorf("初始 CurrentIndex = %d，期望 -1（未启动）", st.CurrentIndex)
	}
	if fmt.Sprint(st.Sent) != "0" || st.Failed != 0 {
		t.Errorf("初始计数应为 0: %+v", st)
	}
}
