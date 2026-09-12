package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ipc"
)

// ── 假 native：有状态地模拟真实 native 的行为 ──────────────────
//
// 关键点：它**真的**维护窗口状态，并对命令做出与 dispatch.cpp 一致的回复。
// 一个只会回 {"ok":true} 的哑桩测不出「实际状态是否来自回报」——
// 那正是 ADR-012 的核心。

type fakeCall struct {
	Type    string
	Payload json.RawMessage
}

type fakeNative struct {
	mu       sync.Mutex
	calls    []fakeCall
	events   chan ipc.Message
	closed   bool
	caps     []string
	version  bool
	pingErr  error
	pings    int
	skips    []string   // replay 时回报的 skipped
	forceErr *RemoteError // 强制某类命令报错
	errOn    string
	display  *DisplayActual
}

func newFakeNative(caps ...string) *fakeNative {
	if caps == nil {
		caps = []string{CapPipe, CapJSON, CapWindow}
	}
	return &fakeNative{
		events:  make(chan ipc.Message, 16),
		caps:    caps,
		version: true,
	}
}

func (f *fakeNative) Handshake(ctx context.Context) ([]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, false, errors.New("已关闭")
	}
	return f.caps, f.version, nil
}

func (f *fakeNative) Ping(ctx context.Context, seq int64) error {
	f.mu.Lock()
	f.pings++
	err := f.pingErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (f *fakeNative) Events() <-chan ipc.Message { return f.events }

func (f *fakeNative) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

func (f *fakeNative) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeNative) countOf(msgType string) int {
	n := 0
	for _, c := range f.recorded() {
		if c.Type == msgType {
			n++
		}
	}
	return n
}

func (f *fakeNative) pingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *fakeNative) lastOf(msgType string) (fakeCall, bool) {
	calls := f.recorded()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Type == msgType {
			return calls[i], true
		}
	}
	return fakeCall{}, false
}

// sendEvent 向主管推送一个事件
func (f *fakeNative) sendEvent(m ipc.Message) {
	f.mu.Lock()
	ch := f.events
	f.mu.Unlock()
	ch <- m
}

func mustJSON(t *testing.T, v any) ipc.Message {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return ipc.Message{Payload: b}
}

func (f *fakeNative) Call(ctx context.Context, msgType string, payload any) (ipc.Message, error) {
	raw, _ := json.Marshal(payload)

	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Type: msgType, Payload: raw})
	closed := f.closed
	errOn := f.errOn
	forceErr := f.forceErr
	f.mu.Unlock()

	if closed {
		return ipc.Message{}, errors.New("连接已关闭")
	}
	if errOn != "" && errOn == msgType {
		return ipc.Message{Type: "event.error", Payload: mustMarshal(map[string]any{
			"ok": false, "code": forceErr.Code, "message": forceErr.Message,
		})}, nil
	}

	// 模拟真实 native 的行为：真的维护窗口状态
	var spec map[string]any
	_ = json.Unmarshal(raw, &spec)

	f.mu.Lock()
	defer f.mu.Unlock()

	switch msgType {
	case "display.create":
		d := &DisplayActual{Exists: true, Blur: "mica"}
		if f.display != nil {
			d.Blur = f.display.Blur
		}
		if v, ok := spec["width"].(float64); ok {
			d.Width = int(v)
		}
		if v, ok := spec["height"].(float64); ok {
			d.Height = int(v)
		}
		if v, ok := spec["x"].(float64); ok {
			d.X = int(v)
		}
		if v, ok := spec["y"].(float64); ok {
			d.Y = int(v)
		}
		if v, ok := spec["visible"].(bool); ok {
			d.Visible = v
		} else {
			d.Visible = true
		}
		if v, ok := spec["blur"].(string); ok {
			d.Blur = v
		}
		f.display = d
		return mustJSONQuiet(map[string]any{"ok": true, "display": d}), nil

	case "display.visible":
		if f.display == nil {
			return mustJSONQuiet(map[string]any{"ok": false, "code": "NO_WINDOW", "message": "未建窗"}), nil
		}
		if v, ok := spec["visible"].(bool); ok {
			f.display.Visible = v
		}
		return mustJSONQuiet(map[string]any{"ok": true, "display": f.display}), nil

	case "display.setClickThrough":
		if f.display == nil {
			return mustJSONQuiet(map[string]any{"ok": false, "code": "NO_WINDOW", "message": "未建窗"}), nil
		}
		if v, ok := spec["enabled"].(bool); ok {
			f.display.ClickThrough = v
		}
		return mustJSONQuiet(map[string]any{"ok": true, "display": f.display}), nil

	case "window.blur":
		if f.display == nil {
			return mustJSONQuiet(map[string]any{"ok": false, "code": "NO_WINDOW", "message": "未建窗"}), nil
		}
		if v, ok := spec["mode"].(string); ok {
			f.display.Blur = v
		}
		return mustJSONQuiet(map[string]any{"ok": true, "display": f.display}), nil

	case "native.replay":
		applied := []string{}
		var reg struct {
			Display *Display  `json:"display"`
			Hotkeys []Hotkey  `json:"hotkeys"`
			Tray    *Tray     `json:"tray"`
		}
		if v, ok := spec["registry"]; ok {
			b, _ := json.Marshal(v)
			_ = json.Unmarshal(b, &reg)
		}
		if reg.Display != nil {
			d := &DisplayActual{
				Exists: true, Width: reg.Display.Width, Height: reg.Display.Height,
				X: reg.Display.X, Y: reg.Display.Y, Visible: reg.Display.Visible,
				ClickThrough: reg.Display.ClickThrough, Blur: reg.Display.Blur,
			}
			f.display = d
			applied = append(applied, "display")
		}
		skipped := f.skips
		if reg.Hotkeys != nil {
			skipped = append(append([]string{}, skipped...), "hotkeys")
		}
		if reg.Tray != nil {
			skipped = append(skipped, "tray")
		}
		return mustJSONQuiet(map[string]any{
			"ok": true, "applied": applied, "skipped": skipped,
			"actualState": map[string]any{"connected": true, "display": f.display},
		}), nil
	}

	return mustJSONQuiet(map[string]any{"ok": false, "code": "UNSUPPORTED",
		"message": "本阶段尚未实现: " + msgType}), nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func mustJSONQuiet(v any) ipc.Message {
	return ipc.Message{Payload: mustMarshal(v)}
}

