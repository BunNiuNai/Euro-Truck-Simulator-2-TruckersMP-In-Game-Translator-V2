// Translator V2 — Go 侧进程入口
//
// 已接通的主线：
//
//	ingest/chatlog（读 TMP 聊天日志）→ engine（跳过/词典/混合拆分/竞速）→
//	    ├ logger（翻译日志）
//	    └ api（REST + SSE 推送给前端）
//
// 运行模式：
//
//	go run ./cmd/translator -serve     # 服务模式：起 API + 监控日志（阶段 3 前端用这个）
//	go run ./cmd/translator            # 尾随模式：跳过历史、只处理新消息，跑 30 秒
//	go run ./cmd/translator -replay    # 回放模式：把当前日志从头读一遍（演示用）
//
// 硬约束：不新增 V1 没有的用户可见功能（P1）；业务逻辑全部留在 Go（N3）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/api"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/cache"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/compose"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/debuglog"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/embedded"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/engine"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/hotkeys"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ingest/chatlog"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/logger"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/native"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/providers"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/task"
)

// version 由构建期通过 -ldflags "-X main.version=..." 注入
var version = "v2.0.0-dev"

// ── LLM 适配 ──────────────────────────────────────────────────

// placeholderLLM 用于「尚未配置任何 Provider」时，让主线仍可跑通并显示流程。
type placeholderLLM struct{}

func (placeholderLLM) Call(_ context.Context, _ string) (string, string, string, error) {
	return "〔未配置 Provider〕", "STUB", "", nil
}

// providerLLM 把 providers.Client 适配成 engine.LLMCaller。
type providerLLM struct {
	c *providers.Client
	// failWhenUnconfigured 决定「一个 Provider 都没配」时的行为，两个方向**必须不同**：
	//
	//   false（接收方向）→ 返回占位文案，界面显示「〔未配置 Provider〕」。
	//     比抛错误清楚：用户一眼看出是没配，而不是「配了但挂了」。
	//
	//   true（发送方向）→ 返回错误，compose 据此给出 FAIL_TRANSLATION。
	//     **绝不能**返回占位文案：那会把「〔未配置 Provider〕」这行字
	//     真的发进游戏聊天框，在别的玩家眼里就是一句乱码，
	//     而且是发出去之后才发现的。
	failWhenUnconfigured bool
}

// errNoProvider 是「一个启用的 Provider 都没有」时的错误。
var errNoProvider = errors.New("尚未配置 Provider，请在设置页填入 API Key")

// Call 每次调用时**现算**有没有可用的 Provider，而不是沿用构造时的判断。
//
// ⚠️ 这修的是一个让程序「配好了也不能用」的缺陷：
// 原先 `llm` 是在 bootstrap 里一次性选定的接口值——启动时没有启用 Provider
// 就永远绑在 placeholderLLM 上。用户在设置页填好 Endpoint/Key/Model 点保存后，
// 热重载只调 `client.SetProviders`，**换不掉那个接口值**，于是界面照样显示
// 「未配置 Provider」，只能重启才生效。而这个程序明确做了配置热重载，
// 不该要求用户重启。
//
// 现算的代价只是一次加锁遍历（Provider 通常个位数），换来「配好即生效」。
func (p providerLLM) Call(ctx context.Context, text string) (string, string, string, error) {
	if p.c.EnabledCount() == 0 {
		if p.failWhenUnconfigured {
			return "", "", "", errNoProvider
		}
		return "〔未配置 Provider〕", "STUB", "", nil
	}
	res, err := p.c.Translate(ctx, text)
	if err != nil {
		return "", "", "", err
	}
	return res.Translated, res.Provider, res.Model, nil
}

// providerAdmin 把 providers.Client 适配成 api.ProviderTester。
type providerAdmin struct{ c *providers.Client }

func (a providerAdmin) TestConnection(p config.Provider) (bool, string) {
	res := a.c.TestConnection(context.Background(), p)
	return res.Success, res.Message
}

// FetchModels 拉取某个 Provider 的可用模型列表（设置页的 📥 按钮）。
//
// 直接透传 ctx：拉模型要打外部网络，超时由调用方（api 层）控制，
// 在这里换成 context.Background() 会让请求一直挂着不返回。
func (a providerAdmin) FetchModels(ctx context.Context, p config.Provider) providers.ModelList {
	return a.c.FetchModels(ctx, p)
}

func (a providerAdmin) Health() map[string]any {
	snap := a.c.HealthSnapshot()
	out := make(map[string]any, len(snap))
	now := time.Now()
	for label, h := range snap {
		out[label] = map[string]any{
			"failures": h.Failures,
			"cooling":  !h.CoolUntil.IsZero() && now.Before(h.CoolUntil),
		}
	}
	return out
}

// ── 辅助 ──────────────────────────────────────────────────────

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func countEnabled(ps []config.Provider) int {
	n := 0
	for _, p := range ps {
		if p.Enabled {
			n++
		}
	}
	return n
}

// ensureEmbeddedResources 把内嵌的资源解到数据目录，返回那个目录；没打包时返回 ""。
//
// 解到数据目录（`<configDir>/resources/`）而不是 exe 旁边：exe 可能放在
// Program Files 之类没有写权限的地方，而且「单 EXE」的卖点就是旁边不需要放东西。
//
// 只在内容不同时才重写。这几个文件只有几十 KB，逐字节比对也不贵，
// 所以用**内容**而不是长度——资源是会被人手工调的小文件（改一句词典、
// 调一个默认值），长度一样但内容不同的情况完全可能发生，
// 那时用长度判断就会永远不更新。
func ensureEmbeddedResources() string {
	files := embedded.Resources()
	if len(files) == 0 {
		return ""
	}

	dir := filepath.Join(config.ConfigDir(), "resources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "内嵌资源解包失败（建目录）: %v\n", err)
		return ""
	}

	for name, want := range files {
		target := filepath.Join(dir, name)
		if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, want) {
			continue
		}
		if err := os.WriteFile(target, want, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "内嵌资源解包失败（%s）: %v\n", name, err)
			continue
		}
		fmt.Printf("已从内嵌资源解出: %s\n", target)
	}
	return dir
}

// findResourcesDir 在常见位置寻找 resources 目录（开发运行与打包运行兼顾）。
func findResourcesDir() string {
	candidates := []string{
		filepath.Join(".", "resources"),
		filepath.Join("..", "resources"),
		filepath.Join("..", "..", "resources"), // 从 backend/ 运行 go run ./cmd/translator
		filepath.Join("..", "..", "..", "resources"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "resources")}, candidates...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "dictionary.json")); err == nil {
			return c
		}
	}
	return "resources"
}

// ── 统计与打印 ────────────────────────────────────────────────

type counters struct {
	total, localDict, skipTarget, skipNonText, allTarget, mixed, llm, errorCnt int
}

func (c *counters) add(state string) {
	c.total++
	switch state {
	case "local_dict":
		c.localDict++
	case "skip_target":
		c.skipTarget++
	case "skip_nontext":
		c.skipNonText++
	case "all_target":
		c.allTarget++
	case "mixed":
		c.mixed++
	case "llm":
		c.llm++
	case "error":
		c.errorCnt++
	}
}

func (c *counters) summary() {
	zeroAPI := c.localDict + c.skipTarget + c.skipNonText + c.allTarget
	fmt.Println()
	fmt.Println("=== 统计 ===")
	fmt.Printf("  总消息       %d\n", c.total)
	fmt.Printf("  本地词库     %d\n", c.localDict)
	fmt.Printf("  已是目标语言 %d\n", c.skipTarget)
	fmt.Printf("  非文字       %d\n", c.skipNonText)
	fmt.Printf("  全部目标语言 %d\n", c.allTarget)
	fmt.Printf("  混合拆分     %d\n", c.mixed)
	fmt.Printf("  需 LLM       %d\n", c.llm+c.errorCnt)
	if c.total > 0 {
		fmt.Printf("  零 API 调用率 %.1f%%\n", float64(zeroAPI)/float64(c.total)*100)
	}
}

var stateLabel = map[string]string{
	"local_dict":   "本地词库",
	"skip_target":  "已是目标语言",
	"skip_nontext": "非文字",
	"all_target":   "全部为目标语言",
	"skip_empty":   "空消息",
	"mixed":        "混合拆分",
	"llm":          "LLM",
	"error":        "翻译失败",
}

func printMessage(m domain.RenderedMessage) {
	label, ok := stateLabel[m.CacheState]
	if !ok {
		label = m.CacheState
	}
	orig := strings.ReplaceAll(m.Original, "\n", " ")
	trans := strings.ReplaceAll(m.Translated, "\n", " ")
	speaker := m.Speaker
	if m.IsSelf {
		speaker = "(You) " + speaker
	}
	fmt.Printf("[%s] %s | %s → %s  [%s]\n",
		m.Timestamp, truncate(speaker, 20), truncate(orig, 40), truncate(trans, 40), label)
}

// ── 入口 ──────────────────────────────────────────────────────

