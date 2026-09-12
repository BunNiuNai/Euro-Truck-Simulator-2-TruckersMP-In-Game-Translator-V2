// Package api REST 接口与 SSE 事件推送（前端的唯一入口，ADR-009）。
//
// 契约见 docs/architecture.md 第 9 节。
//
// ⚠️ 与架构文档 v1.3 的差异（ADR-016）：事件推送用 **SSE** 而不是 WebSocket。
// 理由：本场景只需要「服务端 → 前端」单向推送（命令走 REST），
// 而 SSE 是纯 HTTP、浏览器 EventSource 自带断线重连、无需手写 RFC6455 帧协议，
// 也不引入第三方依赖。详见 docs/architecture.md 的 ADR-016。
//
// 不变量：
//   - 错误响应统一为 {"error":{"code":"...","message":"..."}}，message 与 V1 逐字一致
//   - 前端不做本地持久化，配置/日志/统计的唯一事实源在 Go
//   - 只监听回环地址（本机单用户工具，不对外暴露）
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/compose"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/providers"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/logger"
)

// Translator 是 api 需要的翻译能力（由 engine.Translator 实现）。
type Translator interface {
	TranslateText(text string) domain.RenderedMessage
	TargetLanguage() string
	Stats() domain.Stats
}

// ProviderTester 是 api 需要的 Provider 管理能力（由 providers.Client 实现）。
type ProviderTester interface {
	TestConnection(p config.Provider) (bool, string)
	// FetchModels 拉取某 Provider 的可用模型（设置页的 📥 按钮）。
	// 返回 providers.ModelList 而不是拆成多个返回值：那是个现成的结果类型，
	// 拆开只会让「成功但列表为空」与「失败」更难区分。
	FetchModels(ctx context.Context, p config.Provider) providers.ModelList
	Health() map[string]any
}

// ClipboardWriter 是「把文本写进系统剪贴板」的能力（由 native.Supervisor 实现）。
//
// 为什么不由前端调 navigator.clipboard：WebView2 里那个 API 需要用户手势
// 或显式授权，而热键触发时并没有手势。走 native 的 Win32 剪贴板最可靠。
type ClipboardWriter interface {
	CopyText(ctx context.Context, text string) error
}

// WindowMover 是「拖动/缩放悬浮窗」的能力（由 native.Supervisor 实现）。
//
// 为什么必须绕经 Go，而不是前端直接找 native：前端只活在 WebView 里，唯一
// 通向外界的路径就是 HTTP；native 不监听任何网络端口，两端只靠命名管道相连。
type WindowMover interface {
	BeginMoveResize(zone string) error
}

// ManualSender 是「手动发送」的完整链路：中文 → 译文 → 藏窗 → 模拟按键 →
// 读聊天日志确认（由 internal/compose.Sender 实现）。
//
// 与 RawSender 的区别：这条会**翻译**，所以它收发的是中文，返回的是整条
// 结果（含译文），前端据此渲染提示与消息列表。
type ManualSender interface {
	Send(ctx context.Context, chinese string) compose.Outcome
}

// RawSender 是「把这串文本原样发进游戏聊天」的能力（广告发送用）。
//
// 与 ManualSender 的区别：**不翻译**。广告内容本来就是写好的最终文本。
type RawSender interface {
	SendRaw(ctx context.Context, text string) error
}