// ── 测试脚手架 ──────────────────────────────────────────────

// injector 记录每次拨号返回的假连接，便于断言「重连了」。
type injector struct {
	mu    sync.Mutex
	conns []*fakeNative
	err   error
	// newFake 每次拨号时构造连接，nil 则复用同一个
	newFake func() *fakeNative
	shared  *fakeNative
	dials   int
}

func (i *injector) dial(pipeName string, timeout time.Duration) (conn, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.dials++
	if i.err != nil {
		return nil, i.err
	}
	var c *fakeNative
	if i.newFake != nil {
		c = i.newFake()
	} else {
		c = i.shared
	}
	i.conns = append(i.conns, c)
	return c, nil
}

func (i *injector) dialCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.dials
}

func (i *injector) last() *fakeNative {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.conns) == 0 {
		return nil
	}
	return i.conns[len(i.conns)-1]
}

// at 返回第 n 条连接（0 起）。
func (i *injector) at(n int) *fakeNative {
	i.mu.Lock()
	defer i.mu.Unlock()
	if n < 0 || n >= len(i.conns) {
		return nil
	}
	return i.conns[n]
}

// newTestSupervisor 构造一个时间参数被压缩到毫秒级的主管。
func newTestSupervisor(t *testing.T, inj *injector) *Supervisor {
	t.Helper()
	s := New("\\\\.\\pipe\\test", "", WithDialer(inj.dial))
	s.hbInterval = 15 * time.Millisecond
	s.hbMisses = 2
	s.minBackoff = 5 * time.Millisecond
	s.maxBackoff = 20 * time.Millisecond
	s.callWait = 500 * time.Millisecond
	return s
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

// runSupervisor 在后台跑主管，返回取消函数
func runSupervisor(t *testing.T, s *Supervisor) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(ctx); err != nil {
			t.Errorf("Run 返回错误: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run 未在 3 秒内退出")
		}
	})
	return cancel
}

// ── 用例 ────────────────────────────────────────────────────