// app 把启动期构造出来的东西聚在一起，避免 main 里一堆参数传递。
type app struct {
	cfg    *config.Config
	dict   *dictionary.Dictionary
	log    *logger.Logger
	engine *engine.Translator
	// sendEngine 是**发送方向**的翻译器：目标语言与 system prompt 都与
	// engine 不同（V1 translator.py:906 的 translate_for_send）。
	sendEngine *engine.SendTranslator
	padmin     providerAdmin
	// llmClient / sendLLMClient 是接收方向与发送方向的 Provider 客户端。
	//
	// 存到 app 上是因为**热重载要用**：OnConfigChanged 必须把新的 Provider
	// 列表与目标语言推给它们。只留在 newApp 的局部变量里的话，用户加了一家
	// Provider、点保存，请求还是只发给旧的那几家——设置页显示已保存、
	// 实际没生效，这种「静默不生效」比报错难查得多。
	llmClient     *providers.Client
	sendLLMClient *providers.Client
	resDir string
	logDir string
	// keymap 是统一的热键解析表（缺陷 D7 的修复：V1 有四处解析器）。
	// native 侧不解析热键，收到的就是这里解析好的 VK/Mods。
	keymap *hotkeys.Keymap
}

// extractEmbeddedNative 把内嵌的 native 可执行文件解出来，返回它的路径。
//
// 解到**数据目录**（`<configDir>/native/`）而不是临时目录：
//   · 临时目录里的东西可能被清理工具顺手删掉，而 native 是常驻进程；
//   · 每次启动都往 temp 写一份几 MB 的 exe 纯属白费磁盘和 IO。
//
// 只在**内容**不同时才重写。
//
// ⚠️ 这里曾经是「只在长度不同时才重写」，理由是"native 每次重新构建几乎一定
// 改变长度，算哈希不划算"。**那个假设是错的，而且代价很大**：
// 一个 token 的源码改动（`pressCombo(0, …)` → `pressCombo(TN_MOD_CONTROL, …)`）
// 编译出来的 exe **字节数完全相同**。于是解包逻辑判定"已经是同一份"，
// 继续用上一次留下的旧二进制——修复明明做对了、测试也全绿，
// 用户那边却毫无变化。症状是「修复无效」，最容易被怀疑到错误的地方去。
//
// 200 KB 的读盘 + 逐字节比较在启动时完全可忽略（真要几 MB 也一样），
// 拿它换"绝不会跑到过期二进制"是划算的。
func extractEmbeddedNative(a *app) string {
	data := embedded.NativeBinary()
	if len(data) == 0 {
		return ""
	}

	dir := filepath.Join(config.ConfigDir(), "native")
	target := filepath.Join(dir, embedded.NativeBinaryName())

	if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, data) {
		return target
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.log.Warn("SYS", "内嵌 native 解包失败（建目录）: "+err.Error())
		return ""
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		a.log.Warn("SYS", "内嵌 native 解包失败（写入）: "+err.Error())
		return ""
	}
	a.log.Info("SYS", fmt.Sprintf("已从内嵌资源解出 native: %s（%d KB）",
		target, len(data)/1024))
	return target
}

// findNativeExe 定位 native 能力层可执行文件。
//
// 查找顺序（先具体后宽泛）：
//  1. 环境变量 ETS2_TRANSLATOR_NATIVE —— 显式覆盖，联调时最方便
//  2. 本可执行文件同目录下的 translator_native.exe —— 打包后的布局
//  3. 工作目录下的 dist/ —— go run / 开发时的布局
//
// 找不到返回空串：调用方据此**降级**（显示、热键、输入不可用，翻译照常），
// 而不是直接启动失败。
func findNativeExe() string {
	if v := os.Getenv("ETS2_TRANSLATOR_NATIVE"); v != "" {
		return v
	}

	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "translator_native.exe"))
	}
	candidates = append(candidates,
		filepath.Join("dist", "translator_native.exe"),
		filepath.Join("..", "dist", "translator_native.exe"),
		filepath.Join("..", "..", "dist", "translator_native.exe"),
	)

	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// desiredHotkeys 把配置里的热键整理成 native 的期望状态。
//
// **只注册 V1 设置界面里有的 3 个**（copy / enter / send）。
// config 里还有 chat_hotkey 与 paste_hotkey 两个字段，但 V1 的设置界面
// 没有给它们入口（缺陷 D7 顺带记录），所以不凭字段名替用户决定要注册什么。
//
// 注意 enter_hotkey 的语义是「发送」、send_hotkey 的语义是「呼出输入框」——
// 名字容易看反，标签以 V1 `_build_hotkeys_tab` 的为准。
// traySpec 生成托盘的期望规格。
//
// 菜单内容与文案**照抄** V1 `main.py:173-182` 的 `_setup_tray`：顺序、分隔线
// 位置、中英混排都不改——那是用户看得见的东西，P1 要求一致。
//
// 勾选状态（鼠标穿透）由 Go 决定并随每次下发刷新：native 不持有业务状态，
// 它只按这份规格把菜单画出来（矩阵 W7）。
func traySpec(cfg *config.Config) native.Tray {
	return native.Tray{
		Enabled: true,
		Tip:     "ETS2 聊天翻译器（开源）",
		Menu: []native.TrayMenuItem{
			{ID: "toggle", Label: "Show/Hide 显示/隐藏", Default: true},
			{ID: "switch_mode", Label: "Switch Mode 切换模式"},
			{ID: "click_through", Label: "Click-Through 鼠标穿透", Checked: cfg.ClickThrough},
			{Separator: true},
			{ID: "settings", Label: "Settings 设置"},
			{Separator: true},
			{ID: "quit", Label: "Quit 退出"},
		},
	}
}

// onOff 与 V1 的日志文案一致（`f"鼠标穿透: {'开' if ... else '关'}"`）。
func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

// hasModifier 判断组合键里是否含修饰键（Ctrl/Alt/Shift/Win）。
//
// 用于**拒绝**把裸键注册成全局热键，见 desiredHotkeys 的说明。
func hasModifier(combo string) bool {
	for _, part := range strings.Split(strings.ToLower(combo), "+") {
		switch strings.TrimSpace(part) {
		case "ctrl", "control", "alt", "shift", "win", "super", "cmd", "meta":
			return true
		}
	}
	return false
}

// desiredHotkeys 生成要注册的全局热键。
//
// ⚠️ **只注册呼出输入栏那一个**，与 V1 一致。
//
// V1 从头到尾只有一个全局热键（`overlay.py:773-804` 轮询 `cfg.send_hotkey`
// 呼出输入栏）；`copy_hotkey` / `enter_hotkey` 在 V1 里只被设置界面采集，
// **从未注册**。V2 曾经把三个都注册了，代价是抢走用户的系统按键：
//
//   · `send` 绑在 `cfg.EnterHotkey`（就是游戏里提交聊天的那个**裸回车**）——
//     注册成功后用户**在游戏里按回车毫无反应**，而且看不出是翻译器干的。
//   · `copy` 绑在 `cfg.CopyHotkey`（默认 ctrl+c）——那会在**所有程序**里
//     把 Ctrl+C 截走，复制粘贴全废。
//
// 再加一道保险：**不带修饰键的组合一律不注册**。这是硬性约束，不是偏好——
// `RegisterHotKey` 是**独占**而不是监听，注册成功之后那个键在任何程序里
// 都不再产生正常输入。
//
// 如果以后确实要支持「用热键发送」，热键必须由用户显式指定成一个
// 带修饰键的组合（如 ctrl+alt+s），不能用 EnterHotkey 这种语义完全不同的字段。
func desiredHotkeys(cfg *config.Config) []native.Hotkey {
	specs := []native.Hotkey{
		{ID: "focus", Combo: cfg.SendHotkey},
		// 下面两个**有意不注册**，见函数头说明。
		// 保留在列表里只是为了启动日志能看出它们被跳过了。
		{ID: "copy", Combo: ""},
		{ID: "send", Combo: ""},
	}

	out := make([]native.Hotkey, 0, len(specs))
	for _, h := range specs {
		if h.Combo == "" {
			continue // 没配 / 有意不注册 = 不启用
		}
		if !hasModifier(h.Combo) {
			// 说出来，不要静默跳过：用户配了一个"按 X 发送"却完全没反应时，
			// 日志里必须留下"为什么不生效"，否则又是一次无从查起。
			fmt.Printf("热键 %s=%q 未注册：不带修饰键的组合会被系统独占，"+
				"注册后该键在所有程序里都会失效。请改成 ctrl/alt/shift + 键的形式\n",
				h.ID, h.Combo)
			continue
		}
		h.Enabled = true
		out = append(out, h)
	}
	return out
}

// nativeOps 把 Supervisor + 键表适配成 compose.NativeOps。
//
// 放在 cmd 层而不是 internal/native：因为「用哪个热键呼出聊天框」由**配置**
// 决定，native 收到的应该已经是解析好的 VK/Mods（缺陷 D7 的统一解析器）。
//
// 持有 *app 而不是 *config.Config：配置对象在 PUT /api/config 之后会被整体
// 替换（OnConfigChanged 里 a.cfg = cfg），抓一份快照就会用上过期值。
type nativeOps struct {
	sup *native.Supervisor
	km  *hotkeys.Keymap
	app *app
}

// sendDelayMs 是模拟按键序列里每步之间的等待。
// 500ms 与 V1 input_sender.py 及 V2 广告发送器的默认值一致。
const sendDelayMs = 500

