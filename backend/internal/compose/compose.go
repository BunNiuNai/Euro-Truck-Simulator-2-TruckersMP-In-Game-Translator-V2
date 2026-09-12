// Package compose 实现「手动发送」链路：翻译 → 校验 → 藏起悬浮窗 →
// 模拟按键发到游戏 → 读聊天日志确认。
//
// V1 的对应物是 `compose_sender.py`（181 行）加上 `overlay.py:866-960` 的编排。
//
// **与 V1 的分工差异（架构差异，不是功能差异）**：V1 把编排摊在 UI 线程的
// `after()` 回调里（`_on_send_enter` → `_do_translate` → `_on_translate_done`
// → `_do_auto_send` → `_on_send_done`），V2 把整条编排收在 Go 里，界面只渲染
// 结果。用户看到的行为（提示文案、失败时输入框回填、成功时插入消息）逐条对齐。
//
// 剪贴板的保存与还原**不在这里**：native 的 `sendChatMessage` 内部已经做了
// （模拟按键靠剪贴板粘贴，用完立刻还原）。V1 把这一步放在 compose_sender，
// V2 放在了更靠近使用点的地方，净行为一致。
package compose

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/logger"
)

// 发送结果。取值与 V1 `compose_sender.py:21` 的 `SendResult` 枚举**一字不差**——
// 前端按这些字符串决定提示文案，改名就会静默失配。
const (
	ResultOKConfirmed   = "OK_CONFIRMED"
	ResultOKUnconfirmed = "OK_UNCONFIRMED"
	ResultFailSend      = "FAIL_SEND"
	ResultFailTranslate = "FAIL_TRANSLATION"
	ResultBusy          = "BUSY"
)

// 默认时长，取自 V1。
const (
	// DefaultHideDelay 是「藏起悬浮窗」之后、开始模拟按键之前等的时长
	// （V1 overlay.py:917 的 `time.sleep(0.25)`）。
	DefaultHideDelay = 250 * time.Millisecond
	// DefaultTimeout 是等待聊天日志确认的时长（V1 compose_sender.py:111）。
	DefaultTimeout = 2500 * time.Millisecond
	// DefaultPoll 是没有新行时的轮询间隔（V1 compose_sender.py:176）。
	DefaultPoll = 80 * time.Millisecond
)

// NativeOps 是发送链路需要 native 做的那两件事。
//
// 抽成接口是为了让本包能脱离真实 native 自测——发送链路里最容易出错的
// 是**步骤顺序**（先藏窗、再按键、最后还要把窗放回来），而不是系统调用本身。
type NativeOps interface {
	// SendChat 模拟按键把这段英文发进游戏聊天框。
	SendChat(ctx context.Context, english string) error
	// SetOverlayVisible 显示/隐藏悬浮窗。
	SetOverlayVisible(ctx context.Context, visible bool) error
}

// Options 构造参数。
type Options struct {
	// Translate 把中文译成要发出去的英文（V1 的 translate_for_send）。
	Translate func(text string) (string, error)
	Native    NativeOps
	// Confirm 判断这条文本有没有在聊天日志里出现（见 ChatLogConfirmer）。
	Confirm func(text string) bool
	Log     *logger.Logger

	HideDelay time.Duration // 0 表示用 DefaultHideDelay
	Timeout   time.Duration // 仅在自测里用到；确认超时由 Confirm 自己掌握
}

// Outcome 是一次发送的完整结果。前端据此渲染提示与消息列表。
type Outcome struct {
	Result  string `json:"result"`
	Chinese string `json:"chinese"`
	English string `json:"english"`
	// Message 是给用户看的原因（失败时才有）。
	Message string `json:"message,omitempty"`
}

// Sender 串起整条发送链路。零值不可用，必须用 New 构造。
type Sender struct {
	opt  Options
	busy sync.Mutex
}

func New(opts Options) *Sender {
	if opts.HideDelay <= 0 {
		opts.HideDelay = DefaultHideDelay
	}
	return &Sender{opt: opts}
}

