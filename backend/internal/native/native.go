// Package native 是 native 能力层的 Go 侧主管（ADR-012）。
//
// 职责边界（宪法：一个职责只属于一种语言）：
//   - **期望状态**由 Go 持有：窗口规格、热键、托盘分别应该是什么样
//   - **实际状态**由 C++ 持有：窗口真的建出来了吗、多大、可见吗
//   - 本包负责让两者收敛：连接、握手、重放、心跳、断线重连
//
// 为什么不把期望状态交给 native：进程会重启（升级、崩溃、Go 主动重启），
// 一旦 native 重启，它记不住任何东西。Go 活得更久，所以由 Go 记住。
//
// 不变量：
//   - 断线后**不**清空期望状态，重连时整体重放
//   - 能力缺失时不下发对应命令，但期望状态照样记住，等能力上线自动生效
//   - 心跳连续 3 次无应答即判定失联并触发重连，不靠「写失败」这种被动信号
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/debuglog"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ipc"
)

const (
	heartbeatInterval = 1 * time.Second
	heartbeatMisses   = 3
	dialTimeout       = 5 * time.Second
	// shutdownTimeout 是「请 native 优雅退出」的等待上限。
	// 它比 dialTimeout 短：退出路径上多等几秒，用户只会觉得程序关不掉。
	shutdownTimeout = 2 * time.Second
	callTimeout       = 5 * time.Second

	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// 能力名。与 native/src/dispatch.cpp 的 capabilities() 一一对应。
const (
	CapPipe    = "pipe"
	CapJSON    = "json"
	CapWindow  = "window"
	CapInput   = "input"
	CapHotkey  = "hotkey"
	CapTray    = "tray"
	CapWebview = "webview"
)

// ── 期望状态 ────────────────────────────────────────────────

// Display 是悬浮窗的期望规格。字段名与 native 侧读取的键严格一致。
type Display struct {
	X            int     `json:"x"`
	Y            int     `json:"y"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Topmost      bool    `json:"topmost"`
	ClickThrough bool    `json:"clickThrough"`
	Opacity      float64 `json:"opacity"`
	Blur         string  `json:"blur"` // auto | mica | acrylic | none
	Title        string  `json:"title"`
	Visible      bool    `json:"visible"`

	// URL 是要在窗口里显示的页面地址。
	//
	// 由 **Go** 决定而不是 native：只有 Go 知道自己监听哪个端口、前端产物
	// 挂在哪。native 只负责把这个地址显示出来（ADR-013）。
	// 为空表示不建 WebView——那样窗口会是一块空玻璃。
	URL string `json:"url,omitempty"`

	// 设置窗口的初始位置与尺寸（V1 config.py 的 settings_win_w/settings_win_h，
	// 位置 X/Y 是 V2 补的：V1 只记尺寸）。
	//
	// 设置窗口由网页里的 window.open 触发创建，那一刻 native 没法再回头问
	// Go「该开多大、开在哪」——所以在下发悬浮窗规格时一并带过去。
	SettingsX      int `json:"settingsX"`
	SettingsY      int `json:"settingsY"`
	SettingsWidth  int `json:"settingsWidth"`
	SettingsHeight int `json:"settingsHeight"`
}

// DefaultDisplay 与 V1 overlay.py 的初始规格一致（居中等价：x/y = -1）。
func DefaultDisplay() Display {
	return Display{
		X: -1, Y: -1, Width: 620, Height: 360,
		Topmost: true, Opacity: 0.8, Blur: "auto",
		Title: "ETS2 Translator", Visible: true,
		// V1 config.py 同名默认值
		SettingsWidth: 540, SettingsHeight: 700,
	}
}

// HotkeySpec 是发给 native 的热键定义。
//
// VK 与 Mods 由 Go 侧解析好——native **不接触热键字符串**，
// 这样解析逻辑只有一处实现（缺陷 D7：V1 有四处，行为还不一致）。
type HotkeySpec struct {
	ID      string `json:"id"`
	VK      uint32 `json:"vk"`
	Mods    uint32 `json:"mods"`
	Enabled bool   `json:"enabled"`
}

// HotkeyResolver 把热键字符串解析成 VK 与 MOD 位掩码。
//
// 由 internal/hotkeys 实现。native 包不 import 它，只依赖这个形状——
// task 包也定义了同样形状的接口，两者可以互相满足。
type HotkeyResolver interface {
	Resolve(combo string) (vk uint32, mods uint32)
}

// Hotkey 是一个热键的期望状态（Go 侧持字符串形式，便于诊断与重放）。
type Hotkey struct {
	ID      string `json:"id"`
	Combo   string `json:"combo"`
	Enabled bool   `json:"enabled"`
}

// TrayMenuItem 是托盘菜单里的一项。
//
// ⚠️ 菜单内容**由 Go 生成**、native 只负责显示（矩阵 W7）：因为「该项该不该
// 打勾」（比如鼠标穿透当前是开是关）属于期望状态，只有 Go 知道。
type TrayMenuItem struct {
	// ID 是命令名（toggle / switch_mode / click_through / settings / quit）。
	// native 只把它原样报回来，点完之后做什么由 Go 决定。
	ID        string `json:"id,omitempty"`
	Label     string `json:"label,omitempty"`
	Separator bool   `json:"separator,omitempty"`
	Checked   bool   `json:"checked,omitempty"`
	// Default 为 true 表示这是默认项：托盘图标被**左键单击**时触发它。
	Default bool `json:"default,omitempty"`
}

// Tray 是托盘图标的期望状态。
type Tray struct {
	Enabled bool           `json:"enabled"`
	Tip     string         `json:"tip,omitempty"`
	Menu    []TrayMenuItem `json:"menu,omitempty"`
}

// TrayActual 是 native 回报的托盘真实情况。
//
// 托盘是**异步**创建的（图标必须在托盘线程里挂），所以 tray.set 返回成功
// 只表示「已发起」；图标到底挂上没有，看这里。
type TrayActual struct {
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
}

// Desired 是 Go 持有的全部期望状态。
type Desired struct {
	Display *Display `json:"display,omitempty"`
	Hotkeys []Hotkey `json:"hotkeys,omitempty"`
	Tray    *Tray    `json:"tray,omitempty"`
}

// ── 实际状态 ────────────────────────────────────────────────

// DisplayActual 是 native 回报的真实窗口情况。
type DisplayActual struct {
	Exists       bool    `json:"exists"`
	X            int     `json:"x"`
	Y            int     `json:"y"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Visible      bool    `json:"visible"`
	ClickThrough bool    `json:"clickThrough"`
	Blur         string  `json:"blur"`
}

// Actual 是 native 回报的全部实际状态。
type Actual struct {
	Connected bool           `json:"connected"`
	Display   *DisplayActual `json:"display,omitempty"`
	Tray      *TrayActual    `json:"tray,omitempty"`
}

// ── 状态快照（给 API / UI 用）──────────────────────────────

// Status 是主管的可序列化快照。这是唯一允许跨出本包的状态形状，
// 不含任何活的 Cordis/连接对象。
type Status struct {
	Connected    bool     `json:"connected"`
	Capabilities []string `json:"capabilities"`
	VersionMatch bool     `json:"versionMatch"`
	Desired      Desired  `json:"desired"`
	Actual       Actual   `json:"actual"`
	// LastError 是最近一次错误；重连成功后清空。
	LastError string `json:"lastError,omitempty"`
	// LastDisconnect 是最近一次会话中断的原因，会一直保留到下次中断为止。
	// 与 LastError 分开是因为「我的悬浮窗为什么消失了」必须能被回答，
	// 而这个答案不该被随后一次成功的重连抹掉。
	LastDisconnect string `json:"lastDisconnect,omitempty"`
	Reconnects     int    `json:"reconnects"`
}

// ── 依赖注入点 ──────────────────────────────────────────────

// conn 是 ipc.Client 的最小子集，抽出来是为了让测试能注入假连接——
// 沙箱里命名管道客户端是被禁的，不抽这一层就没法自测。
type conn interface {
	Call(ctx context.Context, msgType string, payload any) (ipc.Message, error)
	Handshake(ctx context.Context) (capabilities []string, versionMatch bool, err error)
	Ping(ctx context.Context, seq int64) error
	Events() <-chan ipc.Message
	Close() error
}

type dialFunc func(pipeName string, timeout time.Duration) (conn, error)

// ── 错误 ────────────────────────────────────────────────────

// RemoteError 是 native 明确回报的错误（event.error 或无 ok=true）。
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("native 回报错误 [%s] %s", e.Code, e.Message)
}