// resolveDark 把配置里的主题模式解析成「窗口材质应该是深色吗」。
//
// 前端用 prefers-color-scheme 自己解析 system，Go 必须得出同一个答案，
// 否则「跟随系统」会变成浅色界面配深色 Mica。
func resolveDark(theme string) bool {
	switch strings.ToLower(strings.TrimSpace(theme)) {
	case "light":
		return false
	case "dark":
		return true
	default:
		return config.SystemPrefersDark() // "system" 或未知值
	}
}

// posSaveDelay 是「窗口几何落盘」的去抖时长。
// 与 V1 overlay.py:420 的 `after(1000, ...)` 一致：拖动期间几何每帧都在变，
// 逐帧写配置既没必要又磨磁盘。
const posSaveDelay = 1000 * time.Millisecond

// configReloadInterval 是外部配置文件的轮询间隔。
//
// 3 秒照抄 V1 translator.py:290-303 的 mtime 轮询。轮询而不是
// ReadDirectoryChangesW/文件系统通知：配置改动是低频人工操作，
// 3 秒的延迟完全无感，而通知机制要处理重命名、编辑器临时文件、
// 网络盘等一堆边界，收益不抵复杂度。
const configReloadInterval = 3 * time.Second

// startupSubscriberWait 是启动自检等待「前端订阅上 SSE」的上限。
//
// V1 用 `after(300)` / `after(500)` 就够了，因为它的悬浮窗与翻译循环在
// 同一个进程里；V2 的诊断要跨进程送到 WebView2 里，得等 WebView 起来、
// Vue 挂载、EventSource 连上。给足 15 秒，超时就如实记一条日志——
// 悄悄不发才是最坏的结果（用户以为没问题）。
const startupSubscriberWait = 15 * time.Second

// fatal 报告启动阶段的致命错误并退出。
//
// ⚠️ 为什么不能只写 stderr：打包后的 EXE 用 `-H=windowsgui` 编译
// （双击时不弹黑框），**代价是 stdout/stderr 全部被丢弃**。启动阶段一旦失败
// （词库读不到、配置写不了、端口被占），用户看到的就是
// 「双击了没反应」——没有任何线索，只能来问「为什么打不开」。
//
// 所以这里**两样都做**：写 stderr（开发期、或从终端跑时看得到）
// 加一个系统对话框（双击时至少有个框告诉他哪一步失败了）。
// V1 也是这么做的：`main.py:1489-1491` 用 MessageBoxW 报启动异常。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)

	text, errText := syscall.UTF16PtrFromString(msg)
	title, errTitle := syscall.UTF16PtrFromString("ETS2 翻译器 V2 — 启动失败")
	if errText == nil && errTitle == nil {
		user32 := syscall.NewLazyDLL("user32.dll")
		const mbIconError = 0x00000010 // MB_ICONERROR
		// 返回值与错误都忽略：这是在退出路径上，弹不出框也不该再引出新问题。
		_, _, _ = user32.NewProc("MessageBoxW").Call(
			0,
			uintptr(unsafe.Pointer(text)),
			uintptr(unsafe.Pointer(title)),
			mbIconError,
		)
	}
	os.Exit(1)
}

func (o nativeOps) SendChat(ctx context.Context, text string) error {
	hotkey := o.app.cfg.ChatHotkey
	vk, mods := hotkeyResolver{km: o.km}.Resolve(hotkey)
	if vk == 0 {
		return fmt.Errorf("无法解析呼出热键 %q，请到设置页重新设置", hotkey)
	}
	return o.sup.SendChatMessage(ctx, text, vk, mods, sendDelayMs)
}

func (o nativeOps) SetOverlayVisible(_ context.Context, visible bool) error {
	return o.sup.SetVisible(visible)
}

// rawSender 是「原样发一段文本」的适配（广告发送用，不翻译）。
type rawSender struct{ ops nativeOps }

func (r rawSender) SendRaw(ctx context.Context, text string) error {
	return r.ops.SendChat(ctx, text)
}

// hotkeyResolver 把 hotkeys.Keymap 适配成 task.HotkeyResolver。
//
// 之所以要这个适配层：task 包不该依赖 hotkeys 包的具体类型，
// 它只要求「能解析出 VK 与 MOD 位掩码」这件事。
type hotkeyResolver struct{ km *hotkeys.Keymap }

func (r hotkeyResolver) Resolve(hotkey string) (vk uint32, mods uint32) {
	if r.km == nil {
		return 0, 0
	}
	spec := r.km.Parse(hotkey)
	return spec.VK, spec.Mods
}

// parseAdInterval 解析广告倒计时的配置值（分钟）。
//
// 与 api.parseCountdownMinutes 同样的容错：配置里是字符串，解析不了就回落到
// V1 的默认值 5 分钟，而不是让 0 分钟流进状态机（那会导致秒发）。
func parseAdInterval(raw string) int {
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 5
		}
		n = n*10 + int(c-'0')
		if n > 1440 {
			return 1440
		}
	}
	if n <= 0 {
		return 5
	}
	return n
}

func main() {
	showVersion := flag.Bool("version", false, "打印版本后退出")
	replay := flag.Bool("replay", false, "回放当前日志（演示用，非 V1 行为）")
	serve := flag.Bool("serve", false, "服务模式：启动 REST + SSE 并持续监控日志（**不带参数时默认就是这个**）")
	tail := flag.Bool("tail", false, "命令行演示：把当前日志读一遍打到 stdout（调试用，不出界面）")
	addr := flag.String("addr", "127.0.0.1:8791", "服务模式监听地址")
	frontend := flag.String("frontend", "", "前端构建产物目录（可选，提供静态文件）")
	nativeExe := flag.String("native", findNativeExe(), "native 能力层可执行文件（空=不启动）")
	flag.Parse()

	// 判断 -native 到底是「用户显式给的」还是「默认值」。
	//
	// 单 EXE 打包后内嵌的 native 应当优先于「旁边碰巧存在的 exe」，
	// 但**不能**因此把 `-native=`（显式传空、表示不启动 native）也一起
	// 变成「那就用内嵌的吧」——那会让联调时想单独跑 Go 都做不到。
	nativeExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "native" {
			nativeExplicit = true
		}
	})

	if *showVersion {
		fmt.Printf("ets2-translator (Go) %s\n", version)
		return
	}

	a := bootstrap()
	defer a.log.Close()

	if !nativeExplicit {
		if p := extractEmbeddedNative(a); p != "" {
			*nativeExe = p
		}
	}

	// ⚠️ 默认（不带任何模式参数）必须是**服务模式**。
	//
	// 这一条是「双击 exe 就能用」的全部关键。此前不带参数跑的是 runTail——
	// 那是命令行演示模式：把聊天日志从头读一遍打到 stdout，既不建窗口也不起
	// HTTP 服务。于是普通用户双击 exe 只看到一个黑框在滚字，没有任何界面。
	//
	// 而双击恰恰是普通用户**唯一**的启动方式：他们不知道也不该知道
	// `-serve` 这种东西。V1 双击就是直接出悬浮窗（main.py 的默认路径）。
	//
	// 演示模式改为显式 `-tail`：它现在是个调试工具，不该再占据默认位置。
	if *serve || (!*replay && !*tail) {
		a.runServe(*addr, *frontend, *nativeExe)
		return
	}
	if *replay {
		runReplay(a)
		return
	}
	runTail(a)
}

