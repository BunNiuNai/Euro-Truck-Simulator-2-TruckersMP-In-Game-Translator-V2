// Package task 发送方向的编排（翻译 → 输入到游戏 → 记录）。
//
// V1 对照：
//   compose_sender.py:ComposeSender（手动发送）
//   main.py:SettingsDialog 的广告发送状态机（_ad_start / _ad_tick / _ad_stop）
//
// ⚠️ 与 V1 的**有意差异**：V1 的广告发送状态机活在设置窗口里，所以
//    关掉设置窗口发送就停了——V1 自己都在界面上写红字警告
//    「使用广告发送请不要关闭设置界面」。V2 把状态机搬到 Go 侧：
//    界面只是控制面板，关掉窗口发送照常继续。
//    这是消除一个已知限制，属于有意改进（记作 D17）。
//
// 本包不解析热键：VK/Mods 由调用方通过 HotkeyResolver 提供，
// 保持「热键解析只有一处实现」（缺陷 D7）。
package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Sender 是把一条消息送进游戏的能力（由 native.Supervisor 实现）。
//
// text 是**要发送的原文**——V1 的广告发送直接调 send_chat_message(text, "y")，
// 不经过翻译。手动发送（compose）才需要先翻译。
type Sender interface {
	SendChatMessage(ctx context.Context, text string, hotkeyVK, hotkeyMods uint32, delayMs int) error
}

// HotkeyResolver 把热键字符串解析成虚拟键码与 MOD_* 位掩码。
//
// 实现由 internal/hotkeys 提供——task 不自己解析热键字符串，
// 保持「热键解析只有一处实现」（缺陷 D7）。
type HotkeyResolver interface {
	// Resolve 返回 vk=0 表示解析失败。
	Resolve(hotkey string) (vk uint32, mods uint32)
}

// Status 是广告发送的状态快照。可直接序列化给前端。
type Status struct {
	Running      bool   `json:"running"`
	CurrentIndex int    `json:"currentIndex"` // 从 0 开始；-1 表示未启动
	RemainingSec int    `json:"remainingSec"`
	IntervalMin  int    `json:"intervalMin"`
	Total        int    `json:"total"`
	Sent         int    `json:"sent"`
	Failed       int    `json:"failed"`
	LastError    string `json:"lastError,omitempty"`
}

// Options 构造参数。
type Options struct {
	Sender   Sender
	Resolver HotkeyResolver
	// Hotkey 是「打开游戏聊天」的热键，来自 cfg.ChatHotkey。
	//
	// ⚠️ V1 的广告发送把这里硬编码成 "y"（main.py:1076），而手动发送用的是
	// cfg.chat_hotkey（compose_sender.py:96）——同一个动作两套取值。用户改了
	// 热键后手动发送能用、广告发送失效。V2 统一走配置（缺陷 D16）。
	Hotkey string
	// DelayMs 是发送序列里每步之间的等待，V1 用 500ms。
	DelayMs int
	// OnStatus / OnLog 用于把状态与日志推给前端。
	OnStatus func(Status)
	OnLog    func(level, text string)
	// Tick 是状态机的步进间隔，默认 1 秒。测试注入短值。
	Tick time.Duration
	// SendTimeout 是单次发送的超时，要留够 native 侧的三段等待。
	SendTimeout time.Duration
}

// AdSender 按固定间隔循环发送一组广告消息。
type AdSender struct {
	sender   Sender
	resolver HotkeyResolver

	onStatus func(Status)
	onLog    func(level, text string)

	tick        time.Duration
	sendTimeout time.Duration
	delayMs     int

	mu        sync.Mutex
	messages  []string
	interval  time.Duration
	hotkey    string
	running   bool
	index     int
	remaining int
	sent      int
	failed    int
	lastErr   string

	stopCh chan struct{}
	doneCh chan struct{}
}

// New 创建发送器。
func New(opts Options) *AdSender {
	tick := opts.Tick
	if tick <= 0 {
		tick = time.Second
	}
	timeout := opts.SendTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	delay := opts.DelayMs
	if delay <= 0 {
		delay = 500
	}
	return &AdSender{
		sender:      opts.Sender,
		resolver:    opts.Resolver,
		onStatus:    opts.OnStatus,
		onLog:       opts.OnLog,
		tick:        tick,
		sendTimeout: timeout,
		delayMs:     delay,
		hotkey:      opts.Hotkey,
		index:       -1,
	}
}

// Configure 更新消息列表与间隔（配置变更时调用）。
//
// 运行中改配置不重启状态机：新内容在下一次发送时生效，
// 用户改一条广告不该打断整个循环。
func (a *AdSender) Configure(messages []string, intervalMin int, hotkey string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.messages = append([]string(nil), messages...)
	if intervalMin > 0 {
		a.interval = time.Duration(intervalMin) * time.Minute
	}
	if hotkey != "" {
		a.hotkey = hotkey
	}
}