var (
	// ErrNotConnected 表示当前没有可用的 native 连接。
	ErrNotConnected = errors.New("native 未连接")

	// ErrCapabilityMissing 表示 native 没报告这项能力。
	ErrCapabilityMissing = errors.New("native 缺少该能力")

	errHeartbeatLost = errors.New("心跳连续无应答，判定失联")
	errEventClosed   = errors.New("事件通道已关闭")
)

// ── 主管 ────────────────────────────────────────────────────

// Supervisor 管理 native 连接的生命周期与期望状态。
//
// 零值不可用；用 New 构造。Run 之前调用 Set* 也可以，期望状态会被记住，
// 连上之后自动生效。
type Supervisor struct {
	pipeName string
	exePath  string // 非空则 Run 时按需拉起 native 进程
	launcher func(exePath, pipeName string) (*Proc, error)

	dial     dialFunc
	resolver HotkeyResolver

	// OnHotkey 在 native 报出热键触发时调用（在 IPC 读循环的 goroutine 上）。
	// 回调里不要做耗时操作，它挡着后续事件的收取。
	OnHotkey func(id string)

	// OnTrayCommand 在用户点了托盘菜单某一项时调用（同样在 IPC 读循环上）。
	//
	// 传进来的是菜单项的 ID（toggle / switch_mode / click_through / settings /
	// quit）——**做什么由这里决定**，native 只负责显示菜单与回报点击。
	OnTrayCommand func(id string)

	// OnWindowChanged 在用户拖动/缩放窗口**结束**后调用，带新几何。
	//
	// 期望状态在这里已经同步更新过了（见 handleEvent），上层拿到的意义是
	// 「可以落盘了」——V1 对应 overlay.py:415 的 _schedule_save_position。
	OnWindowChanged func(x, y, width, height int)

	// OnSettingsGeometry 在用户移动/缩放**设置窗口**后调用，带新几何。
	//
	// 与 OnWindowChanged 分开：两个窗口的几何各自独立存储，
	// 混在一个回调里调用方还得自己分辨是谁。
	OnSettingsGeometry func(x, y, width, height int)

	mu             sync.Mutex
	desired        Desired
	client         conn
	caps           map[string]bool
	version        bool
	actual         Actual
	lastErr        error
	lastDisconnect error
	reconn         int
	launched       *Proc

	// 时间参数。生产用包级常量的默认值，测试注入短值以免每个用例都等好几秒。
	hbInterval time.Duration
	hbMisses   int
	minBackoff time.Duration
	maxBackoff time.Duration
	callWait   time.Duration
}