// Options 构造参数。
type Options struct {
	// Config 是**已加载好的配置对象**。传进来之后 api 与调用方共享同一个指针。
	//
	// 为什么必须共享：调用方（cmd/translator）也要改配置（托盘切穿透、窗口
	// 几何记忆），而 GET /api/config 与 PUT /api/config 都基于 api 这一份。
	// 两份副本的后果是「一边改了、另一边拿旧值把改动覆盖回去」。
	// 为 nil 时 api 自己加载 ConfigPath，保持旧的调用方式可用。
	Config        *config.Config
	Addr          string // 默认 127.0.0.1:8791
	ConfigPath    string // resources/config.json（默认模板）
	PresetsPath   string // resources/providers.json
	DictionaryDir string // 可选：/api/dictionary 用
	Log           *logger.Logger
	Translator    Translator
	Providers     ProviderTester
	Ad            AdController    // 可选：广告发送（nil 时相关端点回报 503）
	Clipboard     ClipboardWriter // 可选：写系统剪贴板（复制译文热键用）
	WindowMove    WindowMover     // 可选：拖动/缩放悬浮窗
	ManualSend    ManualSender    // 可选：手动发送（中文 → 译文 → 发送 → 确认）
	RawSend       RawSender       // 可选：原样发送一段文本（广告发送）
	// OnQuit 由 POST /api/quit 调用，用于优雅退出。
	// 对应 V1 悬浮窗右键菜单的「Exit」（overlay.py:456 → main.py:214 `_shutdown`）。
	OnQuit func()
	// NativeAlive 报告 native 能力层是否在线（由 Supervisor 的状态提供）。
	// 为 nil 时健康检查保守地报 false，而不是假装在线。
	NativeAlive func() bool
	FrontendDir   string          // 可选：前端构建产物目录
	// FrontendFS 是可选的前端产物文件系统（单 EXE 打包时用 go:embed 提供）。
	//
	// 与 FrontendDir 的关系是**目录优先、FS 兜底**：
	// 开发期用 `-frontend` 指向刚构建的产物，那时内嵌的那份多半是上一次
	// 打包留下的旧版本，优先它只会让人改了半天前端却看不到变化；
	// 而打包后的 EXE 旁边根本没有那个目录，自然落到内嵌的那份。
	// 两者都不给就不提供静态文件，只跑 REST/SSE（联调时前端由 Vite dev server 提供）。
	FrontendFS fs.FS
	// OnConfigChanged 在 PUT /api/config 成功后调用（用于热应用新配置）
	OnConfigChanged func(cfg *config.Config)
}

// Server 是 HTTP 服务。
type Server struct {
	opts Options
	hub  *hub
	// msgs 是最近消息的快照环（GET /api/messages），
	// 用于前端刷新或长时间断线后把消息列表取回来。
	msgs *messageRing

	mu  sync.RWMutex
	cfg *config.Config
}

// New 创建服务。
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8791"
	}
	// ⚠️ 配置**只有一个对象**。
	//
	// 以前 api.New 自己 `config.Load` 一份，而 cmd/translator/main.go 又持有
	// 自己那份（`a.cfg`），两份从此各走各的：
	//   · 托盘改了鼠标穿透 → 只写 a.cfg 与磁盘，Server.cfg 还是旧的
	//   · 设置页 GET /api/config 拿到旧值，用户下一次保存**就把托盘的改动
	//     整份写回去了**——用户的改动被静默回滚。
	// 注入同一个指针之后就不存在「两份」这回事了。
	cfg := opts.Config
	if cfg == nil {
		loaded, warnings, err := config.Load(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg = loaded
		for _, w := range warnings {
			if opts.Log != nil {
				opts.Log.Warn("CFG", w)
			}
		}
	}
	s := &Server{opts: opts, hub: newHub(), msgs: newMessageRing(), cfg: cfg}
	return s, nil
}

// Hub 供外部（engine / task）推送事件。
func (s *Server) Publish(evType string, payload any) {
	// 消息额外留一份快照（GET /api/messages）。
	//
	// 事件流是「推」的：错过就没了。前端刷新页面、或断线久到超出补发窗口时，
	// 光靠 SSE 拿不回当前消息列表——而那正是用户最在意的内容。
	s.rememberIfMessage(evType, payload)
	s.hub.publish(event{Type: evType, Payload: payload})
}

// rememberIfMessage 把 message.translated 的载荷存进快照环形缓冲。
func (s *Server) rememberIfMessage(evType string, payload any) {
	if evType != "message.translated" {
		return
	}
	// 只认值类型：载荷一直是值传递（见 cmd/translator 的 srv.Publish 调用），
	// 断言失败说明调用方换了类型，那时宁可不存也不要存半个进去。
	if msg, ok := payload.(domain.RenderedMessage); ok {
		s.pushMessage(msg)
	}
}