// bootstrap 完成全部初始化并打印启动信息。
func bootstrap() *app {
	fmt.Printf("ets2-translator %s\n", version)

	resDir := findResourcesDir()
	// 外部目录里没有词库 → 说明这是一次「单 EXE」运行（旁边没放 resources/），
	// 把内嵌的那份解到数据目录再用。
	//
	// 必须放在这里、而不是 findResourcesDir 内部：开发期源码树里的
	// resources/ 才是事实源，不该被解出来的副本抢在前面。
	if _, err := os.Stat(filepath.Join(resDir, "dictionary.json")); err != nil {
		if p := ensureEmbeddedResources(); p != "" {
			resDir = p
		}
	}

	// 1) 词库
	dict, err := dictionary.Load(filepath.Join(resDir, "dictionary.json"))
	if err != nil {
		fatal("加载词库失败: %v\n\n搜索过的目录: %s", err, resDir)
	}
	from, at := dict.Meta()
	slang, phrases, structured, system, ets2 := dict.Counts()
	fmt.Printf("词库: %s (%s) | 俚语=%d 短语=%d 结构化=%d 系统=%d ETS2=%d\n",
		from, at, slang, phrases, structured, system, ets2)

	// 2) 配置（V2 独立目录，不读 V1 配置；密钥用 DPAPI 加密）
	cfg, cfgWarnings, err := config.Load(filepath.Join(resDir, "config.json"))
	if err != nil {
		fatal("加载配置失败: %v", err)
	}
	for _, w := range cfgWarnings {
		fmt.Printf("[配置警告] %s\n", w)
	}
	fmt.Printf("配置: %s\n", config.ConfigPath())
	fmt.Printf("目标语言: %s | Provider: %d 个（启用 %d）\n",
		cfg.TargetLanguage, len(cfg.LLMProviders), countEnabled(cfg.LLMProviders))

	// 调试日志开关（对应 V1 main.py:56-57 的 `set_debug_enabled(cfg.debug_log)`）。
	//
	// 这是 cfg.debug_log 的**唯一**消费者。此前它是个死配置：设置页能改、
	// config.json 里有、但没有任何代码读它——用户勾上之后什么都没发生，
	// 然后去找 %TEMP%\ets2_translator_debug.log 也找不到，只能以为功能坏了。
	debuglog.SetEnabled(cfg.DebugLog)
	if cfg.DebugLog {
		fmt.Printf("调试日志: 已开启 → %s\n", debuglog.Path())
	}

	// 3) 日志（V1 格式：时间 - 厂商-模型名 - 原文 - 译文）
	log := logger.New(config.LogDir())
	log.Info("SYS", fmt.Sprintf("翻译器启动 | %s | 配置: %s", version, config.ConfigPath()))
	fmt.Printf("日志目录: %s\n", log.Dir())

	// 4) 热键解析（用真实配置验证统一解析器，D7）
	km, err := hotkeys.LoadKeymap(filepath.Join(resDir, "keymap.json"))
	if err != nil {
		fatal("加载键表失败: %v", err)
	}
	for _, hk := range []struct{ name, val string }{
		{"chat", cfg.ChatHotkey}, {"copy", cfg.CopyHotkey}, {"paste", cfg.PasteHotkey},
		{"enter", cfg.EnterHotkey}, {"send", cfg.SendHotkey},
	} {
		spec := km.Parse(hk.val)
		mark := "OK"
		if !spec.Valid() {
			mark = "解析失败（热键将不生效）"
		}
		fmt.Printf("热键 %-6s %-10s → mods=0x%02X vk=%-4d %s\n",
			hk.name, hk.val, spec.Mods, spec.VK, mark)
	}

	// 5) 数据源目录：注册表读「文档」+ %VAR% 展开
	logDir := filepath.Join(config.DocumentsPath(), "ETS2MP", "logs")
	fmt.Printf("日志目录(TMP): %s\n", logDir)

	// 6) Provider 客户端（并行竞速 + 熔断）
	client := providers.New(providers.Options{
		Providers: cfg.LLMProviders,
		Target:    cfg.TargetLanguage,
		SystemPrompt: func(target string) string {
			return engine.ReceiveSystemPrompt(target, dict.PromptMapping())
		},
		Logf: func(level, module, msg string) {
			switch level {
			case "WARN":
				log.Warn(module, msg)
			default:
				log.Info(module, msg)
			}
		},
	})

	// 7) 引擎：**一律**用 providerLLM，不在这里按「现在有没有 Provider」二选一。
	//
	// ⚠️ 二选一是错的，而且症状很气人：那样选出来的接口值之后再也不会变，
	// 启动时没配就永远绑在占位桩上；用户在设置页填好 Endpoint/Key/Model
	// 点保存后，热重载只调 client.SetProviders，**换不掉这个接口值**——
	// 于是配好了照样显示「未配置 Provider」，只能重启。
	// providerLLM.Call 会每次现算（见那里的说明）。
	enabled := countEnabled(cfg.LLMProviders)
	llm := providerLLM{c: client}
	if enabled > 0 {
		fmt.Printf("LLM: 已接入 %d 个启用的 Provider（并行竞速）\n", enabled)
	} else {
		fmt.Println("LLM: 尚未配置 Provider。在设置页填好 Endpoint/Key/Model 后**立即生效**，不需要重启")
	}

	tr := &engine.Translator{
		Target:         cfg.TargetLanguage,
		BatchSeparator: engine.DefaultBatchSeparator,
		Dict:           dict,
		LLM:            llm,
		// 翻译结果缓存：V1 translator.py 的 self._cache（LRU 1000）。
		// 没有它，重复出现的聊天消息会一直白调 API，cached 统计也永远是 0。
		Cache: cache.NewLRU(engine.CacheSize),
		// 同文本并发合并：V1 translator.py:612-627 的 _in_flight / _in_flight_results。
		//
		// ⚠️ 它与上面的 Cache 是**两回事**，不能互相替代：
		//   Cache    = 「这句以前翻过」——跨时间
		//   InFlight = 「这句此刻正在翻」——跨并发，等待者复用别人的结果
		// 少了它，多人同时刷同一句话时每个请求都会真的打一次 API。
		InFlight: cache.NewInFlight(300),
	}
	// 批量翻译：V1 translator.py:354-363 的 0.3 秒聚集窗口 + 满 8 条合并。
	//
	// 必须在 tr 建好之后再挂（Batcher 要持有 tr 调 preTranslate / flush）。
	tr.Batch = engine.NewBatcher(tr, 0, 0)
	// 每 50 条消息记一次统计（V1 translator.py:371-377，`_msg_since_log`）。
	tr.Batch.SetStatsLogger(func(st domain.Stats) {
		log.Info("LLM", fmt.Sprintf("翻译统计: 翻译=%d 缓存=%d 跳过=%d 节省=%v%%",
			st.Translated, st.Cached, st.SelfSkipped, st.SavingsPct()))
	})

	// 7b) 发送方向：**另一套**目标语言与 system prompt。
	//
	// 分开建而不是复用上面那个 client，是照抄 V1 的结构：`translate_for_send`
	// 自己遍历 Provider、用自己的目标语言与 prompt（translator.py:906-927），
	// 与接收方向的竞速互不干扰。
	//
	// ⚠️ 这里的 providerLLM 带 failWhenUnconfigured：没有启用 Provider 时
	//    返回**错误**（compose 据此给 FAIL_TRANSLATION），而不是占位文案——
	//    否则「〔未配置 Provider〕」这行字会被真的发进游戏聊天框。
	//
	//    同时它也不再按启动时的 enabled 二选一：否则用户在设置页配好之后，
	//    发送方向仍然是 nil LLM，照样报 FAIL_TRANSLATION（"配好了还是发不出去"）。
	var sendLLM engine.LLMCaller
	// 与接收方向的 client 一样要在外层声明：它要存进 app 供热重载使用。
	var sendClient *providers.Client
	sendTarget := strings.TrimSpace(cfg.SendTargetLanguage)
	if sendTarget == "" {
		sendTarget = "en"
	}
	{
		sendClient = providers.New(providers.Options{
			Providers:    cfg.LLMProviders,
			Target:       sendTarget,
			SystemPrompt: func(target string) string { return engine.SendSystemPrompt(target) },
			Logf: func(level, module, msg string) {
				switch level {
				case "WARN":
					log.Warn(module, msg)
				default:
					log.Info(module, msg)
				}
			},
		})
		sendLLM = providerLLM{c: sendClient, failWhenUnconfigured: true}
	}
	sendTr := &engine.SendTranslator{Target: sendTarget, LLM: sendLLM}

	return &app{
		cfg: cfg, dict: dict, log: log, engine: tr, sendEngine: sendTr,
		padmin:        providerAdmin{c: client},
		llmClient:     client,
		sendLLMClient: sendClient,
		resDir: resDir, logDir: logDir,
		keymap: km,
	}
}