// Start 开始循环发送。已在运行时不重复启动。
func (a *AdSender) Start() error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("已在运行中")
	}
	if len(a.nonEmptyLocked()) == 0 {
		a.mu.Unlock()
		return fmt.Errorf("请至少填写一条广告消息")
	}
	if a.interval <= 0 {
		a.mu.Unlock()
		return fmt.Errorf("倒计时必须大于 0 分钟")
	}
	// 热键解析不了的话，发送序列第一步就会失败——提前拦下，
	// 而不是让它每分钟失败一次
	hotkey := a.hotkey
	if a.resolver == nil {
		a.mu.Unlock()
		return fmt.Errorf("缺少热键解析器")
	}
	if vk, _ := a.resolver.Resolve(hotkey); vk == 0 {
		a.mu.Unlock()
		return fmt.Errorf("无法解析呼出热键 %q", hotkey)
	}

	a.running = true
	a.index = -1
	a.sent = 0
	a.failed = 0
	a.lastErr = ""
	a.remaining = int(a.interval / time.Second)
	a.stopCh = make(chan struct{})
	a.doneCh = make(chan struct{})
	a.advanceLocked()

	st := a.statusLocked()
	intervalMin := int(a.interval / time.Minute)
	a.mu.Unlock()

	a.emit(st)
	a.logf("INFO", fmt.Sprintf("开始发送，间隔 %d 分钟", intervalMin))

	go a.loop()
	return nil
}

// Stop 停止循环。幂等。
func (a *AdSender) Stop() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	a.running = false
	close(a.stopCh)
	done := a.doneCh
	a.index = -1
	a.remaining = 0
	st := a.statusLocked()
	a.mu.Unlock()

	<-done // 等状态机真正退出，避免 Stop 返回后还在发
	a.emit(st)
	a.logf("INFO", "已暂停")
}

// Status 返回当前快照。
func (a *AdSender) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

// ── 内部 ────────────────────────────────────────────────────

func (a *AdSender) statusLocked() Status {
	intervalMin := 0
	if a.interval > 0 {
		intervalMin = int(a.interval / time.Minute)
	}
	return Status{
		Running:      a.running,
		CurrentIndex: a.index,
		RemainingSec: a.remaining,
		IntervalMin:  intervalMin,
		Total:        len(a.messages),
		Sent:         a.sent,
		Failed:       a.failed,
		LastError:    a.lastErr,
	}
}

func (a *AdSender) nonEmptyLocked() []string {
	out := make([]string, 0, len(a.messages))
	for _, m := range a.messages {
		if strings.TrimSpace(m) != "" {
			out = append(out, m)
		}
	}
	return out
}

// advanceLocked 跳到下一个**非空**消息。
//
// 与 V1 `_ad_advance_index` 一致：只停在有内容的槽位上，
// 否则会把空消息也当一条发出去（游戏里就是一次空回车）。
func (a *AdSender) advanceLocked() {
	n := len(a.messages)
	if n == 0 {
		a.index = -1
		return
	}
	start := a.index
	for i := 0; i < n; i++ {
		a.index = (a.index + 1) % n
		if strings.TrimSpace(a.messages[a.index]) != "" {
			return
		}
	}
	a.index = (start + 1) % n
}

func (a *AdSender) emit(st Status) {
	if a.onStatus != nil {
		a.onStatus(st)
	}
}

func (a *AdSender) logf(level, text string) {
	if a.onLog != nil {
		a.onLog(level, text)
	}
}

// loop 是状态机主体。每秒步进一次，与 V1 的 `_ad_schedule_tick` 同序：
// 先看倒计时是否归零（归零则发送并重置），再减一。
func (a *AdSender) loop() {
	defer close(a.doneCh)

	t := time.NewTicker(a.tick)
	defer t.Stop()

	// V1 是启动后立刻跑一次 tick（而不是先等一秒）
	a.step()

	for {
		select {
		case <-a.stopCh:
			return
		case <-t.C:
			a.step()
		}
	}
}

func (a *AdSender) step() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}

	if a.remaining <= 0 {
		idx := a.index
		text := ""
		if idx >= 0 && idx < len(a.messages) {
			text = strings.TrimSpace(a.messages[idx])
		}
		a.advanceLocked()
		a.remaining = int(a.interval / time.Second)
		st := a.statusLocked()
		hotkey := a.hotkey
		a.mu.Unlock()

		a.emit(st)
		if text != "" {
			// 发送放到 goroutine：一次约 1.5 秒，不该挡住状态机的秒级步进
			go a.send(idx, text, hotkey)
		}
		return
	}

	a.remaining--
	st := a.statusLocked()
	a.mu.Unlock()
	a.emit(st)
}

func (a *AdSender) send(idx int, text, hotkey string) {
	vk, mods := uint32(0), uint32(0)
	if a.resolver != nil {
		vk, mods = a.resolver.Resolve(hotkey)
	}
	if vk == 0 {
		a.recordResult(idx, text, fmt.Errorf("无法解析热键 %q", hotkey))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.sendTimeout)
	defer cancel()

	err := a.sender.SendChatMessage(ctx, text, vk, mods, a.delayMs)
	a.recordResult(idx, text, err)
}

func (a *AdSender) recordResult(idx int, text string, err error) {
	preview := text
	if len([]rune(preview)) > 30 {
		preview = string([]rune(preview)[:30]) + "..."
	}

	a.mu.Lock()
	if err != nil {
		a.failed++
		a.lastErr = err.Error()
	} else {
		a.sent++
		a.lastErr = ""
	}
	st := a.statusLocked()
	a.mu.Unlock()

	if err != nil {
		a.logf("ERROR", fmt.Sprintf("发送失败 消息%d: %v", idx+1, err))
	} else {
		a.logf("INFO", fmt.Sprintf("已发送 消息%d: %s", idx+1, preview))
	}
	a.emit(st)
}