// UpdateConfig 在**锁保护下**就地修改配置。
//
// 这是除了 PUT /api/config 之外，唯一被允许改配置的入口。
//
// 为什么必须走它：配置现在是 api 与 cmd/translator **共享的同一个对象**
// （见 Options.Config 的说明）。托盘切穿透、窗口几何记忆这些写入方若直接
// `a.cfg.ClickThrough = x`，就与 GET /api/config 的读、与 PUT 的写并发，
// 属于无保护的数据竞争——轻则读到撕裂的值，重则崩。
//
// 回调在锁内执行，所以**不要在里面做 I/O 或调用其它可能阻塞的服务**
// （落盘请在回调返回之后做）。
func (s *Server) UpdateConfig(mutate func(*config.Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		mutate(s.cfg)
	}
}

// Config 返回当前配置的**深拷贝**（只读用途）。
//
// 必须是深拷贝：返回值被当作**快照**持有——GET /api/config 把它整份序列化出去、
// 落盘路径用它做保存、健康检查从它读字段；而配置本体会被 UpdateConfig 就地改写。
// 浅拷贝只有顶层结构体是新的，LLMProviders / AdMessages 这些切片仍与本体共享
// 底层数组——快照会跟着本体一起变，那就不是快照了。
//
// ⚠️ 改这段之前先看 config.Clone 与 secrets_test.go：深拷贝语义被测试钉住。
func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// SubscriberCount 返回当前 SSE 订阅者数量。
//
// 给启动自检用：SSE **不补发**订阅之前产生的事件（ADR-016 的已知限制），
// 所以要等真的有前端订阅了再推诊断消息，否则那些消息谁也收不到。
func (s *Server) SubscriberCount() int { return s.hub.subscriberCount() }

// NotifyConfigChanged 在配置被**外部**改动（文件热重载）后触发一次热应用。
//
// 与 PUT /api/config 复用同一段 OnConfigChanged 回调，不另写一份热应用逻辑：
// 复制出去的话，将来加一个字段只会改到一边，于是出现「从设置页改生效、
// 手改配置文件不生效」这种一半好一半坏的状态——而用户根本分不清
// 两条路径的区别。
//
// 调用方必须**先**用 UpdateConfig 把新值就地写进共享配置对象，再调本函数；
// 回调拿到的是那个共享对象本身（不是副本，理由见 handleConfig 里的说明）。
func (s *Server) NotifyConfigChanged() {
	if s.opts.OnConfigChanged == nil {
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if cfg == nil {
		return
	}

	// 在锁外调用：回调里会做 IPC（SetHotkeys / SetDark）等可能阻塞的事，
	// 持着 s.mu 调会把 GET /api/config 一起卡住。
	s.opts.OnConfigChanged(cfg)
	s.Publish("config.reloaded", map[string]any{
		"changedKeys": []string{},
		// 标出来源：设置页保存与手改文件走的是同一条热应用，
		// 但排障时需要知道到底是哪边触发的。
		"external": true,
	})
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.wrap(s.handleHealth))
	mux.HandleFunc("/api/config", s.wrap(s.handleConfig))
	mux.HandleFunc("/api/providers", s.wrap(s.handleProviders))
	mux.HandleFunc("/api/providers/test", s.wrap(s.handleProviderTest))
	mux.HandleFunc("/api/providers/models", s.wrap(s.handleProviderModels))
	mux.HandleFunc("/api/presets", s.wrap(s.handlePresets))
	mux.HandleFunc("/api/logs", s.wrap(s.handleLogs))
	mux.HandleFunc("/api/logs/reveal", s.wrap(s.handleLogsReveal))
	mux.HandleFunc("/api/stats", s.wrap(s.handleStats))
	// 消息快照：前端刷新页面或长断线后把消息列表取回来（events.go 的 id 补发
	// 盖不住「整个页面重新加载」——那时 Last-Event-ID 已经没了）
	mux.HandleFunc("/api/messages", s.wrap(s.handleMessages))
	mux.HandleFunc("/api/translate", s.wrap(s.handleTranslate))
	mux.HandleFunc("/api/send", s.wrap(s.handleSend))
	mux.HandleFunc("/api/ad/status", s.wrap(s.handleAdStatus))
	mux.HandleFunc("/api/ad/start", s.wrap(s.handleAdStart))
	mux.HandleFunc("/api/ad/stop", s.wrap(s.handleAdStop))
	mux.HandleFunc("/api/clipboard", s.wrap(s.handleClipboard))
	mux.HandleFunc("/api/window/move", s.wrap(s.handleWindowMove))
	mux.HandleFunc("/api/quit", s.wrap(s.handleQuit))
	mux.HandleFunc("/api/compose/send", s.wrap(s.handleComposeSend))
	mux.HandleFunc("/api/events", s.handleEvents)

	if s.opts.FrontendDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.opts.FrontendDir)))
	} else if s.opts.FrontendFS != nil {
		mux.Handle("/", http.FileServer(http.FS(s.opts.FrontendFS)))
	}
	return mux
}