// runServe 服务模式：API + SSE + 日志监控，直到收到 Ctrl+C。
func (a *app) runServe(addr, frontendDir, nativeExe string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── native 能力层（ADR-012）──
	//
	// 拿不到 native 不影响翻译链路：窗口、热键、输入这些只是显示与发送能力。
	// 所以这里不 os.Exit，而是让它自己重连，并照常启动其余部分——
	// 用户至少还能看到翻译在跑。
	sup := native.New("", nativeExe,
		native.WithResolver(hotkeyResolver{km: a.keymap}),
	)
	go func() {
		if err := sup.Run(ctx); err != nil {
			a.log.Warn("NATIVE", "native 主管退出: "+err.Error())
		}
	}()
	if nativeExe == "" {
		fmt.Println("native: 未指定可执行文件（-native），显示/热键/输入能力不可用")
	} else {
		fmt.Printf("native: %s\n", nativeExe)
	}

	// quit 是**唯一**的退出入口。
	//
	// 顺序不能反，而且必须先请 native 优雅退出、再取消 ctx：
	//   · cancel() 之后 Supervisor 会立刻断开与 native 的连接
	//   · 那时再发 shutdown 就没有收件人了，native 会被后面的 Kill 强杀
	//   · 强杀的进程不会撤销托盘图标，Windows 外壳会把鬼影留在通知区域，
	//     用户看到「程序关了、托盘里还挂着一个」，点它又没反应
	//     （V1 `tray_icon.py:156-162` 的 NIM_DELETE 就是干这个的）
	//
	// 用 sync.Once 是因为它有三个调用方（托盘 Quit、右键菜单 Exit、Ctrl+C），
	// 用户完全可能连按两次，重复执行会二次 cancel 并向已关闭的连接发请求。
	var quitOnce sync.Once
	quit := func() {
		quitOnce.Do(func() {
			sup.Shutdown(context.Background())
			cancel()
		})
	}

	// ── 广告发送状态机（Go 侧，见 internal/task 的文件头）──
	//
	// srv 必须先声明：OnStatus 回调会捕获它，而回调可能在任何时刻被触发。
	var srv *api.Server

	adSender := task.New(task.Options{
		Sender:   sup,
		Resolver: hotkeyResolver{km: a.keymap},
		Hotkey:   a.cfg.ChatHotkey,
		DelayMs:  500,
		OnStatus: func(st task.Status) {
			if srv != nil {
				srv.Publish("ad.status", st)
			}
		},
		OnLog: func(level, text string) {
			if level == "ERROR" {
				a.log.Error("AD", text)
			} else {
				a.log.Info("AD", text)
			}
			if srv != nil {
				srv.Publish("ad.log", map[string]any{"level": level, "text": text})
			}
		},
	})
	adSender.Configure(a.cfg.AdMessages, parseAdInterval(a.cfg.AdCountdown), a.cfg.ChatHotkey)

	// 热键触发后推给前端执行。
	//
	// 为什么不在这里直接做：呼出输入栏、复制当前译文、发送——这三个动作
	// 的依据（输入栏内容、消息列表）都在前端。Go 只负责「热键被按下了」，
	// 具体做什么由掌握界面的那一侧决定。
	sup.OnHotkey = func(id string) {
		if srv == nil {
			return
		}
		// 呼出输入框之前必须先把窗口弄到前台。
		//
		// 热键是全局的：用户按的时候正在游戏里，悬浮窗既不是前台窗口、也可能
		// 被隐藏。只让前端调 input.focus() 在那种情况下完全看不出效果——
		// 这正是 V1 要写一整套抢焦点流程（overlay.py:809-849）的原因。
		if id == "focus" || id == "send" {
			if err := sup.FocusWindow(); err != nil && !errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("HOT", "窗口置前失败: "+err.Error())
			}
		}
		srv.Publish("hotkey", map[string]any{"id": id})
	}

	// ── 托盘菜单：点了哪一项之后做什么 ──
	//
	// 与 V1 `main.py:187-208` 的五个 `_tray_*` 回调一一对应。native 只**显示**
	// 菜单并把点击报回来；决策全在这里，因为「当前是不是穿透」「窗口可不可见」
	// 属于期望状态，只有 Go 持有（ADR-012）。
	sup.OnTrayCommand = func(id string) {
		switch id {
		case "toggle":
			// 取反时读的是**实际**可见性，不是自己记的标志位：
			// 窗口还可能被别处（配置、前端）改过，自记的标志会漂。
			visible := true
			if st := sup.Status(); st.Actual.Display != nil {
				visible = st.Actual.Display.Visible
			}
			if err := sup.SetVisible(!visible); err != nil && !errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("TRAY", "切换显示失败: "+err.Error())
			}

		case "click_through":
			// ⚠️ 走 srv.UpdateConfig（锁保护），不要直接 `a.cfg.X = …`。
			//
			// 配置现在是 api 与这里**共享的同一个对象**：直接改就与
			// GET /api/config 的读、PUT 的写并发，属于无保护的数据竞争。
			//
			// 新值在锁内捕获，后续判断都用这个局部量——出了锁再读
			// `a.cfg.ClickThrough` 同样是无锁读。
			var nowOn bool
			srv.UpdateConfig(func(c *config.Config) {
				c.ClickThrough = !c.ClickThrough
				nowOn = c.ClickThrough
			})
			// 落盘与托盘菜单都取 Config() 的快照：它在 RLock 下深拷贝，
			// 而 config.Save 要序列化整个结构体（含 Providers 切片），
			// 直接传 a.cfg 会与并发的写撞成撕裂读。
			snap := srv.Config()
			if err := config.Save(snap); err != nil {
				a.log.Warn("TRAY", "保存穿透配置失败: "+err.Error())
			}
			// V1 在这里会记一条 `鼠标穿透: 开/关`（main.py:198），照抄。
			a.log.Info("SYS", "鼠标穿透: "+onOff(nowOn))
			if err := sup.SetClickThrough(nowOn); err != nil && !errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("TRAY", "应用穿透失败: "+err.Error())
			}
			// 菜单上的勾要跟着变，否则用户看到的还是旧状态
			if err := sup.SetTray(traySpec(snap)); err != nil && !errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("TRAY", "刷新托盘失败: "+err.Error())
			}

		case "switch_mode":
			// ⚠️ V1 的「切换模式」是个**死功能**：`overlay.py:_apply_mode` 的
			//    文档字符串写着 "always borderless overlay"，它无条件把窗口设成
			//    无边框置顶，压根不读 window_mode。所以 V1 点这一项除了改配置值
			//    什么都不会发生。V2 保持同样行为（已记入缺陷表 D20），
			//    不凭空造一个 V1 没有的独立窗口模式——那会违反 P1。
			// 同样走锁保护入口，理由见上面 click_through 那段
			var mode string
			srv.UpdateConfig(func(c *config.Config) {
				if c.WindowMode == "overlay" {
					c.WindowMode = "standalone"
				} else {
					c.WindowMode = "overlay"
				}
				mode = c.WindowMode
			})
			if err := config.Save(srv.Config()); err != nil {
				a.log.Warn("TRAY", "保存窗口模式失败: "+err.Error())
			}
			a.log.Info("SYS", "窗口模式切换: "+mode)

		case "settings":
			// 走 settings.open 而不是让前端 window.open：托盘点击不是用户手势，
			// 浏览器会把它当弹窗拦掉（详见 native 那边的注释）。
			if err := sup.OpenSettings(fmt.Sprintf("http://%s/?page=settings", addr)); err != nil &&
				!errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("TRAY", "打开设置窗口失败: "+err.Error())
			}

		case "quit":
			a.log.Info("SYS", "托盘菜单退出")
			quit()
		}
	}

	// ── 窗口位置记忆（V1 `overlay.py:401-424`）──
	//
	// 拖动/缩放结束 → native 上报新几何 → 去抖 1000ms 落盘。
	//
	// 去抖时长与 V1 overlay.py:420 的 `after(1000, ...)` 一致：拖动期间几何
	// 每帧都在变，逐帧写配置既没必要又磨磁盘。
	//
	// posTimer 只在 IPC 读循环那一个 goroutine 上被读写（两个回调都由它调用），
	// 所以不需要额外加锁。
	// saveWindowPos 把悬浮窗几何落盘。
	//
	// 去抖计时器与**退出收尾**共用同一份实现。退出时也必须调一次：
	// 用户拖完窗口、1 秒内就退出的话计时器还没到点（posSaveDelay），
	// 不补这一次，最后那次拖动就白拖了——下次启动窗口回到旧位置。
	// V1 在 `main.py:_shutdown` 里显式调 `_save_position` 就是为了这个。
	saveWindowPos := func() {
		// 几何的权威来源是 Supervisor 的期望状态（它已经在自己的锁下
		// 更新过了），不是回调参数——避免两处各记一份。
		st := sup.Status()
		if st.Desired.Display == nil {
			return
		}
		d := st.Desired.Display
		// 锁保护写入（配置是与 api 共享的同一个对象）
		srv.UpdateConfig(func(c *config.Config) {
			c.WinX, c.WinY = d.X, d.Y
			c.WinW, c.WinH = d.Width, d.Height
		})
		if err := config.Save(srv.Config()); err != nil {
			a.log.Warn("UI", "保存窗口位置失败: "+err.Error())
			return
		}
		a.log.Info("UI", fmt.Sprintf("悬浮窗位置已记忆: %d,%d %dx%d", d.X, d.Y, d.Width, d.Height))
	}

	var posTimer *time.Timer
	sup.OnWindowChanged = func(_, _, _, _ int) {
		if posTimer != nil {
			posTimer.Stop()
		}
		posTimer = time.AfterFunc(posSaveDelay, func() {
			posTimer = nil
			saveWindowPos()
		})
	}

	// 设置窗口的位置尺寸同样要记住。
	//
	// V1 只记尺寸不记位置，所以它的设置窗口每次都在同一处打开；这里把位置
	// 一并记住——用户把设置窗口拖到副屏、或者挪开挡住的悬浮窗，下次不该复位。
	//
	// 最近一次上报单独存一份并加锁：上报来自 IPC 读循环，而退出收尾在主协程，
	// 两边并发读写裸 int 是数据竞争；更糟的是退出时读到零值会把窗口位置
	// 存成 (0,0)，下次启动设置窗口跑到屏幕左上角。
	var (
		setPosMu    sync.Mutex
		setPosTimer *time.Timer
		setSeen     bool
		setX, setY  int
		setW, setH  int
	)

	saveSettingsPos := func() {
		setPosMu.Lock()
		seen, x, y, w, h := setSeen, setX, setY, setW, setH
		setPosMu.Unlock()
		if !seen {
			return
		}
		srv.UpdateConfig(func(c *config.Config) {
			c.SettingsWinX, c.SettingsWinY = x, y
			if w > 0 {
				c.SettingsWinW = w
			}
			if h > 0 {
				c.SettingsWinH = h
			}
		})
		if err := config.Save(srv.Config()); err != nil {
			a.log.Warn("UI", "保存设置窗口位置失败: "+err.Error())
			return
		}
		a.log.Info("UI", fmt.Sprintf("设置窗口位置已记忆: %d,%d %dx%d", x, y, w, h))
	}

	sup.OnSettingsGeometry = func(x, y, w, h int) {
		setPosMu.Lock()
		defer setPosMu.Unlock()

		setSeen, setX, setY, setW, setH = true, x, y, w, h
		if setPosTimer != nil {
			setPosTimer.Stop()
		}
		// 回调里也要拿同一把锁：它读 setX 等变量，与本函数并发。
		setPosTimer = time.AfterFunc(posSaveDelay, saveSettingsPos)
	}

	// ── 手动发送链路（V1 `compose_sender.py` + `overlay.py:866-960`）──
	//
	// 把四样东西接起来：发送方向的翻译、native 的按键模拟与窗口可见性、
	// 以及读聊天日志做的送达确认。整条编排在 internal/compose 里。
	ops := nativeOps{sup: sup, km: a.keymap, app: a}
	confirmer := &compose.ChatLogConfirmer{
		Dir: a.logDir,
		// 每次调用重新取玩家名：它能在设置页里改。
		SelfName: func() string { return a.cfg.PlayerName },
	}
	composer := compose.New(compose.Options{
		Translate: func(text string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return a.sendEngine.Translate(ctx, text)
		},
		Native: ops,
		Confirm: func(text string) bool {
			return confirmer.Wait(text, compose.DefaultTimeout)
		},
		Log: a.log,
	})

	// 先把期望状态记下来。
	// 此刻多半还没连上 native，会返回 ErrNotConnected——那是正常的：
	// Supervisor 已经把期望状态存下了，连上之后 native.replay 会带上它（ADR-012）。
	if err := sup.SetHotkeys(desiredHotkeys(a.cfg)); err != nil && !errors.Is(err, native.ErrNotConnected) {
		a.log.Warn("HOT", "下发热键失败: "+err.Error())
	}

	// 托盘：同样是先把期望状态记下来。此刻多半还没连上 native，
	// 返回 ErrNotConnected 属正常——连上后 native.replay 会带上它。
	if err := sup.SetTray(traySpec(a.cfg)); err != nil && !errors.Is(err, native.ErrNotConnected) {
		a.log.Warn("TRAY", "下发托盘失败: "+err.Error())
	}

	// 窗口材质的深浅也要按配置来，而不是吃 native 的默认值 true。
	if err := sup.SetDark(resolveDark(a.cfg.Theme)); err != nil && !errors.Is(err, native.ErrNotConnected) {
		a.log.Warn("UI", "下发窗口材质深浅失败: "+err.Error())
	}

	// 显示层的期望状态：窗口规格 + 页面地址。
	//
	// 页面地址由这里决定——只有 Go 知道自己监听哪个端口、前端产物挂在哪。
	// native 拿到 url 才会建 WebView（ADR-013）。
	//
	// ⚠️ V1 启动就显示窗口（悬浮窗是它的主界面），这里保持一致。
	disp := native.DefaultDisplay()
	disp.Title = "ETS2 Translator"

	// 谁能提供页面：显式 `-frontend` 目录，**或**打包时内嵌的那一份。
	//
	// ⚠️ 只判 `frontendDir != ""` 是错的，而且错得很隐蔽：单 EXE 双击运行时
	// 不带 -frontend，但内嵌前端在，HTTP 层照样服务得了页面。漏判的后果不是
	// 少打一条日志，而是 disp.URL 留空 → native **不建 WebView** →
	// 用户看到一个**空白窗口**，同时日志还写着「显示层将没有内容可显示」。
	// 两个现象互相印证，排障的人会去查前端产物，而真正的问题是这里少了一个条件。
	if frontendDir != "" || embedded.FrontendFS() != nil {
		disp.URL = fmt.Sprintf("http://%s/?page=overlay", addr)
	} else {
		// 真的什么都没有：空 url 让 native 不建 WebView，只留窗口本身——
		// 比显示一个 404 页面好。这是开发期跑 go run 且没构建前端的情形。
		a.log.Warn("UI", "既没有 -frontend 目录也没有内嵌前端，显示层将没有内容可显示")
	}
	// 配置里的窗口规格覆盖默认值
	disp.X, disp.Y = a.cfg.WinX, a.cfg.WinY
	if a.cfg.WinW > 0 {
		disp.Width = a.cfg.WinW
	}
	if a.cfg.WinH > 0 {
		disp.Height = a.cfg.WinH
	}
	// ⚠️ 这里**故意不把 window_opacity 交给 native**，固定发 1.0。
	//
	// ⚠️ 这一段的旧结论是错的，而且代码与注释互相矛盾：
	// 注释写着「统一由前端承担（--window-alpha 直接取 window_opacity）」，
	// 但**前端从来没有用过那个变量**——整个前端只有各元素自己的 opacity 规则，
	// 没有一处读 window_opacity。于是 native 被写死 1.0、CSS 也没做，
	// `window_opacity` 这个设置**完全不生效**：悬浮窗是一块不透明的板子，
	// 用户完全看不到身后的游戏，外观页那个滑块调了也没反应。
	//
	// 现在按 V1 的做法来：**native 的窗口级 alpha = cfg.WindowOpacity**。
	// V1 用的就是 tkinter 的 `-alpha`（整个窗口一起透），用户调的就是这个语义。
	// 前端不需要再叠一层——它本来就没叠，所以不存在"双重透明"。
	opacity := a.cfg.WindowOpacity
	if opacity <= 0 || opacity > 1 {
		// 0 会让窗口彻底看不见（用户会以为程序没启动），
		// 配置被改坏时回落到 V1 的默认值，而不是把一个全透明的窗口交出去。
		opacity = 0.8
	}
	disp.Opacity = opacity
	disp.ClickThrough = a.cfg.ClickThrough
	disp.Visible = true

	// 设置窗口的位置尺寸也要下发：它是网页里 window.open 触发的，
	// 那一刻 native 没法再回头问 Go 该开多大、开在哪。
	//
	// -1 表示「没记过位置」，native 会自己居中。老配置缺这两个字段时，
	// config.Load 会从模板补上 -1，所以直接透传即可。
	disp.SettingsX, disp.SettingsY = a.cfg.SettingsWinX, a.cfg.SettingsWinY
	if a.cfg.SettingsWinW > 0 {
		disp.SettingsWidth = a.cfg.SettingsWinW
	}
	if a.cfg.SettingsWinH > 0 {
		disp.SettingsHeight = a.cfg.SettingsWinH
	}

	if err := sup.SetDisplay(disp); err != nil && !errors.Is(err, native.ErrNotConnected) {
		a.log.Warn("UI", "下发窗口失败: "+err.Error())
	}

	var err error
	srv, err = api.New(api.Options{
		Config:        a.cfg, // 共享同一个配置对象，避免两份副本互相覆盖
		Addr:          addr,
		ConfigPath:    filepath.Join(a.resDir, "config.json"),
		PresetsPath:   filepath.Join(a.resDir, "providers.json"),
		Log:           a.log,
		Translator:    a.engine,
		Providers:     a.padmin,
		Ad:            adSender,
		Clipboard:     sup,
		WindowMove:    sup,
		ManualSend:    composer,
		RawSend:       rawSender{ops: ops},
		// 悬浮窗右键菜单的「Exit」走这里，与托盘菜单的「Quit」是同一条退出路径
		// （V1 里两者也都最终调到 main.py:_shutdown）。
		OnQuit: quit,
		// 健康检查里的 nativeAlive 要如实反映连接状态——它以前是硬编码 false。
		NativeAlive: func() bool { return sup.Status().Connected },
		FrontendDir:   frontendDir,
		// 单 EXE 打包时前端是内嵌的。**只有**没给 -frontend 时才用它：
		// 开发期用 `-frontend ..\frontend\dist` 指向刚构建的产物，
		// 那时内嵌的那份多半是上一次打包留下的旧版本，优先它只会让人
		// 改了半天前端却看不到变化。
		FrontendFS: embedded.FrontendFS(),
		OnConfigChanged: func(cfg *config.Config) {
			// 热应用：目标语言与 Provider 列表同步给引擎与客户端
			a.engine.SetTarget(cfg.TargetLanguage)
			a.cfg = cfg

			// 调试日志开关也要热应用：用户勾上之后要能立刻看到
			// %TEMP% 里出现文件，否则他只能重启一次才知道生效没有
			debuglog.SetEnabled(cfg.DebugLog)

			// 配置一变就清空翻译缓存。
			//
			// ⚠️ 这一条是实测出来的，不清会让「配好 Provider 也没用」：
			// 没配 Provider 时 providerLLM 返回的是占位文案「〔未配置 Provider〕」，
			// 它**不报错**，于是被当成正常译文写进了缓存（缓存 key 是原文）。
			// 用户在设置页配好 Key 之后再看到同一条消息，走的是缓存命中——
			// 根本到不了 LLM，界面上照样显示「未配置 Provider」，
			// 而缓存要 1000 条才轮转，他只会认为「配了还是不行」。
			//
			// 顺带也覆盖另外两种情况：换目标语言、换 Provider 之后，
			// 旧译文本来就不该继续复用。
			a.engine.ClearCache()

			// ⚠️ 只 SetTarget 是不够的。Provider 列表（含每家自己的 timeout）
			// 必须同步推给**两个**客户端，否则用户加了一家 Provider、点了保存，
			// 请求还是只发给旧的那几家：设置页显示「已保存」，实际没生效，
			// 而且没有任何报错。这类静默不生效比直接报错难查得多。
			if a.llmClient != nil {
				a.llmClient.SetProviders(cfg.LLMProviders)
				a.llmClient.SetTarget(cfg.TargetLanguage)
			}
			// 发送方向是**另一套**目标语言（V1 translator.py:906-927）：
			// 空值回落到 "en"，与构造时同一套兜底。
			sendTarget := strings.TrimSpace(cfg.SendTargetLanguage)
			if sendTarget == "" {
				sendTarget = "en"
			}
			if a.sendLLMClient != nil {
				a.sendLLMClient.SetProviders(cfg.LLMProviders)
				a.sendLLMClient.SetTarget(sendTarget)
			}
			a.sendEngine.SetTarget(sendTarget)

			// 广告设置也要跟着走，否则用户改了消息得重启才生效
			adSender.Configure(cfg.AdMessages, parseAdInterval(cfg.AdCountdown), cfg.ChatHotkey)
			// 热键同理。失败也不回滚期望状态——它会在下次重连时补上。
			if err := sup.SetHotkeys(desiredHotkeys(cfg)); err != nil &&
				!errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("HOT", "更新热键失败: "+err.Error())
			}
			// 不透明度也要热应用。
			//
			// `display.create` 只在建窗时读一次 opacity，所以以前拖完滑块
			// 必须重启才生效——用户看到的是「调了毫无反应」。这里下发的
			// display.opacity 只改窗口级 alpha，不重建窗口，所以不闪。
			if err := sup.SetOpacity(cfg.WindowOpacity); err != nil &&
				!errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("UI", "应用窗口不透明度失败: "+err.Error())
			}

			// 主题变了要把窗口材质的深浅也切过去。
			//
			// 不切的话就是「浅色界面 + 深色 Mica」的错配——前端配色靠 CSS 变量
			// 立刻变，窗口材质却还停在旧主题上。
			if err := sup.SetDark(resolveDark(cfg.Theme)); err != nil &&
				!errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("UI", "切换窗口材质深浅失败: "+err.Error())
			}
		},
	})
	if err != nil {
		// 端口被占是最常见的启动失败（上一个实例没退干净、或别的程序占了
		// 8791）。这条必须弹框说出来——否则用户只看到「双击没反应」，
		// 而真实原因是「已经有一个在跑了」。
		fatal("启动 API 失败: %v\n\n如果程序已经在运行，请先退出它（或检查托盘图标）。", err)
	}

	// ── 日志定期清理（ADR-007：每 6 小时一次，与 UI 生命周期无关）──
	//
	// Logger 构造时已经清过一次；这里补上常驻的定时任务。
	// 只清一次的话，「日志保留 7 天」会退化成「启动那一刻超过 7 天的那些」，
	// 长期不重启的用户磁盘上会一直堆着过期日志。
	a.log.StartPeriodicCleanup(ctx)

	// ── 配置文件热重载 ──
	//
	// V1 translator.py:290-303 每 3 秒比一次 config.json 的 mtime，变了就重载。
	// V2 的 Watcher 早就写好了，但**一直没有调用点**——于是手改 config.json
	// 完全不生效：配置文档写着「文件是唯一事实源」，用户改完却发现要重启。
	//
	// 走 UpdateConfig + NotifyConfigChanged，与 PUT /api/config 共用同一条
	// 热应用路径。另写一份的话，将来加字段只会改到一边。
	go func() {
		w := config.NewWatcher(filepath.Join(a.resDir, "config.json"))
		t := time.NewTicker(configReloadInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fresh, changed, warnings, err := w.Poll()
				for _, warn := range warnings {
					a.log.Warn("CFG", warn)
				}
				if err != nil {
					// 解析失败**保留当前配置**：半截写坏的 JSON（编辑器保存到一半、
					// 用户手抖删了个括号）不该让正在跑的翻译全部失效。
					a.log.Warn("CFG", "配置热重载失败，保留当前配置: "+err.Error())
					continue
				}
				if !changed || fresh == nil {
					continue
				}
				// 就地写入共享对象（不能换指针，理由见 handleConfig 的说明）
				srv.UpdateConfig(func(c *config.Config) { *c = *fresh })
				srv.NotifyConfigChanged()
				a.log.Info("SYS", "配置已热重载")
			}
		}
	}()

	// ── 启动自检（V1 main.py:119-162）──
	//
	// V1 启动时往**消息列表**里插两条 System 消息：
	//   ① 文档目录 + 聊天日志状态（main.py:119-126）
	//   ② 每个启用 Provider 的连通性测试结果（main.py:128-162）
	// 它们是留在列表里可以回看的条目，不是转瞬即逝的提示条。
	//
	// 这修的正是「启动诊断只进 stdout 不进界面」：用户双击 exe 启动时
	// 根本没有终端窗口，日志全打在看不到的地方，出问题只能靠猜。
	go func() {
		if !waitForSubscriber(ctx, srv, startupSubscriberWait) {
			// 超时必须说出来。悄悄不发的话，用户看到的是「启动后界面上
			// 什么诊断都没有」，而真实原因是没人订阅——两者完全不同的
			// 排查方向，日志里必须留下是哪一种。
			if ctx.Err() == nil {
				a.log.Warn("SYS", fmt.Sprintf(
					"启动诊断未发出：%v 内没有前端订阅 SSE（消息不补发，发了也收不到）",
					startupSubscriberWait))
			}
			return
		}
		if ctx.Err() != nil {
			return
		}

		// ① 文档目录与聊天日志状态。
		// V1 的 `f"文档目录: {docs_path}\n聊天日志: {status}"`（main.py:123）
		publishSystemMessage(srv, "System",
			fmt.Sprintf("文档目录: %s\n聊天日志: %s",
				config.ConfigDir(), chatlog.LogDirStatus(a.logDir)),
			"",
		)

		// ② 判断要不要引导用户去配置，以及要不要做连通性自检。
		//
		// ⚠️ 这两件事在 V1 里是**互斥**的（main.py:1472-1485）：
		//   没配 Provider、或有 Provider 但没填 api_key → 开设置页，
		//   **不跑**连通性自检；只有配置看起来齐全时才跑自检。
		//
		// 不照抄这个互斥就会同时弹出一个设置页和一条「连通性测试失败」——
		// 用户还没开始配，就先收到一条失败消息，像是程序坏了。
		enabled := make([]config.Provider, 0, len(a.cfg.LLMProviders))
		for _, p := range a.cfg.LLMProviders {
			if p.Enabled {
				enabled = append(enabled, p)
			}
		}

		needSetup := len(enabled) == 0
		for _, p := range enabled {
			if strings.TrimSpace(p.APIKey) == "" {
				needSetup = true
				break
			}
		}

		if needSetup {
			// V1 main.py:1483 `app.overlay.root.after(500, app._open_settings)`
			//
			// 走 settings.open 而不是让前端 window.open：这里不是用户手势，
			// 浏览器会把弹窗拦掉（与托盘「设置」那一项同因）。
			if err := sup.OpenSettings(fmt.Sprintf("http://%s/?page=settings", addr)); err != nil &&
				!errors.Is(err, native.ErrNotConnected) {
				a.log.Warn("SYS", "自动打开设置页失败: "+err.Error())
			}
			a.log.Info("SYS", "尚未配置可用的 Provider，已自动打开设置页")
			return
		}

		// ③ Provider 连通性自检（V1 main.py:128-162）。
		//
		// **串行**逐个测，与 V1 main.py:140-152 一致：并发会在用户自己的
		// API 账号上同时打出 N 个请求，而这一步只是启动自检，慢几秒无所谓。
		okAll := true
		msgs := make([]string, 0, len(enabled))
		for _, p := range enabled {
			ok, msg := a.padmin.TestConnection(p)
			if !ok {
				okAll = false
			}
			msgs = append(msgs, fmt.Sprintf("%s: %s", p.Label, msg))
		}

		// 文案逐字照抄 V1 main.py:157、161
		title := "API 连通性测试"
		if !okAll {
			title = "API 连通性测试失败"
		}
		publishSystemMessage(srv, "System", title, strings.Join(msgs, "\n"))
		a.log.Info("SYS", fmt.Sprintf("%s（%d 家）", title, len(enabled)))
	}()

	// 重复声明 ctx/cancel：native 主管已经用了一个，这里给数据源单独一个
	// （两者生命周期一致，但分开更清楚谁负责取消谁）。
	srcCtx, srcCancel := context.WithCancel(context.Background())
	defer srcCancel()
	// 数据源 → 引擎 → 日志 + SSE
	src := chatlog.New(a.logDir, func() string { return a.cfg.PlayerName },
		func(name string) {
			a.log.Info("TMP", "检测到服务器: "+name)
			srv.Publish("source.status", map[string]any{"server": name})
		})
	out := make(chan domain.Event, 500)
	if err := src.Start(srcCtx, out); err != nil {
		fmt.Fprintf(os.Stderr, "启动数据源失败: %v\n", err)
		os.Exit(1)
	}
	// 新消息的两条反馈 —— V1 overlay.py:704,714,720-721：
	//   ① 每条消息都把窗口叫回来（`deiconify`）：用户隐藏了窗口，
	//      收到新消息时会重新出现。这是 V1 的行为，虽然和托盘的「隐藏」
	//      有点打架，但 P1 要求一致。
	//   ② 非自己消息按**批**计数，弹一条「翻译了 N 条消息」（绿字、3 秒）。
	//
	// ⚠️ 用固定 250ms 节拍而不是去抖计时器：V1 是 `root.after(250, poll)`
	//    的固定轮询，所以「连续来 3 条」会显示 3；去抖的话会一直往后延，
	//    一长串消息最终只弹一次 1 —— 那是另一种行为。
	var (
		noticeMu    sync.Mutex
		noticeCount int
	)
	flushNotice := func() {
		noticeMu.Lock()
		n := noticeCount
		noticeCount = 0
		noticeMu.Unlock()
		if n > 0 {
			srv.Publish("notice", map[string]any{
				"text":  fmt.Sprintf("翻译了 %d 条消息", n),
				"level": "ok",
			})
		}
	}
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				flushNotice()
			}
		}
	}()

	go func() {
		// 批量翻译：扫描协程 → out → 批量器 → results。
		//
		// 为什么本循环不直接调 engine.Translate：那是**同步**的，第一条消息
		// 就会把循环卡在翻译上（最长几秒），后面的消息根本没机会入批，
		// 0.3 秒窗口永远等不到第二条——批量等于没做，但代码看起来是做了的。
		// V1 的消费线程只负责「取 + 攒 + 到期冲刷」，这里保持同样的分工：
		// 批量器独占 out，本循环只消费成品。
		//
		// 缓冲 64 是防抖：本循环要做发 SSE、叫窗口、弹提示这些事，
		// 偶尔慢一点不该让批量器停下来等。
		results := make(chan domain.RenderedMessage, 64)
		go a.engine.Batch.Run(ctx, out, results)

		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-results:
				// ⚠️ 只记**真正调用过 LLM** 的消息。
				//
				// V1 `logger.py:186-191` 只在真的翻译过时写翻译日志；V2 原来对
				// 每条消息（含缓存命中、自身消息跳过、占位桩）都写一行，于是
				// 日志里塞满了没花过 API 的条目，「缓存到底省了多少」反而看不出来。
				if msg.CacheState == "llm" {
					a.log.Translation(msg.Provider, msg.Model, msg.Original, msg.Translated)
				}
				srv.Publish("message.translated", msg)

				// ① 把窗口叫回来（仅在确实隐藏时发 IPC，免得每条消息都白跑一趟）
				if st := sup.Status(); st.Actual.Display != nil && !st.Actual.Display.Visible {
					if err := sup.SetVisible(true); err != nil &&
						!errors.Is(err, native.ErrNotConnected) {
						a.log.Warn("UI", "新消息显示窗口失败: "+err.Error())
					}
				}
				// ② 累计非自己消息，交给上面的 250ms 节拍去弹提示
				if !msg.IsSelf {
					noticeMu.Lock()
					noticeCount++
					noticeMu.Unlock()
				}
				// 统计随手一起推：前端不必自己按消息累加，
				// 那样前端与 Go 会各算一套，迟早对不上。
				st := a.engine.Stats()
				srv.Publish("stats.updated", map[string]any{
					"translated":  st.Translated,
					"cached":      st.Cached,
					"selfSkipped": st.SelfSkipped,
					"total":       st.Total(),
					"savingsPct":  st.SavingsPct(),
				})
			}
		}
	}()

	// Ctrl+C 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\n正在退出...")
		// 顺序与 quit() 一致：先请 native 优雅退出（撤销托盘图标）再取消 ctx。
		// os.Exit **不会**执行 defer，所以这里把能同步做的收尾先做完。
		quit()
		src.Stop()
		os.Exit(0)
	}()

	time.Sleep(600 * time.Millisecond)
	fmt.Printf("监控状态: %s\n", src.Status().State)
	fmt.Printf("API: http://%s  （SSE: /api/events）\n", addr)
	fmt.Println("按 Ctrl+C 退出")

	// 退出：要么 HTTP 服务出错，要么 ctx 被取消。
	//
	// ctx 会在三条路径上被取消：Ctrl+C、托盘菜单的 Quit、前端右键菜单的 Exit
	// （后两条都走 POST /api/quit → OnQuit → cancel）。
	//
	// ⚠️ 这里以前是裸的 `srv.ListenAndServe()`——它**永远阻塞**，只在出错时返回。
	//    后果是：cancel() 之后 native 因为管道断开先退出了（用户看到悬浮窗消失，
	//    以为退干净了），Go 却一直挂着、8791 继续监听。再启动一个新实例就会出现
	//    两个进程抢同一个命名管道。改成 srv.Run(ctx) 之后才有真正的优雅退出。
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "服务退出: %v\n", err)
	}

	// ── 退出收尾（V1 `main.py:_shutdown` 的等价物）──
	//
	// 顺序不能换：
	//  1. 先补落盘窗口位置。用户拖完窗口、1 秒内就退出的话去抖计时器还没到点，
	//     不补这一次，最后那次拖动白拖，下次启动窗口回到旧位置。
	//  2. 再停扫描并**等它真的退出**。只 cancel 不等待的话，已经读进来、
	//     还在翻译管道里的最后几条消息会随进程一起消失——日志里有、界面上没有。
	//     5 秒这个数是照抄 V1 `main.py:_shutdown` 的 join 超时。
	//  3. 最后请 native 优雅退出。必须排在 Kill 之前：强杀的进程不会撤销托盘
	//     图标，Windows 外壳会把鬼影留在通知区域，直到用户把鼠标划过去
	//     （V1 `tray_icon.py:156-162` 的 NIM_DELETE 就是这个作用）。
	saveWindowPos()
	saveSettingsPos()

	src.Stop()
	if err := src.Wait(5 * time.Second); err != nil {
		a.log.Warn("SYS", "退出时"+err.Error())
	}

	// native 的优雅退出**不在这里发**：请求由 Supervisor 在其 Run 的
	// ctx 结束分支里、teardown 之前发出。放在这里发过一次是空转——
	// 退出正是靠取消 ctx 触发的，走到这一行时连接早就被关掉了。
}