// Send 执行一次完整的手动发送。
//
// 阻塞数秒（藏窗 250ms + 模拟按键约 1.5s + 确认最多 2.5s），调用方要放在
// 自己的 goroutine 里——HTTP handler 天然就是。
func (s *Sender) Send(ctx context.Context, chinese string) Outcome {
	chinese = strings.TrimSpace(chinese)
	out := Outcome{Chinese: chinese}
	if chinese == "" {
		out.Result = ResultFailTranslate
		out.Message = "没有要发送的内容"
		return out
	}

	english, err := s.opt.Translate(chinese)
	if err != nil {
		out.Result = ResultFailTranslate
		out.Message = "翻译失败"
		// V1 在翻译异常时单独记一条 LLM 日志并往消息列表插一条 System 消息
		// （overlay.py:947-955）；消息列表那半由前端拿到 result 后处理。
		s.logError("LLM", "发送翻译失败: "+err.Error())
		return out
	}
	out.English = english

	// 校验不过就**绝不发送**：译文为空、与原文相同、或仍然大部分是中文
	// （说明模型没真的翻），发出去只会让队友看到一串中文。
	if !Validate(chinese, english) {
		out.Result = ResultFailTranslate
		out.Message = "翻译无效，未发送"
		return out
	}

	// 同一时刻只允许一条。V1 用非阻塞锁，并发直接返回 BUSY（compose_sender.py:78），
	// 而不是排队——排队会让第二条在几秒后才发出去，用户早就不看那个输入框了。
	if !s.busy.TryLock() {
		out.Result = ResultBusy
		out.Message = "上一次发送还没结束"
		return out
	}
	defer s.busy.Unlock()

	// ⚠️ 发送前必须把悬浮窗藏起来。
	//
	// 悬浮窗是**置顶**的，模拟按键时它会把前台焦点抢走，那段聊天框就收不到
	// 字符，整条消息会打进空气里。V1 在 overlay.py:911-917 做的正是这件事，
	// 并且在藏好之后又等了 250ms 让窗口动画与焦点切换彻底落定。
	if err := s.opt.Native.SetOverlayVisible(ctx, false); err != nil {
		out.Result = ResultFailSend
		out.Message = "隐藏悬浮窗失败"
		s.logError("SEND", "隐藏悬浮窗失败: "+err.Error())
		return out
	}
	// 无论后面怎么失败，窗口都必须放回来——否则用户眼前会「少一个窗口」，
	// 而且他自己没有任何办法找回来（托盘能在，但那是另一条路）。
	restore := true
	defer func() {
		if restore {
			if err := s.opt.Native.SetOverlayVisible(ctx, true); err != nil {
				s.logError("SEND", "恢复悬浮窗失败: "+err.Error())
			}
		}
	}()

	time.Sleep(s.opt.HideDelay)

	if err := s.opt.Native.SendChat(ctx, out.English); err != nil {
		out.Result = ResultFailSend
		out.Message = "发送失败"
		s.logError("SEND", "发送失败: "+err.Error())
		return out
	}

	// 确认只是「有没有在日志里看到自己这条」，拿不到不代表发送失败——
	// V1 也把它分成 OK_CONFIRMED / OK_UNCONFIRMED 两种，而不是当成错误。
	confirmed := false
	if s.opt.Confirm != nil {
		confirmed = s.opt.Confirm(out.English)
	}
	if confirmed {
		out.Result = ResultOKConfirmed
		s.logInfo("SEND", "发送完成: 确认成功 | "+truncate(out.English, 50))
	} else {
		out.Result = ResultOKUnconfirmed
		s.logInfo("SEND", "发送完成: 确认超时 | "+truncate(out.English, 50))
	}
	return out
}

func (s *Sender) logInfo(tag, msg string) {
	if s.opt.Log != nil {
		s.opt.Log.Info(tag, msg)
	}
}

func (s *Sender) logError(tag, msg string) {
	if s.opt.Log != nil {
		s.opt.Log.Error(tag, msg)
	}
}

// Validate 判断译文能不能发出去。三条规则照抄 V1 `compose_sender.py:57-71`：
//   - 译文不能为空
//   - 译文不能和原文一模一样（说明压根没翻）
//   - 译文中 CJK 字符不能超过 30%（说明还是中文）
func Validate(chinese, english string) bool {
	eng := strings.TrimSpace(english)
	chn := strings.TrimSpace(chinese)
	if eng == "" {
		return false
	}
	if eng == chn {
		return false
	}
	return !mostlyChinese(eng)
}

// mostlyChinese 判断文本里 CJK 字符是否超过 30%。
// 范围与 V1 的正则 `[一-鿿]` 一致，即 U+4E00–U+9FFF。
func mostlyChinese(text string) bool {
	if text == "" {
		return false
	}
	runes := []rune(text)
	cjk := 0
	for _, r := range runes {
		if r >= 0x4E00 && r <= 0x9FFF {
			cjk++
		}
	}
	// V1 用的是 Python 的 len()（字符数），所以这里也按 rune 数算。
	return float64(cjk)/float64(len(runes)) > 0.3
}

// normalize 折叠所有空白为单个空格并去掉首尾空白。
// 聊天日志与译文之间的空格差异（ETS2 会重排空白）不该让确认失败。
func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