// ListenAndServe 启动服务（阻塞），只在出错时返回。
//
// ⚠️ 退出请用 Run 而不是这个：它没有 http.Server 的句柄，ctx 取消也关不掉它，
//    调用方会一直卡在这里（见 Run 的注释）。
func (s *Server) ListenAndServe() error {
	return s.Run(context.Background())
}

// Run 启动服务，并在 ctx 被取消时**优雅关闭**后返回。
//
// ⚠️ 这个函数的存在本身就是一次修复。
//
// 原来 runServe 末尾是裸的 `srv.ListenAndServe()`，而它只在出错时返回。
// 于是 /api/quit（悬浮窗右键的 Exit、托盘菜单的 Quit）调用 cancel() 之后：
//   · native 因为管道断开先退出了——用户看到悬浮窗消失，以为退干净了；
//   · Go 却卡在 ListenAndServe 里不退，8791 端口继续监听。
// 表现就是「退出了但其实没退」，再启动一个新实例还会有两个进程抢同一个管道。
// Ctrl+C 之所以看不出问题，是因为那条路径自己调了 os.Exit(0)。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.opts.Addr, Handler: s.Handler()}
	if s.opts.Log != nil {
		s.opts.Log.Info("API", "HTTP 服务监听 "+s.opts.Addr)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		// 正常关闭时 ListenAndServe 返回 ErrServerClosed，那不是错误
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// 给在途请求一点时间收尾（比如正在返回的 /api/quit 响应本身），
		// 超时就强制关，绝不能因为某个连接赖着不走而卡住整个退出流程。
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("HTTP 服务未能优雅退出: %w", err)
		}
		return nil
	}
}

// shutdownWait 是 HTTP 服务优雅退出的等待上限。
const shutdownWait = 3 * time.Second

// Version 是产品版本号，由 /api/health 提供给界面 header 显示。
//
// 用 var 而不是 const：打包脚本可以用 `-ldflags "-X .../internal/api.Version=..."`
// 把真实版本号注入进来，不必改源码。
var Version = "v2.0.0-dev"

// ── 响应辅助 ──────────────────────────────────────────────────

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiResponse struct {
	OK    bool      `json:"ok"`
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiResponse{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, status int, code domain.ErrorCode, detail string) {
	writeJSON(w, status, apiResponse{
		OK:    false,
		Error: &apiError{Code: string(code), Message: code.Message(detail)},
	})
}

// wrap 统一处理 panic 与方法校验，避免单个接口出错拖垮服务。
func (s *Server) wrap(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if s.opts.Log != nil {
					s.opts.Log.Error("API", "handler panic: "+toString(rec))
				}
				writeErr(w, http.StatusInternalServerError, domain.ErrTranslateFailed, toString(rec))
			}
		}()
		h(w, r)
	}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// ── 各接口 ────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	cfg := s.Config()
	enabled := 0
	for _, p := range cfg.LLMProviders {
		if p.Enabled {
			enabled++
		}
	}
	// nativeAlive 以前是**硬编码的 false**，注释还写着「阶段 3 接入 native 之后
	// 才有意义」——接入了却没人回来改，于是这个字段一直在说谎：
	// 明明三个窗口都开着、native 进程也在跑，健康检查却报未连接。
	// 现在如实取自 Supervisor 的连接状态（未注入时保守地报 false）。
	nativeAlive := false
	if s.opts.NativeAlive != nil {
		nativeAlive = s.opts.NativeAlive()
	}

	data := map[string]any{
		"configPath":  config.ConfigPath(),
		"configDir":   config.ConfigDir(),
		"logDir":      config.LogDir(),
		"targetLang":  cfg.TargetLanguage,
		"providers":   map[string]any{"total": len(cfg.LLMProviders), "enabled": enabled},
		"nativeAlive": nativeAlive,
		"sseClients":  s.hub.subscriberCount(),
		// 界面 header 里的版本号取自这里。
		//
		// 之前 header 是自己硬编码一个版本号，而健康检查里**根本没有**这个
		// 字段：两处各说各话，升级时只会改到其中一处，用户看到的版本
		// 和实际跑的二进制对不上——排障时这会把人带偏。
		"version": Version,
	}
	if s.opts.Providers != nil {
		data["providerHealth"] = s.opts.Providers.Health()
	}
	writeOK(w, data)
}