// callContext 返回带调用超时的 context。
func (s *Supervisor) callContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.callWait)
}

// Option 配置 Supervisor。
type Option func(*Supervisor)

// WithDialer 注入自定义拨号器（测试用）。
func WithDialer(d func(pipeName string, timeout time.Duration) (conn, error)) Option {
	return func(s *Supervisor) { s.dial = d }
}

// WithLauncher 注入自定义进程拉起方式（测试用）。
func WithLauncher(l func(exePath, pipeName string) (*Proc, error)) Option {
	return func(s *Supervisor) { s.launcher = l }
}

// WithResolver 注入热键解析器（用于把期望状态里的热键字符串转成 VK/Mods）。
func WithResolver(r HotkeyResolver) Option {
	return func(s *Supervisor) { s.resolver = r }
}

// New 构造一个主管。exePath 为空表示「只连接已存在的 native 进程」。
func New(pipeName, exePath string, opts ...Option) *Supervisor {
	if pipeName == "" {
		pipeName = ipc.DefaultPipeName
	}
	s := &Supervisor{
		pipeName: pipeName,
		exePath:  exePath,
		caps:     map[string]bool{},
		desired:  Desired{Display: ptr(DefaultDisplay())},
		dial: func(name string, timeout time.Duration) (conn, error) {
			return ipc.Dial(name, timeout)
		},
		launcher:   launchProcess,
		hbInterval: heartbeatInterval,
		hbMisses:   heartbeatMisses,
		minBackoff: initialBackoff,
		maxBackoff: maxBackoff,
		callWait:   callTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

// Proc 是一个已拉起的 native 进程。
//
// 它比裸的 stop 函数多一个 Alive()：判断「进程是不是已经死了」是重连逻辑
// 必须知道的事。只知道「拉过没有」会让崩溃后的 native 永远不再被拉起
// （见 ensureProcess 的说明）。
type Proc struct {
	stop  func()
	alive func() bool
}

// Stop 结束进程并回收。可重复调用。
func (p *Proc) Stop() {
	if p != nil && p.stop != nil {
		p.stop()
	}
}

// Alive 报告进程是否仍在运行。
func (p *Proc) Alive() bool {
	if p == nil || p.alive == nil {
		return false
	}
	return p.alive()
}

// launchProcess 拉起 native 子进程。
//
// **把 native 的输出接到本进程的 stdout/stderr**：native 会打印热键是否
// 降级、窗口材质是否生效、管道是否断开等诊断信息，丢掉它们会让排障变成盲猜。
// 用 os.Stdout 而不是管道——管道会引出「谁读、读完要不要关」的一堆问题，
// 而这里只是想让用户看得见。
//
// ⚠️ 但**打包后的 EXE 用 `-H=windowsgui` 编译，此时 os.Stdout 是无效句柄**，
// native 的全部诊断被系统直接丢弃。这不是小问题：排查「设置窗口浮不上来」
// 时我完全看不到 native 内部走了哪条分支、有没有执行抬升，只能靠读代码猜——
// 连着猜错了四次。所以改成**同时写一个固定文件**，任何一次运行都能事后回看。
func openChildLog() *os.File {
	path := filepath.Join(os.TempDir(), "ets2_translator_native.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil // 写不了就退回原来的行为，不该因此起不来
	}
	return f
}

func launchProcess(exePath, pipeName string) (*Proc, error) {
	cmd := exec.Command(exePath, "--pipe", pipeName)

	// 每次运行追加一段分隔标记：这个文件是**跨多次运行累积**的，
	// 没有分隔的话，事后完全分不清哪些行属于哪一次。
	if f := openChildLog(); f != nil {
		fmt.Fprintf(f, "\n===== native 启动 %s =====\n", time.Now().Format("2006-01-02 15:04:05"))
		cmd.Stdout = f
		cmd.Stderr = f
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("拉起 native 进程失败: %w", err)
	}

	// 用一个「已退出」的信号通道表达存活状态。
	//
	// ⚠️ Wait 必须有人调，而且**只能调一次**：
	//  · 不调的话退出的子进程会一直挂着，进程表越积越多
	//  · 调两次（比如 Stop 里再 Wait）会直接返回错误，拿不到真实退出码
	// 所以这里起一个协程专门等它，Alive 只读这个 channel，不碰 Wait。
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	var once sync.Once
	return &Proc{
		stop: func() {
			once.Do(func() {
				if cmd.Process != nil {
					// 已经退出的进程 Kill 会报错，属正常，忽略即可
					_ = cmd.Process.Kill()
				}
				<-exited // 等 Wait 协程收尾，保证 Stop 返回后进程真的没了
			})
		},
		alive: func() bool {
			select {
			case <-exited:
				return false
			default:
				return true
			}
		},
	}, nil
}

// ── 期望状态的写入端 ────────────────────────────────────────

// SetDisplay 设置窗口期望规格，并在已连接时立即下发。
func (s *Supervisor) SetDisplay(d Display) error {
	s.mu.Lock()
	s.desired.Display = ptr(d)
	s.mu.Unlock()
	return s.pushDisplay(d)
}

// SetOpacity 只改悬浮窗的窗口级 alpha，**不重建窗口**。
//
// 为什么不能复用 SetDisplay：`display.create` 对 native 意味着「先销毁再重建
// 窗口」，用户每拖一下不透明度滑块窗口都会闪一下。而这个操作要的只是一次
// `SetLayeredWindowAttributes`。
//
// 补这个方法的起因：`display.create` 只在建窗时读一次 opacity，运行期改配置
// 要重启才生效——用户在设置里拖滑块看到的是「毫无反应」，而窗口其实一直停在
// 启动那一刻的值上（实测窗口 alpha 与配置项对不上，误导排查了一大圈）。
func (s *Supervisor) SetOpacity(v float64) error {
	s.mu.Lock()
	if s.desired.Display == nil {
		d := DefaultDisplay()
		s.desired.Display = &d
	}
	s.desired.Display.Opacity = v
	s.mu.Unlock()

	return s.pushDisplayCommand("display.opacity", map[string]any{"opacity": v})
}

// SetVisible 只改可见性。
func (s *Supervisor) SetVisible(visible bool) error {
	s.mu.Lock()
	if s.desired.Display == nil {
		d := DefaultDisplay()
		s.desired.Display = &d
	}
	s.desired.Display.Visible = visible
	s.mu.Unlock()
	return s.pushDisplayCommand("display.visible", map[string]any{"visible": visible})
}

// SetClickThrough 只改鼠标穿透。
func (s *Supervisor) SetClickThrough(enabled bool) error {
	s.mu.Lock()
	if s.desired.Display == nil {
		d := DefaultDisplay()
		s.desired.Display = &d
	}
	s.desired.Display.ClickThrough = enabled
	s.mu.Unlock()
	return s.pushDisplayCommand("display.setClickThrough", map[string]any{"enabled": enabled})
}

// SetBlur 只改毛玻璃模式。
func (s *Supervisor) SetBlur(mode string) error {
	s.mu.Lock()
	if s.desired.Display == nil {
		d := DefaultDisplay()
		s.desired.Display = &d
	}
	s.desired.Display.Blur = mode
	s.mu.Unlock()
	return s.pushDisplayCommand("window.blur", map[string]any{"mode": mode})
}

// BeginMoveResize 让 native 开始一次拖动或缩放。
//
// zone 取 "caption" / "left" / "right" / "top" / "bottom" /
// "topleft" / "topright" / "bottomleft" / "bottomright"。
//
// ⚠️ 这是**瞬时动作**，不是期望状态：绝不写进 s.desired。否则重连重放
//    （ADR-012）会把窗口莫名其妙地重新拖一次。
//
// native 只记下起点就立刻回复，位移由它的主循环逐帧推进，所以这里可以
// 正常等待回复，不会被一次拖动卡住。
func (s *Supervisor) BeginMoveResize(zone string) error {
	s.mu.Lock()
	client := s.client
	hasWindow := s.caps[CapWindow]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasWindow {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapWindow)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "window.beginMoveResize", map[string]any{"zone": zone})
	if err != nil {
		return err
	}
	var r replyBody
	return decodeReply(resp, &r)
}

// NoteDisplayBounds 只更新期望状态里的窗口几何，**不下发任何命令**。
//
// ⚠️ 不能用 SetDisplay 代替：那条路会下发 display.create，而 native 的
// applyDisplay 是「先销毁再重建」——用户每次拖完窗口都会闪一下。
// 这里要的只是把「用户改过」这个事实记下来，让重连重放（ADR-012）
// 不会把窗口弹回旧位置。
func (s *Supervisor) NoteDisplayBounds(x, y, width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.desired.Display == nil {
		d := DefaultDisplay()
		s.desired.Display = &d
	}
	s.desired.Display.X = x
	s.desired.Display.Y = y
	// 尺寸为 0 说明调用方没拿到有效值，别把期望状态写坏
	if width > 0 {
		s.desired.Display.Width = width
	}
	if height > 0 {
		s.desired.Display.Height = height
	}
}

// SetTray 设置托盘期望状态并立即下发。
//
// 与 SetDisplay 一样：先写 desired 再下发，失败也不回滚——期望状态是目标，
// 重连重放时会补上（ADR-012）。
func (s *Supervisor) SetTray(t Tray) error {
	s.mu.Lock()
	s.desired.Tray = &t
	client := s.client
	hasTray := s.caps[CapTray]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasTray {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapTray)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "tray.set", t)
	if err != nil {
		return err
	}
	var r replyBody
	if err := decodeReply(resp, &r); err != nil {
		return err
	}
	if r.Tray != nil {
		s.mu.Lock()
		s.actual.Tray = r.Tray
		s.mu.Unlock()
	}
	return nil
}

