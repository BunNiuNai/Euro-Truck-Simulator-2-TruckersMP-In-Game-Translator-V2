// Package domain 领域对象与错误码。
//
// V1 来源：message_types.py（DisplayMessage / TranslationStats）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - Event / RenderedMessage / Stats 是 Source→Engine→Sink 之间唯一的通信载体
//   - 错误码文案必须与 V1 translator.py:_format_error 逐字一致
//   - 不得引入 V1 没有的用户可见字段（首要原则 P1）
package domain

import (
	"fmt"
	"time"
)

// Event 是 Source 产出的原始事件。对应 V1 monitor.ChatMessage。
//
// JSON tag 是**跨进程契约**：这些对象会经 SSE 直接推给前端。
// 没有 tag 时 encoding/json 会输出 Go 字段名（"Speaker" 而不是 "speaker"），
// 前端按 camelCase 读取就会静默拿到 undefined——一个查起来很痛的失效。
type Event struct {
	// ID 是去重键：speaker + text + timestamp（V1 monitor.py:244）
	ID string `json:"id"`

	Time time.Time `json:"time"`

	// Timestamp 保留原始 HH:MM:SS，用于 UI 右上角对齐显示
	Timestamp string `json:"timestamp"`

	// Speaker 是玩家名；系统消息为 "[Channel]"
	Speaker string `json:"speaker"`

	Text string `json:"text"`

	// Lang 是启发式检测结果，由 Engine 填充
	Lang string `json:"lang"`

	// Origin 标记来源："chatlog" | "screen" | ...
	Origin string `json:"origin"`

	IsSelf   bool `json:"isSelf"`
	IsSystem bool `json:"isSystem"`
}

// RenderedMessage 是交给 Sink 呈现的成品。对应 V1 message_types.DisplayMessage。
type RenderedMessage struct {
	ID         string `json:"id"`
	Speaker    string `json:"speaker"`
	Original   string `json:"original"`
	Translated string `json:"translated"`
	Lang       string `json:"lang"`
	Timestamp  string `json:"timestamp"`
	IsSelf     bool   `json:"isSelf"`
	IsSystem   bool   `json:"isSystem"`

	// Provider / Model 记录实际生效的厂商与模型（V1 翻译日志格式需要）
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// CacheState 说明这条消息**走了哪条路**（由 engine 设置，前端据它决定显示什么）。
	// 全部取值（engine.go / batch.go）：
	//
	//	skip_empty    原文为空
	//	all_target    已全是目标语言
	//	skip_nontext  非文字内容（纯标点/数字/emoji）
	//	skip_target   已是目标语言，跳过
	//	self_skip     自己的消息，不翻译
	//	mixed         混合语言，走了拆分
	//	local_dict    本地词典命中（零 API）
	//	hit           缓存命中（零 API）
	//	llm           真实调用过 LLM
	//	error         出错（**不写缓存**，见 D14）
	//	fallback      批量拆分失败后逐条回退（**要写缓存**，所以不叫 error）
	//
	// ⚠️ 此前这里只写了 "local_dict" | "hit" | "miss"，其中 "miss" 根本不存在。
	CacheState string `json:"cacheState"`

	LatencyMs int `json:"latencyMs"`

	// ErrCode 非空表示这是一条错误提示行
	ErrCode ErrorCode `json:"errCode"`
}

// Stats 对应 V1 message_types.TranslationStats。
type Stats struct {
	Translated  int64 `json:"translated"`
	Cached      int64 `json:"cached"`
	SelfSkipped int64 `json:"selfSkipped"`
}

// Total 对应 V1 TranslationStats.total
func (s Stats) Total() int64 { return s.Translated + s.Cached + s.SelfSkipped }

// SavingsPct 对应 V1 TranslationStats.savings_pct，返回形如 "42%"
func (s Stats) SavingsPct() string {
	if s.Total() == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", int(float64(s.Cached+s.SelfSkipped)/float64(s.Total())*100))
}

// ErrorCode 是翻译错误分类。
//
// 必须与 V1 translator.py:_format_error 的分类逐条对齐，前端展示文案不变。
type ErrorCode string

const (
	ErrNetwork          ErrorCode = "NETWORK"
	ErrTimeout          ErrorCode = "TIMEOUT"
	ErrAuthFailed       ErrorCode = "AUTH_FAILED"
	ErrForbidden        ErrorCode = "FORBIDDEN"
	ErrRateLimited      ErrorCode = "RATE_LIMITED"
	ErrServerError      ErrorCode = "SERVER_ERROR"
	ErrHTTPError        ErrorCode = "HTTP_ERROR"
	ErrBadResponse      ErrorCode = "BAD_RESPONSE"
	ErrTranslateFailed  ErrorCode = "TRANSLATE_FAILED"
	ErrAllProvidersFail ErrorCode = "ALL_PROVIDERS_FAILED"
)

// Message 返回与 V1 逐字一致的错误文案。
//
// detail 用于 HTTP_ERROR（reason phrase）与 TRANSLATE_FAILED（原始异常文本），
// 其余错误码忽略 detail。
func (c ErrorCode) Message(detail string) string {
	switch c {
	case ErrNetwork:
		return "[网络错误] 无法连接到 API 服务器，请检查地址和网络"
	case ErrTimeout:
		return "[请求超时] API 服务器响应超时，请稍后重试"
	case ErrAuthFailed:
		return "[认证失败] API 密钥无效，请检查设置"
	case ErrForbidden:
		return "[权限不足] 无权访问该 API，请检查密钥权限"
	case ErrRateLimited:
		return "[请求过于频繁] 请稍后重试"
	case ErrServerError:
		// V1 原文案带状态码：f"[服务器错误 {code}] API 服务器异常，请稍后重试"
		return "[服务器错误 " + detail + "] API 服务器异常，请稍后重试"
	case ErrHTTPError:
		// V1 原文案：f"[HTTP 错误 {code}] {exc.response.reason_phrase}"
		return "[HTTP 错误 " + detail + "]"
	case ErrBadResponse:
		return "[响应格式错误] API 返回了意外的数据结构"
	case ErrAllProvidersFail:
		// V1 里「所有 Provider 都失败」是从 `_call_api_internal` 抛出的
		// `Exception("所有 Provider 翻译失败")`，由 `_flush_llm` 的 except 接住后
		// 走 `_format_error` 的**兜底分支** → `f"[翻译失败] {exc}"`。
		// 所以它在界面上是带 `[` 前缀的。
		//
		// ⚠️ 这个前缀不是装饰：渲染层靠它判断「这条是不是错误」——
		// overlay.py:667 用它选错误样式，:673 用它决定**隐藏原文行**
		// （出错时原文没有参考价值）。少了前缀，两条行为一起失效，
		// 用户看到的错误与正常译文长得一模一样。
		return "[翻译失败] 所有 Provider 发送翻译失败"
	default:
		if detail == "" {
			return "[翻译失败]"
		}
		return "[翻译失败] " + detail
	}
}