// waitForSubscriber 等到 SSE 至少有一个订阅者，或超时 / ctx 结束。
//
// ⚠️ 为什么要等：SSE **不补发**订阅之前产生的事件（ADR-016 的已知限制，
// frontend 的 useEvents.ts 文件头也写着这一条）。启动自检如果不等前端连上
// 就发，那两条诊断会推给零个订阅者——「诊断进界面」变成「诊断进空气」，
// 而且不会有任何报错。
func waitForSubscriber(ctx context.Context, srv *api.Server, max time.Duration) bool {
	deadline := time.Now().Add(max)
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		if srv.SubscriberCount() > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// publishSystemMessage 往消息列表里插一条「系统」消息。
//
// 对应 V1 的 `overlay.add_message("System", original, translated, is_self=True)`
// （main.py:124-126、156-162）。
//
// ⚠️ 是 `is_self=True`、**不是** `is_system=True`：V1 传的就是这个组合
// （`add_message` 的 `is_system` 默认 False，见 overlay.py:581-583）。
// V2 前端的 MessageItem 对这两个标志走不同渲染分支，
// 顺手改成 is_system 会让启动诊断长成另一个样子，与 V1 不一致。
func publishSystemMessage(srv *api.Server, speaker, original, translated string) {
	if translated == "" {
		translated = original
	}
	srv.Publish("message.translated", domain.RenderedMessage{
		// ID 必须唯一：前端按 id 定位/去重，用固定值会让第二条盖掉第一条
		ID:         fmt.Sprintf("sys-%d", time.Now().UnixNano()),
		Speaker:    speaker,
		Original:   original,
		Translated: translated,
		Lang:       "zh-CN",
		Timestamp:  time.Now().Format("15:04:05"),
		IsSelf:     true,
		CacheState: "self_skip",
	})
}

// runReplay 演示模式：把当前日志从头读一遍（V1 真实行为是跳过历史）。
func runReplay(a *app) {
	latest := chatlog.FindLatestLog(a.logDir)
	if latest == "" {
		fmt.Println(chatlog.LogDirStatus(a.logDir))
		os.Exit(1)
	}
	fmt.Printf("回放: %s\n\n", filepath.Base(latest))

	f, err := os.Open(latest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开日志失败: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	var c counters
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ev := chatlog.ParseLine(sc.Text(), "")
		if ev == nil {
			continue
		}
		msg := a.engine.Translate(context.Background(), ev)
		c.add(msg.CacheState)
		printMessage(msg)
		a.log.Translation(msg.Provider, msg.Model, msg.Original, msg.Translated)
	}
	c.summary()
}

// runTail 真实模式：跳过历史、只处理新写入的消息，运行 30 秒。
func runTail(a *app) {
	out := make(chan domain.Event, 500)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("\n监控中（30 秒，只处理新消息——V1 的真实行为）...\n\n")
	src := chatlog.New(a.logDir, func() string { return a.cfg.PlayerName }, nil)
	if err := src.Start(ctx, out); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(600 * time.Millisecond)
	fmt.Printf("状态: %s\n", src.Status().State)

	var c counters
	for {
		select {
		case <-ctx.Done():
			c.summary()
			src.Stop()
			return
		case ev := <-out:
			msg := a.engine.Translate(ctx, &ev)
			c.add(msg.CacheState)
			printMessage(msg)
			a.log.Translation(msg.Provider, msg.Model, msg.Original, msg.Translated)
		}
	}
}