// 连上之后必须整体重放期望状态——这是 ADR-012 的全部意义。
func TestReplayOnConnect(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	s.SetDisplay(Display{X: 10, Y: 20, Width: 800, Height: 400, Blur: "mica", Visible: true})

	runSupervisor(t, s)
	waitFor(t, 2*time.Second, "发出 native.replay", func() bool {
		return fake.countOf("native.replay") > 0
	})

	call, _ := fake.lastOf("native.replay")
	var body struct {
		Registry struct {
			Display Display `json:"display"`
		} `json:"registry"`
	}
	if err := json.Unmarshal(call.Payload, &body); err != nil {
		t.Fatalf("重放载荷不是合法 JSON: %v", err)
	}
	got := body.Registry.Display
	if got.Width != 800 || got.Height != 400 || got.X != 10 || got.Y != 20 {
		t.Errorf("重放的窗口规格不对: %+v", got)
	}
	if got.Blur != "mica" {
		t.Errorf("重放的 blur = %q，期望 mica", got.Blur)
	}
}

// 期望状态必须在断线后存活：换一条新连接，重放的内容要一模一样。
func TestDesiredSurvivesReconnect(t *testing.T) {
	inj := &injector{newFake: func() *fakeNative { return newFakeNative() }}
	s := newTestSupervisor(t, inj)
	s.SetDisplay(Display{X: 5, Y: 6, Width: 700, Height: 500, Blur: "acrylic", Visible: true})

	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "第一条连接完成重放", func() bool {
		c := inj.last()
		return c != nil && c.countOf("native.replay") > 0
	})

	// 掐断第一条连接，逼主管重连
	first := inj.last()
	_ = first.Close()

	waitFor(t, 3*time.Second, "建立第二条连接", func() bool {
		return inj.dialCount() >= 2
	})
	waitFor(t, 3*time.Second, "第二条连接也完成重放", func() bool {
		c := inj.last()
		return c != nil && c != first && c.countOf("native.replay") > 0
	})

	second := inj.last()
	call, _ := second.lastOf("native.replay")
	var body struct {
		Registry struct {
			Display Display `json:"display"`
		} `json:"registry"`
	}
	if err := json.Unmarshal(call.Payload, &body); err != nil {
		t.Fatalf("重放载荷不是合法 JSON: %v", err)
	}
	if body.Registry.Display.Width != 700 || body.Registry.Display.Height != 500 {
		t.Errorf("重连后期望状态丢了: %+v", body.Registry.Display)
	}
	if body.Registry.Display.Blur != "acrylic" {
		t.Errorf("重连后 blur 丢了: %q", body.Registry.Display.Blur)
	}
}

// native 没报告 window 能力时，不下发窗口命令，但期望状态照样记住。
func TestCapabilityGating(t *testing.T) {
	fake := newFakeNative(CapPipe, CapJSON) // 没有 window
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	err := s.SetDisplay(Display{X: 1, Y: 2, Width: 300, Height: 200})
	if !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("缺 window 能力时应返回 ErrCapabilityMissing，实际: %v", err)
	}

	// 缺能力时不发 display.create
	if n := fake.countOf("display.create"); n != 0 {
		t.Errorf("缺 window 能力却下发了 %d 次 display.create", n)
	}
	// 重放里也不该带 display 段（带了只会拿到 skipped，是噪音）
	call, ok := fake.lastOf("native.replay")
	if ok {
		var body struct {
			Registry struct {
				Display any `json:"display"`
			} `json:"registry"`
		}
		_ = json.Unmarshal(call.Payload, &body)
		if body.Registry.Display != nil {
			t.Errorf("缺 window 能力时重放不该带 display 段")
		}
	}
	// 但期望状态本身要留着，等能力上线就能自动生效
	if s.Status().Desired.Display == nil || s.Status().Desired.Display.Width != 300 {
		t.Errorf("期望状态被丢掉了: %+v", s.Status().Desired.Display)
	}
}

// 未连接时下发命令要报错，但不能丢掉期望状态。
func TestPushWhileDisconnected(t *testing.T) {
	inj := &injector{err: errors.New("管道不存在")}
	s := newTestSupervisor(t, inj)

	err := s.SetVisible(false)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("未连接时应返回 ErrNotConnected，实际: %v", err)
	}
	if s.Status().Desired.Display.Visible {
		t.Error("未连接时设置 Visible=false 也应记进期望状态")
	}
}