// 密钥脱敏（`__UNCHANGED__` 哨兵 + 按 id 还原）已在本次改动中**整套删除**。
//
// 那套机制是 V2 自己发明的，V1 里不存在——V1 的密钥就是配置里的一个普通字段，
// 读出来、显示、原样写回，只在落盘时加密。哨兵机制带来的缺陷全部只在它身上
// 才可能发生（空 id 还原成空串、byID[""] 冲突导致两个 Provider 共用密钥、
// 前端把哨兵当空值显示成「尚未配置」），而且每一个都极难查。
//
// 这里保留一段说明而不是删干净，是为了让后来者知道**为什么不再加回去**。

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 直接返回真实配置，**不脱敏**。
		//
		// ⚠️ 这里曾经调 `maskAPIKeys(cfg)` 把密钥换成 `__UNCHANGED__` 哨兵，
		// 再在 PUT 时按 id 还原。那套机制是 V2 自己发明的，V1 里**根本不存在**
		// ——V1 的密钥就是配置里的一个字段：读出来、显示在设置窗口、保存时原样
		// 写回，只在**落盘那一刻**加密（`config.py:359-361`）。
		//
		// 哨兵机制引入了一连串只在它身上才可能发生的缺陷，而且每一个都很难查：
		//   · 按 id 还原，而设置页新建的 Provider **没有 id** → 还原成空串 → 401
		//     （用户看到「测试说密钥无效，但翻译明明能用」）
		//   · `byID[""]` 冲突：两个空 id 的 Provider 共用一个键 →
		//     一个写错，另一个被还原成同一个值 → 「所有密钥都失效」
		//   · 前端把哨兵当成「没配」→ 界面显示「尚未配置」，与实际相反
		//
		// 代价说明：密钥会经过我们**自己的前端进程**。那不是第三方，就是本程序
		// 的 WebView2 窗口；而 V1 是放在同一个进程里，差别不构成风险等级的提升。
		// 「密钥不离开 Go 进程」是 V2 自加的约束，不是 V1 的行为。
		writeOK(w, s.Config())

	case http.MethodPut, http.MethodPost:
		var incoming config.Config
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
			return
		}
		// 前端回传的就是真实密钥（不再有哨兵要还原）。
		// `config.Save` 内部会在落盘时加密——加密的时机与 V1 完全一致。

		if err := config.Save(&incoming); err != nil {
			writeErr(w, http.StatusInternalServerError, domain.ErrTranslateFailed, err.Error())
			return
		}
		// ⚠️ **就地改写**，不要 `s.cfg = incoming.Clone()`。
		//
		// 那行会把共享的配置对象换成一个新指针：cmd/translator 手里的 `a.cfg`
		// 从此指向旧对象，而托盘改的也正是那份旧对象——两边又开始各写各的，
		// 「一边改、另一边用旧值覆盖回去」的老问题原样复活。
		// 就地赋值保证指针身份不变，所有持有者看到的始终是同一份数据。
		s.mu.Lock()
		if s.cfg != nil {
			*s.cfg = *incoming.Clone()
		} else {
			s.cfg = incoming.Clone()
		}
		sharedCfg := s.cfg
		s.mu.Unlock()

		if s.opts.OnConfigChanged != nil {
			// ⚠️ 传**共享对象本身**，不能传副本的地址。
			//
			// 回调里有一句 `a.cfg = cfg`（main 用它更新自己持有的指针）。若这里
			// 传的是 `&某局部副本`，a.cfg 就会指向一个 handler 返回后即失效的栈
			// 变量——之后任何一次 `a.cfg.PlayerName` 都是读已释放内存。
			s.opts.OnConfigChanged(sharedCfg)
		}
		if s.opts.Log != nil {
			s.opts.Log.Info("CFG", "配置已保存")
		}
		s.Publish("config.reloaded", map[string]any{"changedKeys": []string{}})
		writeOK(w, map[string]any{"saved": true})

	default:
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	cfg := s.Config()
	type item struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Label    string `json:"label"`
		Endpoint string `json:"endpoint"`
		Model    string `json:"model"`
		Enabled  bool   `json:"enabled"`
		Format   string `json:"apiFormat"`
		Weight   int    `json:"weight"`
		// 注意：**不返回 api_key**——真实密钥只在 GET /api/config 里下发，
		// 设置页的 Provider 卡片读的是那一份。
		HasKey bool `json:"hasKey"`
	}
	out := make([]item, 0, len(cfg.LLMProviders))
	for i, p := range cfg.LLMProviders {
		out = append(out, item{
			Index: i, ID: p.ID, Label: p.Label, Endpoint: p.Endpoint,
			Model: p.Model, Enabled: p.Enabled, Format: p.APIFormat,
			Weight: p.Weight, HasKey: p.APIKey != "",
		})
	}
	writeOK(w, out)
}