// SetDark 切换窗口材质（Mica/Acrylic）的深浅。
//
// ⚠️ 这条命令 native 一直实现着，但 **Go 侧从来没有人调用它**，而且
//    `tools/scaffold.py` 里也漏了这条常量——也就是说：重新生成头文件会把它
//    整条抹掉，而在此之前它也从未被用过。后果是用户在设置里切换深浅主题时，
//    前端配色变了、窗口材质没变，出现「浅色界面配深色 Mica」的错配。
func (s *Supervisor) SetDark(dark bool) error {
	s.mu.Lock()
	client := s.client
	hasWindow := s.caps[CapWindow]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasWindow {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapWindow)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "theme.set", map[string]any{"dark": dark})
	if err != nil {
		return err
	}
	var r replyBody
	return decodeReply(resp, &r)
}

// FocusWindow 把悬浮窗带到前台并获得键盘焦点。
//
// 热键「呼出输入框」必须先做这一步：热键是**全局**的，用户按的时候正在游戏
// 里，悬浮窗既不是前台窗口、也可能被隐藏。直接让前端调 input.focus() 只在
// 「窗口已经是前台」时才有可见效果——也就是热键按下去经常等于没反应。
// V1 为此写了一整套抢焦点流程（overlay.py:809-849），这一条对应它。
func (s *Supervisor) FocusWindow() error {
	s.mu.Lock()
	client := s.client
	hasWindow := s.caps[CapWindow]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasWindow {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapWindow)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "window.focus", map[string]any{})
	if err != nil {
		return err
	}
	var r replyBody
	return decodeReply(resp, &r)
}