// 心跳连续无应答必须触发重连，不能靠被动等待写失败。
// 用「第一条连接假死、之后恢复正常」来测，顺带覆盖恢复路径。
func TestHeartbeatTriggersReconnect(t *testing.T) {
	var n int
	inj := &injector{newFake: func() *fakeNative {
		f := newFakeNative()
		n++
		if n == 1 {
			f.pingErr = errors.New("假死") // 只有第一条连接无应答
		}
		return f
	}}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 3*time.Second, "心跳失败后重连", func() bool {
		return inj.dialCount() >= 2
	})
	// 第二条连接应当稳定下来，而不是继续被踢
	waitFor(t, 2*time.Second, "重连后恢复连接", func() bool {
		return s.Status().Connected
	})

	st := s.Status()
	if st.Reconnects == 0 {
		t.Error("应记录重连次数")
	}
	// 重连成功后 LastError 会被清空，所以中断原因要看 LastDisconnect——
	// 否则「悬浮窗为什么消失了」这个问题会被一次成功重连抹掉答案。
	if !strings.Contains(st.LastDisconnect, "心跳") {
		t.Errorf("中断原因应说明心跳失败，实际: %q", st.LastDisconnect)
	}

	first := inj.at(0)
	if first == nil {
		t.Fatal("应记录第一条连接")
	}
	if got := first.pingCount(); got < 2 {
		t.Errorf("判定失联前应至少 ping 2 次，实际 %d 次", got)
	}
	// 第二条连接必须重新完成重放，否则窗口在重连后是空的
	second := inj.at(1)
	if second == nil || second.countOf("native.replay") == 0 {
		t.Error("重连后应重新重放期望状态")
	}
}

// native 用 event.error 明确报错时，必须变成 Go 侧的错误，不能当成成功。
func TestRemoteErrorPropagates(t *testing.T) {
	fake := newFakeNative()
	fake.errOn = "display.create"
	fake.forceErr = &RemoteError{Code: "CREATE_FAILED", Message: "建窗失败"}
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	err := s.SetDisplay(Display{Width: 400, Height: 300})
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("应返回 RemoteError，实际: %v", err)
	}
	if re.Code != "CREATE_FAILED" {
		t.Errorf("错误码 = %q，期望 CREATE_FAILED", re.Code)
	}
}

// ok=false 的普通回复同样是失败，只看 err 会漏掉。
func TestOkFalseIsError(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })
	waitFor(t, 2*time.Second, "重放已建窗", func() bool { return fake.countOf("native.replay") > 0 })

	// replay 已经建了窗，先掐掉实际状态以触发 NO_WINDOW 路径不现实，
	// 改为断言未建窗时 native 返回的 ok=false 被识别成错误。
	fake.mu.Lock()
	fake.display = nil
	fake.mu.Unlock()

	err := s.SetClickThrough(true)
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("ok=false 应转成 RemoteError，实际: %v", err)
	}
	if re.Code != "NO_WINDOW" {
		t.Errorf("错误码 = %q，期望 NO_WINDOW", re.Code)
	}
}

// 实际状态必须来自 native 的回报，不能是 Go 自己猜的。
func TestActualStateComesFromNative(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })
	waitFor(t, 2*time.Second, "重放完成", func() bool {
		st := s.Status()
		return st.Actual.Display != nil
	})

	// 请求 640x360，但让 native 实际回报 123x456——Go 必须采信回报
	fake.mu.Lock()
	fake.display = &DisplayActual{Exists: true, Width: 123, Height: 456, Visible: true, Blur: "mica"}
	fake.mu.Unlock()

	if err := s.SetVisible(true); err != nil {
		t.Fatalf("SetVisible: %v", err)
	}

	st := s.Status()
	if st.Actual.Display == nil {
		t.Fatal("实际状态为空")
	}
	if st.Actual.Display.Width != 123 || st.Actual.Display.Height != 456 {
		t.Errorf("实际状态应采信 native 回报，得到 %+v", st.Actual.Display)
	}
	if st.Desired.Display.Width == 123 {
		t.Error("期望状态不该被实际状态覆盖")
	}
}

