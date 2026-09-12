// 广告发送的控制端点。
//
// 状态机本体在 internal/task（不在前端）——见该包的文件头说明：
// V1 把状态机放在设置窗口里，导致关掉窗口发送就停了。V2 搬到 Go 侧后，
// 界面只是控制面板，关掉窗口发送照常继续。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/task"
)

// AdController 是广告发送能力（由 task.AdSender 实现）。
type AdController interface {
	Start() error
	Stop()
	Status() task.Status
	Configure(messages []string, intervalMin int, hotkey string)
}

// handleAdStatus 返回当前状态快照。
//
// 前端主要靠 SSE 的 ad.status 事件拿实时状态，这个端点用于首次加载补快照
// （与 /api/stats 同一套思路：SSE 断了也能重新对齐）。
func (s *Server) handleAdStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Ad == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "广告发送未就绪")
		return
	}
	writeOK(w, s.opts.Ad.Status())
}

// handleAdStart 启动循环发送。
func (s *Server) handleAdStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Ad == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "广告发送未就绪")
		return
	}

	// 启动前把参数推给状态机。
	//
	// ⚠️ 优先用**请求体里的**值，而不是服务端已保存的配置。
	//
	// 原因：V1 的 `_ad_start` 直接读设置窗口里的 Entry 控件，所以「改完消息
	// 不点保存、直接点开始」用的就是屏幕上那份内容。V2 的配置必须显式点保存
	// 才落库，这里若只读已保存的配置，同一个操作就会**静默地发送上一次保存的
	// 旧消息**——界面显示的是用户刚写的文本，发出去的却是旧的，
	// 这种错最难被发现。
	//
	// 请求体是可选的全量覆盖：给了 messages 就用它，没给（或字段缺失）才
	// 回落到已保存的配置。
	var body struct {
		Messages []string `json:"messages"`
		// ⚠️ 倒计时用 json.RawMessage 接，不能声明成 string。
		//
		// 前端的倒计时输入框是 `<input type="number">`，而 Vue 对 number 类型
		// 的 v-model **会自动把值转成数字**，于是请求体里是 `"countdown": 5`
		// 而不是 `"countdown": "5"`。声明成 string 的话，encoding/json 会在
		// 这个字段上直接报错——**整包解码失败**，连已经解出来的 messages 一起丢掉。
		//
		// 实测后果（就是用户报的那一个）：用户填了五条消息、倒计时也填了，
		// 后端却因为这一个字段的类型不符放弃整包、转而读一份空的已保存配置，
		// 最后回「[翻译失败] 请至少填写一条广告消息」。错误文案把用户和排查的
		// 人都指向了完全相反的方向，来回查了好几轮。
		//
		// 兼容两种写法（数字或字符串）比强迫前端改更稳：这个端点将来还会有
		// 别的调用方，而"倒计时是 5 还是 \"5\""不该是个能搞垮整次请求的问题。
		Countdown json.RawMessage `json:"countdown"`
	}
	decodeErr := json.NewDecoder(r.Body).Decode(&body)

	// 把「到底收到了什么」记下来。
	//
	// 不要因为"日志太啰嗦"删掉：这段代码有**两条语义完全不同的分支**
	// （用请求体 / 回落到已保存的配置），而两条最后都可能走到同一句错误文案上。
	// 正是这行日志一次就定位到了上面那个类型不匹配——没有它就只能靠猜。
	if s.opts.Log != nil {
		nonEmpty := 0
		for _, m := range body.Messages {
			if strings.TrimSpace(m) != "" {
				nonEmpty++
			}
		}
		s.opts.Log.Info("AD", fmt.Sprintf(
			"ad/start 收到: decode错误=%v 字段数=%d 非空数=%d 倒计时=%q",
			decodeErr, len(body.Messages), nonEmpty, countdownOf(body.Countdown)))
	}

	switch {
	case decodeErr == nil:
		// 正常路径
	case errors.Is(decodeErr, io.EOF):
		// 完全没有请求体（curl、或将来不带参数的调用方）→ 回落到已保存的配置。
		// 这是上面那段注释原本想要的语义。
		s.syncAdFromConfig()
		if err := s.opts.Ad.Start(); err != nil {
			writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, err.Error())
			return
		}
		writeOK(w, s.opts.Ad.Status())
		return
	default:
		// ⚠️ 请求体存在但解不开 → **明确报错，绝不静默回落**。
		// 静默回落正是上面那个缺陷的放大器：一个字段类型不符，用户得到的是
		// 一句「你没有填写广告消息」，而他明明填了。
		writeErr(w, http.StatusBadRequest, domain.ErrBadResponse,
			"请求体解析失败: "+decodeErr.Error())
		return
	}

	if body.Messages != nil {
		cfg := s.Config()
		// 倒计时留空时传 0，Configure 会忽略它并保留已配置的间隔——
		// 不能拿 parseCountdownMinutes("") 的兜底值 5 去覆盖用户设的 30。
		interval := 0
		if txt := countdownOf(body.Countdown); strings.TrimSpace(txt) != "" {
			interval = parseCountdownMinutes(txt)
		}
		s.opts.Ad.Configure(body.Messages, interval, cfg.ChatHotkey)

		// 把这次用的内容**存进配置**。
		//
		// V1 就是这样：广告消息与倒计时本来就是配置的一部分，重启后还在。
		// V2 原先只在请求体里传一次、不落库，于是用户每次重开程序都要**重新
		// 填五条消息**——而他会以为是自己忘了点保存（其实点了也一样，
		// 因为要保存的是「界面上改过的配置」，而不是「这次发送用的内容」）。
		//
		// 在**开始发送**这一刻落库，而不是在输入框失焦时：那是用户明确表达
		// 「这就是我要用的内容」的时刻，语义最清楚。
		s.persistAdSettings(body.Messages, interval, countdownOf(body.Countdown))
	} else {
		s.syncAdFromConfig()
	}

	if err := s.opts.Ad.Start(); err != nil {
		// 校验失败（没填消息 / 间隔非法 / 热键解析不了）走 400：
		// 这是用户输入问题，不是服务器故障。
		writeErr(w, http.StatusBadRequest, domain.ErrTranslateFailed, err.Error())
		return
	}
	writeOK(w, s.opts.Ad.Status())
}