func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "Provider 服务未就绪")
		return
	}
	var p config.Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}

	// ⚠️ 这里**不需要**还原密钥——前端发回来的就是真实密钥。
	//
	// 曾经需要：V2 自创过「__UNCHANGED__ 哨兵 + 按 id 还原」，那时 GET /api/config
	// 回的是占位符，不还原就会把占位符当 Bearer 发出去，凡是已保存密钥的 Provider
	// 一律测出 401——用户只会以为自己的 Key 坏了。那套机制已整套删除，
	// 原因见本文件 handleConfig 上方的说明。
	//
	// 现在与 V1 一致（`main.py:1379-1391` 用的就是内存里的真实密钥）：
	// GET 回真值、界面直接回显、PUT 原样写回，加密只发生在落盘那一刻。
	// 所以**不要**在这里加任何"还原"逻辑——没有哨兵可还原，加了只会把真密钥改坏。

	ok, msg := s.opts.Providers.TestConnection(p)
	writeOK(w, map[string]any{"success": ok, "message": msg})
}

// handleProviderModels 拉取某个 Provider 的可用模型列表。
//
// ⚠️ 这是 V2 **有意超出 V1 活代码**的一处，且是经过明确决定的：
// 📥 按钮只存在于 V1 的 `settings_ui.py`（`main.py:327` 标 DEPRECATED、
// 全仓无实例化点），`model_fetcher.py` 也只被它引用；活着的 `main.py:1322`
// 只有「Model / 模型」文本框。也就是说这个界面元素在 V1 里**从未对用户出现过**。
// 后端的 `Client.FetchModels` 早已存在且忠于 V1，缺的只是这个出口。
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "Provider 服务未就绪")
		return
	}
	var p config.Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}

	// 与测试端点同理：**不需要**还原密钥，前端回传的就是真实密钥
	// （哨兵机制已整套删除，见 handleConfig 上方的说明）。

	// 独立给超时：FetchModels 打的是外部网络，不能跟着请求的 ctx 无限等。
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	res := s.opts.Providers.FetchModels(ctx, p)
	models := res.Models
	if models == nil {
		// 显式给空数组：nil 会被序列化成 null，前端 `for (const m of models)`
		// 会直接抛异常，而不是「没有模型」。
		models = []string{}
	}

	// 拉取失败**不是**服务器错误：绝大多数情况是 endpoint 或密钥填错了。
	// 用 200 + success:false 回报，让界面把原因显示在按钮旁边，
	// 而不是弹一个「服务器故障」，把用户引到错误的方向去查。
	writeOK(w, map[string]any{
		"success":   res.Success,
		"models":    models,
		"error":     res.Error,
		"latencyMs": res.LatencyMs,
	})
}