// native 回报的 skipped 要如实记录，不能假装成功。
//
// ⚠️ 这个用例的名字与注释换过两轮：最早拿 hotkeys 当「未实现」的例子，
// 热键实现后换成 tray，现在**托盘也实现了**。协议里已经没有未实现的命令，
// 所以这里不再借某个真实能力当例子——而是让假连接直接回报 skipped，
// 验证的不变量始终是同一条：**Go 必须把 native 跳过的东西暴露出来**，
// 而不是当作一切正常（老版本 native 连上来时就会走到这条路）。
func TestSkippedCapabilitiesRecorded(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)

	s.mu.Lock()
	s.desired.Tray = &Tray{Enabled: true}
	s.mu.Unlock()

	runSupervisor(t, s)
	waitFor(t, 3*time.Second, "记录 skipped", func() bool {
		return strings.Contains(s.Status().LastError, "tray")
	})

	if !strings.Contains(s.Status().LastError, "暂未实现") {
		t.Errorf("错误信息应说明暂未实现，实际: %q", s.Status().LastError)
	}
}

// 热键现在已实现：下发的应当是解析好的 VK/Mods，而不是热键字符串。
func TestHotkeysAreResolvedBeforeSend(t *testing.T) {
	// 假 native 必须报告 hotkey 能力，否则 Supervisor 会按「没这能力」跳过
	fake := newFakeNative(CapPipe, CapJSON, CapWindow, CapHotkey)
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	// 注入解析器：ctrl+alt+t → vk='T'(84), mods=CONTROL|ALT(3)
	s.resolver = stubResolver{vk: 84, mods: 0x0001 | 0x0002}

	// 连接前设置期望状态（会返回 ErrNotConnected，但状态已经记下了）
	if err := s.SetHotkeys([]Hotkey{{ID: "toggle", Combo: "ctrl+alt+t", Enabled: true}}); err == nil {
		t.Error("连接前调用应返回 ErrNotConnected")
	}

	runSupervisor(t, s)
	waitFor(t, 3*time.Second, "热键已重放", func() bool { return fake.countOf("native.replay") > 0 })

	call, ok := fake.lastOf("native.replay")
	if !ok {
		t.Fatal("应发出 native.replay")
	}
	var body struct {
		Registry struct {
			Hotkeys []HotkeySpec `json:"hotkeys"`
		} `json:"registry"`
	}
	if err := json.Unmarshal(call.Payload, &body); err != nil {
		t.Fatalf("重放载荷不是合法 JSON: %v", err)
	}
	if len(body.Registry.Hotkeys) != 1 {
		t.Fatalf("应带 1 个热键，实际 %d 个", len(body.Registry.Hotkeys))
	}
	got := body.Registry.Hotkeys[0]
	if got.VK != 84 {
		t.Errorf("VK = %d，期望 84（解析后的值，不是字符串）", got.VK)
	}
	if got.Mods != 0x0003 {
		t.Errorf("Mods = %d，期望 3（CONTROL|ALT）", got.Mods)
	}
}

// 解析不出的热键要如实报错，不能静默丢弃。
func TestUnresolvableHotkeyIsReported(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	s.resolver = stubResolver{vk: 0} // 一律解析失败

	runSupervisor(t, s)
	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	err := s.SetHotkeys([]Hotkey{{ID: "weird", Combo: "???", Enabled: true}})
	if err == nil {
		t.Fatal("全部解析失败时应报错")
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("错误信息应指出是哪个热键，实际: %q", err.Error())
	}
}

// stubResolver 是固定返回值的解析器替身。
type stubResolver struct {
	vk   uint32
	mods uint32
}

func (r stubResolver) Resolve(string) (uint32, uint32) { return r.vk, r.mods }

// 单命令写入要同时更新期望状态与实际状态。
func TestCommandUpdatesBothStates(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "重放完成", func() bool {
		return s.Status().Actual.Display != nil
	})

	if err := s.SetBlur("acrylic"); err != nil {
		t.Fatalf("SetBlur: %v", err)
	}
	st := s.Status()
	if st.Desired.Display.Blur != "acrylic" {
		t.Errorf("期望 blur = %q", st.Desired.Display.Blur)
	}
	if st.Actual.Display.Blur != "acrylic" {
		t.Errorf("实际 blur = %q", st.Actual.Display.Blur)
	}

	if err := s.SetClickThrough(true); err != nil {
		t.Fatalf("SetClickThrough: %v", err)
	}
	if !s.Status().Actual.Display.ClickThrough {
		t.Error("点击穿透的实际状态应为 true")
	}
}