// OpenSettings 让 native 打开设置窗口。
//
// 托盘菜单的「Settings 设置」走这条：那条路径上没有人调用 window.open，
// 所以没有 NewWindowRequested 事件可以蹭，得由 native 自己建窗口并导航。
func (s *Supervisor) OpenSettings(url string) error {
	s.mu.Lock()
	client := s.client
	hasWindow := s.caps[CapWindow]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasWindow {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapWindow)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "settings.open", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var r replyBody
	return decodeReply(resp, &r)
}

// buildHotkeySpecs 把期望状态里的热键字符串解析成 native 要的 VK/Mods。
//
// 解析失败的会被跳过并单独列出——**不能静默丢弃**：用户配了一个
// native 认不出的组合时，唯一能提醒他的地方就是这里。
func buildHotkeySpecs(list []Hotkey, r HotkeyResolver) (specs []HotkeySpec, skipped []string) {
	for _, h := range list {
		if !h.Enabled || h.Combo == "" {
			continue
		}
		var vk, mods uint32
		if r != nil {
			vk, mods = r.Resolve(h.Combo)
		}
		if vk == 0 {
			skipped = append(skipped, h.ID)
			continue
		}
		specs = append(specs, HotkeySpec{ID: h.ID, VK: vk, Mods: mods, Enabled: true})
	}
	return specs, skipped
}

// hotkeyReply 是 hotkey.set 的回复体。
type hotkeyReply struct {
	Registered int      `json:"registered"`
	Fallback   []string `json:"fallback"`
	Warning    string   `json:"warning"`
}

// SetHotkeys 设置热键期望状态，并立即下发给 native。
//
// **降级要如实记录**：native 优先用 RegisterHotKey，被占用时才退回轮询。
// 轮询能用但会白耗 CPU（V1 一直是轮询，是 B9「闲置 CPU 2.88%」的部分来源），
// 所以走降级时必须让上层知道，而不是假装一切正常。
func (s *Supervisor) SetHotkeys(list []Hotkey) error {
	s.mu.Lock()
	s.desired.Hotkeys = list
	client := s.client
	resolver := s.resolver
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}

	specs, skipped := buildHotkeySpecs(list, resolver)
	if len(specs) == 0 {
		if len(skipped) > 0 {
			return fmt.Errorf("没有可用的热键：%v 解析不出主键", skipped)
		}
		return nil // 全部禁用，是合法状态
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, "hotkey.set", map[string]any{"list": specs})
	if err != nil {
		return err
	}
	if err := decodeReply(resp, &replyBody{}); err != nil {
		return err
	}
	var r hotkeyReply
	if err := resp.DecodePayload(&r); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(r.Fallback) > 0:
		s.lastErr = fmt.Errorf("这些热键被占用，已降级为轮询: %v", r.Fallback)
	case len(skipped) > 0:
		s.lastErr = fmt.Errorf("这些热键解析不出主键，未生效: %v", skipped)
	case r.Warning != "":
		s.lastErr = errors.New(r.Warning)
	default:
		s.lastErr = nil
	}
	return nil
}

// ── 下发 ────────────────────────────────────────────────────

func (s *Supervisor) pushDisplay(d Display) error {
	return s.pushDisplayCommand("display.create", d)
}

// pushDisplayCommand 下发一条窗口命令。
//
// 未连接或缺能力时返回错误，但**不**回滚期望状态：期望状态是目标，
// 不是「已生效的事实」，重连重放时会补上。
func (s *Supervisor) pushDisplayCommand(msgType string, payload any) error {
	s.mu.Lock()
	client := s.client
	hasWindow := s.caps[CapWindow]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasWindow {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapWindow)
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := client.Call(ctx, msgType, payload)
	if err != nil {
		return err
	}
	var r replyBody
	if err := decodeReply(resp, &r); err != nil {
		return err
	}
	if r.Display != nil {
		s.mu.Lock()
		s.actual.Display = r.Display
		s.mu.Unlock()
	}
	return nil
}

// ── 连接生命周期 ────────────────────────────────────────────