// handleAdStop 停止循环发送（幂等）。
func (s *Server) handleAdStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}
	if s.opts.Ad == nil {
		writeErr(w, http.StatusServiceUnavailable, domain.ErrTranslateFailed, "广告发送未就绪")
		return
	}
	s.opts.Ad.Stop()
	writeOK(w, s.opts.Ad.Status())
}

// syncAdFromConfig 把配置里的广告设置推给状态机。
func (s *Server) syncAdFromConfig() {
	if s.opts.Ad == nil {
		return
	}
	cfg := s.Config()
	interval := parseCountdownMinutes(cfg.AdCountdown)
	s.opts.Ad.Configure(cfg.AdMessages, interval, cfg.ChatHotkey)
}

// persistAdSettings 把这次发送用的广告设置写进配置并落盘。
//
// 失败只记日志、不影响本次发送：内容已经推给状态机了，这次能正常发；
// 存不下来只意味着下次要重填，不该因此打断用户。
func (s *Server) persistAdSettings(messages []string, intervalMin int, countdown string) {
	s.UpdateConfig(func(c *config.Config) {
		c.AdMessages = append([]string(nil), messages...)
		switch {
		case countdown != "":
			c.AdCountdown = countdown
		case intervalMin > 0:
			// 请求体只给了数字、没给原文（或给的是空串）时，用解析出来的分钟数回填，
			// 否则下次启动倒计时会变回默认值，用户设的 30 分钟就丢了。
			c.AdCountdown = strconv.Itoa(intervalMin)
		}
	})

	// 落盘必须在 UpdateConfig **之外**：那个回调持着 s.mu，
	// 在里面做磁盘 I/O 会把 GET /api/config 一起卡住。
	if err := config.Save(s.Config()); err != nil {
		if s.opts.Log != nil {
			s.opts.Log.Warn("AD", "保存广告设置失败（本次发送不受影响）: "+err.Error())
		}
	}
}

// countdownOf 从请求体里的 countdown 取出文本形式。
//
// 两种写法都要认：前端的倒计时输入框是 `<input type="number">`，Vue 的 v-model
// 会自动把它转成**数字**，所以线上发过来的是 `5` 而不是 `"5"`。
// 用 RawMessage 接着再由这里归一，比强迫前端改更稳——这个端点将来还会有
// 别的调用方，而"倒计时是 5 还是 \"5\""不该是个能搞垮整次请求的问题。
func countdownOf(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	return strings.Trim(s, `"`)
}

// parseCountdownMinutes 解析倒计时配置。
//
// V1 的 `_ad_validate_countdown` 要求是 1..1440 的整数；配置里存的是字符串，
// 所以这里要容错：解析不了就回落到 5 分钟（V1 的默认值），
// 而不是让 0 或负数流进状态机。
func parseCountdownMinutes(raw string) int {
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