// 拨号失败要退避重试，而不是放弃或忙等。
func TestDialFailureBacksOff(t *testing.T) {
	inj := &injector{err: errors.New("管道不存在")}
	s := newTestSupervisor(t, inj)
	s.minBackoff = 5 * time.Millisecond
	s.maxBackoff = 15 * time.Millisecond
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "多次重试", func() bool { return inj.dialCount() >= 3 })

	st := s.Status()
	if st.Connected {
		t.Error("拨号失败时不应报 connected")
	}
	if st.LastError == "" {
		t.Error("应记录最后一次失败原因")
	}
}

// 事件推送要更新实际状态。
func TestEventUpdatesActualState(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })
	waitFor(t, 2*time.Second, "重放完成", func() bool { return s.Status().Actual.Display != nil })

	send := func() {
		fake.sendEvent(ipc.Message{
			Type: "event.stateChanged",
			Payload: mustMarshal(map[string]any{
				"ok": true,
				"display": map[string]any{
					"exists": true, "width": 999, "height": 111, "visible": false, "blur": "none",
				},
			}),
		})
	}

	send()
	waitFor(t, 2*time.Second, "事件更新实际宽度", func() bool {
		d := s.Status().Actual.Display
		return d != nil && d.Width == 999
	})

	d := s.Status().Actual.Display
	if d.Height != 111 || d.Visible {
		t.Errorf("事件里的实际状态没被完整采纳: %+v", d)
	}
}

// NoteDisplayBounds 只记事实，**不下发任何命令**。
//
// 这条断言很关键：如果它顺手走了 SetDisplay，native 的 applyDisplay 是
// 「先销毁再重建」窗口——用户每次拖完窗口都会闪一下。所以光看期望状态对不对
// 不够，必须同时断言「没有新命令发出去」。
func TestNoteDisplayBoundsDoesNotPush(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	before := len(fake.recorded())
	s.NoteDisplayBounds(111, 222, 800, 500)

	d := s.Status().Desired.Display
	if d == nil || d.X != 111 || d.Y != 222 || d.Width != 800 || d.Height != 500 {
		t.Fatalf("期望状态里的几何没被更新: %+v", d)
	}
	if after := len(fake.recorded()); after != before {
		t.Errorf("NoteDisplayBounds 不该下发命令，却多了 %d 条", after-before)
	}
}

// 用户拖动/缩放结束后，native 上报新几何：期望状态要跟着变，上层要收到回调。
//
// 期望状态这一半是重点：不更新的话，重连重放会把窗口弹回旧位置，
// 表现成「我明明拖过去了，重启又回来了」。
func TestWindowChangedEventUpdatesDesired(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)

	got := make(chan [4]int, 1)
	s.OnWindowChanged = func(x, y, w, h int) { got <- [4]int{x, y, w, h} }

	runSupervisor(t, s)
	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	fake.sendEvent(ipc.Message{
		Type: "event.windowChanged",
		Payload: mustMarshal(map[string]any{
			"x": 33, "y": 44, "width": 700, "height": 400,
		}),
	})

	select {
	case v := <-got:
		if v != [4]int{33, 44, 700, 400} {
			t.Errorf("OnWindowChanged 参数不对: %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnWindowChanged 没被调用")
	}

	d := s.Status().Desired.Display
	if d == nil || d.X != 33 || d.Y != 44 || d.Width != 700 || d.Height != 400 {
		t.Errorf("期望状态没跟着更新: %+v", d)
	}
}

// Status 快照必须可序列化且不含活对象。
func TestStatusIsPlainData(t *testing.T) {
	fake := newFakeNative()
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })

	b, err := json.Marshal(s.Status())
	if err != nil {
		t.Fatalf("Status 应可序列化: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Status 序列化结果应能解回: %v", err)
	}
	for _, key := range []string{"connected", "capabilities", "versionMatch", "desired", "actual"} {
		if _, ok := back[key]; !ok {
			t.Errorf("快照缺少字段 %s", key)
		}
	}
}

// 握手版本不匹配要如实报告。
func TestVersionMismatchReported(t *testing.T) {
	fake := newFakeNative()
	fake.version = false
	inj := &injector{shared: fake}
	s := newTestSupervisor(t, inj)
	runSupervisor(t, s)

	waitFor(t, 2*time.Second, "连接建立", func() bool { return s.Status().Connected })
	if s.Status().VersionMatch {
		t.Error("版本不匹配时 VersionMatch 应为 false")
	}
}