// Run 持续维持连接直到 ctx 结束。返回 nil 表示正常退出。
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := s.minBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := s.session(ctx)
		if ctx.Err() != nil {
			// 这里**不**发 shutdown：走到这儿连接已经断了（session 的
			// defer 关掉了它），发了也是空转。优雅退出由调用方在取消 ctx
			// 之前显式调 Shutdown 完成，见该函数的说明。
			s.teardown()
			return nil
		}

		s.mu.Lock()
		s.lastErr = err
		if err != nil {
			s.lastDisconnect = err
		}
		s.client = nil
		s.caps = map[string]bool{}
		s.actual = Actual{}
		s.reconn++
		s.mu.Unlock()

		// 会话非正常结束：退避后重连
		select {
		case <-ctx.Done():
			// 同上：不在这里发 shutdown（此时 s.client 已被上面的失败路径置空）
			s.teardown()
			return nil
		case <-time.After(backoff):
		}
		if backoff < s.maxBackoff {
			backoff *= 2
			if backoff > s.maxBackoff {
				backoff = s.maxBackoff
			}
		}
	}
}

// session 建立一次连接并服务到断开。返回断开原因。
func (s *Supervisor) session(ctx context.Context) error {
	if s.exePath != "" {
		if err := s.ensureProcess(); err != nil {
			return err
		}
	}

	c, err := s.dial(s.pipeName, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx2, cancel := s.callContext(ctx)
	caps, match, err := c.Handshake(ctx2)
	cancel()
	if err != nil {
		return fmt.Errorf("握手失败: %w", err)
	}

	// 先记录能力再重放：replayWith 需要据此决定要不要带 display 段。
	//
	// 注意 client 要等重放**完成之后**才置位。若提前置位，消费者会看到
	// 「已连接但期望状态尚未重放」的中间态——那一刻窗口还没建出来，
	// 而 UI 已经显示「已连接」。这是测试抖动暴露出来的真实语义缺陷。
	s.mu.Lock()
	s.caps = map[string]bool{}
	for _, name := range caps {
		s.caps[name] = true
	}
	s.version = match
	s.lastErr = nil
	s.mu.Unlock()

	// 重连后整体重放期望状态（ADR-012 的核心）
	if err := s.replayWith(c); err != nil {
		s.mu.Lock()
		s.caps = map[string]bool{}
		s.mu.Unlock()
		return fmt.Errorf("重放失败: %w", err)
	}

	// 到这里才算真正「连上」：能力已知 + 期望状态已重放
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()

	// 心跳判定失联时要显式通知本函数。
	// 注意：心跳协程里单纯 return 是**不会**取消 hbCtx 的，
	// 早先那样写导致失联后永远不重连——一个测试抓到的真 bug。
	lost := make(chan struct{})
	go func() {
		if s.heartbeat(hbCtx, c) {
			close(lost)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-lost:
			return errHeartbeatLost
		case m, ok := <-c.Events():
			if !ok {
				return errEventClosed
			}
			s.handleEvent(m)
		}
	}
}

// replayWith 把全部期望状态推给 native，并记录哪些项没被应用。
func (s *Supervisor) replayWith(c conn) error {
	s.mu.Lock()
	desired := s.desired
	resolver := s.resolver
	hasWindow := s.caps[CapWindow]
	hasHotkey := s.caps[CapHotkey]
	s.mu.Unlock()

	// 注册表里的热键要**解析成 VK/Mods** 再发——native 不认热键字符串。
	registry := struct {
		Display *Display     `json:"display,omitempty"`
		Hotkeys []HotkeySpec `json:"hotkeys,omitempty"`
		Tray    *Tray        `json:"tray,omitempty"`
	}{
		Tray: desired.Tray,
	}

	// 没窗口能力时不必发 display 段，硬发只会拿到 UNSUPPORTED
	if hasWindow {
		registry.Display = desired.Display
	}
	if hasHotkey {
		specs, _ := buildHotkeySpecs(desired.Hotkeys, resolver)
		registry.Hotkeys = specs
	}

	if registry.Display == nil && len(registry.Hotkeys) == 0 && registry.Tray == nil {
		return nil
	}

	ctx, cancel := s.callContext(context.Background())
	defer cancel()

	resp, err := c.Call(ctx, "native.replay", map[string]any{"registry": registry})
	if err != nil {
		return err
	}
	var r replyBody
	if err := decodeReply(resp, &r); err != nil {
		return err
	}
	if r.Actual != nil {
		s.mu.Lock()
		s.actual = *r.Actual
		s.mu.Unlock()
	}
	if len(r.Skipped) > 0 {
		// 如实记录而不是假装成功：这些能力 native 还没实现
		s.mu.Lock()
		s.lastErr = fmt.Errorf("native 暂未实现: %v", r.Skipped)
		s.mu.Unlock()
	}
	return nil
}

// heartbeat 每秒 ping 一次，连续 hbMisses 次无应答就判定失联。
//
// 返回值 true 表示「判定失联，调用方应立即断开重连」；false 表示 ctx 结束。
// 不依赖「写失败」这种被动信号——对端假死时写操作照样是成功的。
func (s *Supervisor) heartbeat(ctx context.Context, c conn) bool {
	t := time.NewTicker(s.hbInterval)
	defer t.Stop()

	misses := 0
	var seq int64
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}

		seq++
		pctx, cancel := context.WithTimeout(ctx, s.hbInterval*2)
		err := c.Ping(pctx, seq)
		cancel()

		if err == nil {
			misses = 0
			continue
		}
		misses++
		if misses >= s.hbMisses {
			s.mu.Lock()
			s.lastErr = fmt.Errorf("心跳失败 %d 次: %w", misses, err)
			s.mu.Unlock()
			return true
		}
	}
}