// handlePresets 返回 resources/providers.json 的内容。
//
// 不必在 Go 里给预设再建一遍模型（它本来就是给前端用的结构），
// 但仍然要**解析再包信封**——早期版本直接把文件字节写出去，
// 于是响应里没有 {ok, data} 外套，前端按统一契约解析时
// 会在 body.error.code 上抛 TypeError。所有 /api/* 必须走同一个信封。
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	raw, err := readFileIfExists(s.opts.PresetsPath)
	if err != nil || raw == nil {
		writeOK(w, map[string]any{"presets": []any{}, "categories": []any{}, "icons": map[string]string{}})
		return
	}

	// 用 map 而非具名结构体：结构随 providers.json 演进，Go 侧不做约束；
	// 但要确认它确实是 JSON 对象，否则前端拿到的会是数组或标量。
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeErr(w, http.StatusInternalServerError, domain.ErrBadResponse,
			"providers.json 不是合法 JSON: "+err.Error())
		return
	}
	writeOK(w, doc)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.opts.Log == nil {
			writeOK(w, map[string]any{"lines": []string{}, "dir": "", "files": []string{}})
			return
		}
		n := 200
		if v := r.URL.Query().Get("n"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				n = parsed
			}
		}
		writeOK(w, map[string]any{
			"lines": s.opts.Log.Recent(n),
			"dir":   s.opts.Log.Dir(),
			"files": s.opts.Log.Files(),
		})

	case http.MethodDelete:
		if s.opts.Log == nil {
			writeOK(w, map[string]any{"deleted": 0})
			return
		}
		deleted, errs := s.opts.Log.DeleteAll()
		writeOK(w, map[string]any{"deleted": deleted, "errors": errs})

	default:
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
	}
}

// handleLogsReveal 在系统文件管理器里打开日志目录。
//
// 对应 V1 `main.py:_open_log_dir`（用 `os.startfile` 打开目录）。
//
// 注意：Windows 的 explorer.exe **即使成功也常返回非零退出码**，
// 所以只能用 Start() 启动、不看退出码——用 Run() 判成败会误报失败。
func (s *Server) handleLogsReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	dir := config.LogDir()
	if err := openInFileManager(dir); err != nil {
		writeErr(w, http.StatusInternalServerError, domain.ErrTranslateFailed,
			"打开目录失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"dir": dir})
}

// handleComposeSend 是手动发送（输入栏回车 / 发送热键）的入口。
//
// 整条链路在 internal/compose 里：翻译 → 校验 → 藏起悬浮窗 → 模拟按键 →
// 读聊天日志确认。这里只做参数校验与超时。
//
// ⚠️ 这个 handler 会阻塞好几秒（250ms 藏窗 + 约 1.5s 按键 + 最多 2.5s 确认），
// 这是**设计如此**：它是整条链路的同步结果，前端拿到 result 才知道该显示
// 「已发送并确认 ✓」还是回填输入框。HTTP 每个请求本来就是独立 goroutine。
func (s *Server) handleComposeSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.ManualSend == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed,
			"发送链路不可用（native 未连接或未配置 Provider）")
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, "text 为空")
		return
	}

	// 给足时间：翻译可能走网络，后面还有固定约 4.3 秒的发送与确认。
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	out := s.opts.ManualSend.Send(ctx, text)
	writeOK(w, out)
}

// handleSend 把一段文本**原样**发进游戏聊天（广告发送用，不翻译）。
//
// 前端「广告发送」页会用它做测试发送。以前这里固定返回 501，
// 现在 native 的输入模拟已经就绪，可以真的发了。
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.RawSend == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed,
			"发送能力不可用（native 未连接）")
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, "text 为空")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.opts.RawSend.SendRaw(ctx, text); err != nil {
		writeErr(w, http.StatusBadGateway, domain.ErrTranslateFailed, err.Error())
		return
	}
	writeOK(w, map[string]any{"sent": true})
}