// ensureProcess 保证 native 子进程正在运行。
//
// ⚠️ 判据是「进程**还活着**」，不是「曾经拉起来过」。
//
// 早先这里是 `if s.launched != nil { return nil }`，而 s.launched 只在
// teardown（进程退出时才调）里清空。于是 native 一旦崩溃：
//   · 它的退出没有经过 teardown，s.launched 仍然非 nil
//   · 之后每次重连都认为「进程已经拉起来了」，直接返回
//   · Go 侧就一直在重试一个再也不会有人监听的管道名
// 表现出来就是：native 挂过一次以后，悬浮窗、热键、托盘永久消失，
// 日志里只有无穷无尽的「连接失败」，看不出根因是「进程没被重新拉起」。
func (s *Supervisor) ensureProcess() error {
	s.mu.Lock()
	if s.launched != nil && s.launched.Alive() {
		s.mu.Unlock()
		return nil
	}
	// 记下要收掉的旧进程：它已经死了（或从未活成），但可能还占着管道名，
	// 不收掉的话新进程起来会和它抢。
	dead := s.launched
	s.launched = nil
	s.mu.Unlock()

	if dead != nil {
		dead.Stop()
	}

	proc, err := s.launcher(s.exePath, s.pipeName)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.launched = proc
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) teardown() {
	s.mu.Lock()
	client := s.client
	stop := s.launched
	s.client = nil
	s.launched = nil
	s.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
	if stop != nil {
		stop.Stop()
	}
}

// Shutdown 请 native 自己优雅退出（协议里的 `shutdown`，dispatch.cpp:754-761）。
//
// **必须在取消 ctx / teardown / Kill 之前调**，而且不能直接杀进程。
//
// 原因是 Windows 通知区域（托盘）的图标由**外壳**持有：进程被强杀时外壳
// 收不到撤销通知，那个图标会一直留在托盘里，直到用户把鼠标划过去才消失。
// 用户看到的是「程序已经关了，托盘里还挂着一个」，点它又没反应。
// native 的 shutdown 分支会走正常退出路径撤销图标（对应 V1
// `tray_icon.py:156-162` 的 `NIM_DELETE`）。
//
// ⚠️ 这个函数踩过两次顺序坑，调用点别再挪：
//   ① 放在 main 的退出收尾里 → 那时 srv.Run(ctx) 已返回并 teardown 过连接，
//      这里看到 client == nil，一个字节都没发出去，优雅退出成了空转。
//   ② 改到 Supervisor.Run 的 ctx 结束分支里 → 一样拿不到连接：
//      Run 的失败路径在进 select 之前就把 s.client 置空了，
//      ctx 分支拿到的永远是 nil。
// 唯一可靠的位置是**取消 ctx 之前**，由调用方显式同步调用（见
// cmd/translator 里的 quit()）。
//
// 对端可能已经不在了，所以只尽力发一次、不返回错误：退出路径上再多一个
// 「关闭失败」的弹窗只会更烦人。
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	if _, err := c.Call(ctx, "shutdown", nil); err != nil {
		s.mu.Lock()
		s.lastErr = fmt.Errorf("请求 native 退出失败: %w", err)
		s.mu.Unlock()
	}
}

// ── 输入能力（发送方向）──────────────────────────────────────

// sendResult 是 input.send 的回复体。
type sendResult struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SendChatMessage 让 native 模拟按键，把一条消息发进游戏聊天。
//
// hotkeyVK / hotkeyMods 是**已解析好的**虚拟键码与 MOD_* 位掩码——
// 热键解析只在 Go 侧有一处实现（`internal/hotkeys`），native 不重复解析，
// 这样就不会重演 V1 的「四处解析器行为不一致」（缺陷 D7）。
//
// native 侧这一步会阻塞约 1.5 秒（三段等待），期间它会照常泵窗口消息，
// 所以界面不会卡住。调用方的 ctx 超时要留够。
func (s *Supervisor) SendChatMessage(ctx context.Context, text string, hotkeyVK, hotkeyMods uint32, delayMs int) error {
	s.mu.Lock()
	client := s.client
	hasInput := s.caps[CapInput]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasInput {
		return fmt.Errorf("%w: %s（native 未报告输入能力）", ErrCapabilityMissing, CapInput)
	}

	if delayMs <= 0 {
		delayMs = 500
	}

	debuglog.Logf("send_chat_message: text=%q, vk=%d mods=0x%02x delay=%d",
		text, hotkeyVK, hotkeyMods, delayMs)

	resp, err := client.Call(ctx, "input.send", map[string]any{
		"text":       text,
		"hotkeyVK":   hotkeyVK,
		"hotkeyMods": hotkeyMods,
		"delayMs":    delayMs,
	})
	if err != nil {
		debuglog.Logf("send_chat_message EXCEPTION: %v", err)
		return err
	}

	// input.send 用 TN_N2C_INPUT_RESULT 回复，正文里的 ok 才是结论。
	var r sendResult
	if err := decodeReply(resp, &replyBody{}); err != nil {
		debuglog.Logf("send_chat_message EXCEPTION: %v", err)
		return err
	}
	if err := resp.DecodePayload(&r); err != nil {
		debuglog.Logf("send_chat_message EXCEPTION: %v", err)
		return err
	}
	if !r.OK {
		debuglog.Logf("send_chat_message EXCEPTION: %s %s", r.Code, r.Message)
		return &RemoteError{Code: r.Code, Message: r.Message}
	}
	debuglog.Logf("send_chat_message: sequence complete")
	return nil
}