// handleClipboard 把一段文本写进系统剪贴板。
//
// 对应 V1 的「复制热键」：用户按下 copy_hotkey 时，把当前译文放进剪贴板。
// 走 native 的 Win32 剪贴板而不是 WebView 的 navigator.clipboard——
// 后者需要用户手势或授权，而热键触发时并没有手势。
func (s *Server) handleClipboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Clipboard == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed,
			"剪贴板能力不可用（native 未连接）")
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, "text 为空")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.opts.Clipboard.CopyText(ctx, req.Text); err != nil {
		writeErr(w, http.StatusBadGateway, domain.ErrTranslateFailed, err.Error())
		return
	}
	writeOK(w, map[string]any{"copied": true, "length": len(req.Text)})
}

// handleWindowMove 开始一次悬浮窗拖动或缩放。
//
// 由前端在最外圈监听到 mousedown 时调用（见 OverlayApp.vue）。
//
// ⚠️ 前端**发完就放手**，不要 await 结果再去更新界面：这个调用只表示
//    「已经开始拖」，真正的位移在 native 主循环里逐帧推进，直到用户松开
//    鼠标。等它返回再渲染会让拖动明显发滞。
//
// 之所以需要这条链路，是因为 WebView2 的子窗口铺满客户区，宿主窗口在
// 四边收不到 WM_NCHITTEST——纯 Win32 的做法（WM_NCLBUTTONDOWN 交给系统
// 模态循环）又会阻塞 native 主线程、拖垮管道心跳。详见 native/src/window.hpp。
func (s *Server) handleWindowMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.WindowMove == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed,
			"窗口拖动能力不可用（native 未连接）")
		return
	}

	var req struct {
		Zone string `json:"zone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}

	zone := strings.TrimSpace(req.Zone)
	if zone == "" {
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, "zone 为空")
		return
	}

	if err := s.opts.WindowMove.BeginMoveResize(zone); err != nil {
		s.opts.Log.Error("WIN", "拖动/缩放失败 zone="+zone+": "+err.Error())
		writeErr(w, http.StatusBadGateway, domain.ErrTranslateFailed, err.Error())
		return
	}
	// 用户主动拖窗口不算高频操作，留一条审计记录——「窗口自己动了」这类
	// 问题排查时，有没有这条日志是能否定位的分水岭。
	s.opts.Log.Info("WIN", "拖动/缩放 zone="+zone)
	writeOK(w, map[string]any{"zone": zone})
}

// handleQuit 优雅退出。
//
// 对应 V1 悬浮窗右键菜单的「Exit」：overlay.py:456 的 `_on_exit` 接到
// main.py:214 的 `_shutdown`——保存位置与配置、停托盘、停后台线程、销毁窗口。
//
// 不做额外鉴权：服务只监听本机回环地址，能访问这个端口的人本来就能在本机
// 执行任意操作。
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.OnQuit == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "退出能力不可用")
		return
	}

	// 先把回复发出去，稍后才真正退。
	//
	// ⚠️ 顺序不能反过来：等退出流程走完再回包的话，连接早就关了，
	//    前端只会看到一个网络错误，根本分不清「退出成功了」和「崩了」。
	writeOK(w, map[string]any{"quitting": true})

	go func() {
		time.Sleep(150 * time.Millisecond)
		s.opts.OnQuit()
	}()
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}

	targetLang := s.Config().TargetLanguage
	stats := domain.Stats{}
	if s.opts.Translator != nil {
		targetLang = s.opts.Translator.TargetLanguage()
		stats = s.opts.Translator.Stats()
	}

	// total 与 savingsPct 由 Go 算好一并返回：它们是 V1 的口径
	// （分子只含 cached 与 selfSkipped，且用截断而非四舍五入），
	// 前端自己再算一遍迟早会算歪。
	writeOK(w, map[string]any{
		"targetLang":  targetLang,
		"translated":  stats.Translated,
		"cached":      stats.Cached,
		"selfSkipped": stats.SelfSkipped,
		"total":       stats.Total(),
		"savingsPct":  stats.SavingsPct(),
	})
}

func (s *Server) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	var req struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse, err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, "text 为空")
		return
	}
	if s.opts.Translator == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "翻译引擎未就绪")
		return
	}

	msg := s.opts.Translator.TranslateText(req.Text)
	writeOK(w, map[string]any{
		"id":          msg.ID,
		"translation": msg.Translated,
		"provider":    msg.Provider,
		"model":       msg.Model,
		"cache":       msg.CacheState,
		"detected":    msg.Lang,
	})
}