// CopyText 把文本放进系统剪贴板（「复制译文」用）。
func (s *Supervisor) CopyText(ctx context.Context, text string) error {
	s.mu.Lock()
	client := s.client
	hasInput := s.caps[CapInput]
	s.mu.Unlock()

	if client == nil {
		return ErrNotConnected
	}
	if !hasInput {
		return fmt.Errorf("%w: %s", ErrCapabilityMissing, CapInput)
	}

	debuglog.Logf("clipboard_set: text=%q, len=%d", text, len(text))

	resp, err := client.Call(ctx, "input.copy", map[string]any{"text": text})
	if err != nil {
		return err
	}
	var r sendResult
	if err := resp.DecodePayload(&r); err != nil {
		return err
	}
	if !r.OK {
		return &RemoteError{Code: r.Code, Message: r.Message}
	}
	return nil
}

// ── 事件 ────────────────────────────────────────────────────

// handleEvent 处理 native 主动推送的事件。
//
// 热键事件（event.hotkey）是唯一有实际业务含义的：native 检测到用户按下
// 热键后推上来，Go 再决定做什么（呼出输入栏、复制译文……）。
func (s *Supervisor) handleEvent(m ipc.Message) {
	switch m.Type {
	case "event.hotkey":
		var p struct {
			ID string `json:"id"`
		}
		if err := m.DecodePayload(&p); err != nil {
			return
		}
		if p.ID != "" && s.OnHotkey != nil {
			s.OnHotkey(p.ID)
		}

	case "event.trayCommand":
		var p struct {
			ID string `json:"id"`
		}
		if err := m.DecodePayload(&p); err != nil {
			return
		}
		if p.ID != "" && s.OnTrayCommand != nil {
			s.OnTrayCommand(p.ID)
		}

	case "event.stateChanged":
		var r replyBody
		if err := m.DecodePayload(&r); err == nil {
			if r.Actual != nil {
				s.mu.Lock()
				s.actual = *r.Actual
				s.mu.Unlock()
			} else if r.Display != nil {
				s.mu.Lock()
				s.actual.Display = r.Display
				s.mu.Unlock()
			}
		}

	case "event.windowChanged":
		var p struct {
			X      int `json:"x"`
			Y      int `json:"y"`
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		if err := m.DecodePayload(&p); err != nil {
			return
		}
		// 先把事实记进期望状态，再交给上层去落盘——顺序不能反：
		// 上层万一落盘失败，至少内存里的期望状态是对的，重连不会弹回去。
		s.NoteDisplayBounds(p.X, p.Y, p.Width, p.Height)
		if s.OnWindowChanged != nil {
			s.OnWindowChanged(p.X, p.Y, p.Width, p.Height)
		}

	case "event.settingsGeometry":
		var p struct {
			X      int `json:"x"`
			Y      int `json:"y"`
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		if err := m.DecodePayload(&p); err != nil {
			return
		}
		if s.OnSettingsGeometry != nil {
			s.OnSettingsGeometry(p.X, p.Y, p.Width, p.Height)
		}

	case "event.error":
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := m.DecodePayload(&e); err == nil {
			s.mu.Lock()
			s.lastErr = &RemoteError{Code: e.Code, Message: e.Message}
			s.mu.Unlock()
		}
	}
}

// ── 状态读取 ────────────────────────────────────────────────

// Status 返回当前快照。
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	caps := make([]string, 0, len(s.caps))
	for name := range s.caps {
		caps = append(caps, name)
	}
	st := Status{
		Connected:    s.client != nil,
		Capabilities: caps,
		VersionMatch: s.version,
		Desired:      s.desired,
		Actual:       s.actual,
		Reconnects:   s.reconn,
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	if s.lastDisconnect != nil {
		st.LastDisconnect = s.lastDisconnect.Error()
	}
	return st
}

// ── 回复解析 ────────────────────────────────────────────────

type replyBody struct {
	OK      bool           `json:"ok"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Display *DisplayActual `json:"display"`
	Tray    *TrayActual    `json:"tray"`
	Applied []string       `json:"applied"`
	Skipped []string       `json:"skipped"`
	Actual  *Actual        `json:"actualState"`
}

// decodeReply 解析 native 的回复。
//
// 两条失败路径都必须当成错误：event.error 信封，以及 ok=false 的正文。
// 只看 err 会把「native 明确说不行」当成成功，那是难查的失效。
func decodeReply(m ipc.Message, out *replyBody) error {
	if m.Type == "event.error" {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = m.DecodePayload(&e)
		return &RemoteError{Code: e.Code, Message: e.Message}
	}
	if err := m.DecodePayload(out); err != nil {
		return err
	}
	if out.Code != "" && !out.OK {
		return &RemoteError{Code: out.Code, Message: out.Message}
	}
	if out.OK {
		return nil
	}
	// 没有 ok 字段的回复（例如 pong）不在这里判断
	if len(m.Payload) == 0 {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(m.Payload, &probe); err != nil {
		return nil
	}
	if _, has := probe["ok"]; has {
		return &RemoteError{Code: out.Code, Message: out.Message}
	}
	return nil
}
