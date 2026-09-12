// SSE 事件推送与订阅中心。
//
// 与架构文档 v1.3 的差异见 ADR-016：用 SSE 而不是 WebSocket。
// SSE 是纯 HTTP 长连接：`data: <json>\n\n`，浏览器用 EventSource 消费并自动重连。
//
// ⚠️ 「EventSource 自己会重连」这句话只说对了一半——**重连不会补发断线期间
// 产生的事件**。前端为此在每个 onOpen 之后重新拉一轮快照（见 useEvents.ts），
// 但那只覆盖 health/config/stats 这类「有当前值」的状态，
// **补不了消息**：断线那几秒进来的聊天消息就此永久消失，界面上没有任何痕迹。
// 所以这里实现了 SSE 标准的 `id:` + `Last-Event-ID` 补发（见 hub.replayAfter）：
// 每个订阅者缓冲满时会被丢弃（不能因为前端慢就拖住主流程），
// 但客户端重连时能凭最后收到的事件号把缺的帧拿回来。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// event 是推送信封，与架构文档第 9.2 节的 `{type, payload}` 一致。
//
// ID 不上 JSON：它是 SSE 帧头里的 `id:` 字段（浏览器把它作为
// Last-Event-ID 在重连时原样送回），不是载荷的一部分。
// 放进 payload 会让前端的 Envelope 类型平白多一个字段，
// 而且两处各存一份同一个号迟早会不一致。
type event struct {
	ID      int64  `json:"-"`
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

const (
	// 每个订阅者的缓冲；满了就丢（与 ingest 队列同策略：不能因为前端慢而拖住主流程）
	subBuffer = 256

	// SSE 心跳间隔：防止中间层或系统把空闲长连接掐掉
	keepAliveInterval = 15 * time.Second

	// replayBuffer 是保留的最近事件数，供客户端重连后补发。
	//
	// 取 512（订阅者缓冲 256 的两倍）：一次短暂断线（几秒）里产生的消息
	// 通常只有几十条，512 帧足够覆盖；再大也没意义——
	// 真要断很久，补发几百条历史消息反而会把界面刷满，
	// 那时更合理的是让用户看到「已重新连接」并自己去翻日志。
	replayBuffer = 512

	// dropWarnInterval 是「丢弃事件」告警的最小间隔。
	//
	// 丢弃会成片发生（前端卡住的瞬间每帧都丢），逐帧记一条日志会把
	// 日志本身淹掉——那是排障时最不想看到的结果。这里按时间限流，
	// 并在一条日志里带上累计条数。
	dropWarnInterval = 10 * time.Second
)

type hub struct {
	mu     sync.Mutex
	subs   map[int]chan event
	nextID int

	// seq 是全局事件序号，随每帧递增；recent 保留最近 replayBuffer 帧。
	seq    int64
	recent []event

	// drops 记录每个订阅者因缓冲满而丢掉的帧数；
	// lastDropWarn 是上一次为此订阅者记告警的时间（限流用）。
	drops        map[int]int
	lastDropWarn map[int]time.Time
}

func newHub() *hub {
	return &hub{
		subs:         map[int]chan event{},
		drops:        map[int]int{},
		lastDropWarn: map[int]time.Time{},
	}
}

func (h *hub) subscribe() (int, chan event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan event, subBuffer)
	h.subs[id] = ch
	return id, ch
}

func (h *hub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, id)
	// 顺手清掉丢弃计数：不清的话，反复重连会在这两个 map 里
	// 留下越来越多的死条目（键是自增的 id，永远不会被复用）。
	delete(h.drops, id)
	delete(h.lastDropWarn, id)
}

// publish 非阻塞推送：订阅者缓冲满时丢弃该事件，绝不阻塞调用方。
func (h *hub) publish(ev event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	ev.ID = h.seq
	h.recent = append(h.recent, ev)
	if len(h.recent) > replayBuffer {
		// 一次裁掉一批而不是每帧裁一个：每帧都重切片会让底层数组
		// 一直往后爬，保留的那部分永远占着前面已经没用的空间。
		h.recent = append([]event(nil), h.recent[len(h.recent)-replayBuffer:]...)
	}

	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			h.drops[id]++
		}
	}
}

// replayAfter 返回「序号大于 after」的最近事件，供重连补发。
//
// after <= 0（客户端没带 Last-Event-ID，或带了非法值）时不补发：
// 那种情况说明这是一次全新连接，把几百条历史事件一次性砸过去
// 只会让界面瞬间刷满，而客户端本来就会自己拉一轮快照。
func (h *hub) replayAfter(after int64) []event {
	if after <= 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// recent 是按序号递增的，二分找出第一个 > after 的位置
	lo, hi := 0, len(h.recent)
	for lo < hi {
		mid := (lo + hi) / 2
		if h.recent[mid].ID <= after {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(h.recent) {
		return nil
	}
	out := make([]event, len(h.recent)-lo)
	copy(out, h.recent[lo:])
	return out
}

// takeGap 报告并清除「这个订阅者丢过帧」的标记，同时按需返回丢弃总数与
// 是否应当记一条告警（已限流）。
//
// 丢弃是很糟糕的状态：订阅者少收了几帧，而前端不知道自己漏了什么——
// 界面上会凭空少几条消息且没有任何提示。所以这里让 handler 在补上
// 空位之后主动发一帧 resync.required，让前端重新对齐。
func (h *hub) takeGap(id int) (dropped int, warn bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	d := h.drops[id]
	if d == 0 {
		return 0, false
	}
	h.drops[id] = 0

	now := time.Now()
	shouldWarn := now.Sub(h.lastDropWarn[id]) >= dropWarnInterval
	if shouldWarn {
		h.lastDropWarn[id] = now
	}
	return d, shouldWarn
}

func (h *hub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// handleEvents 是 SSE 端点。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "TRANSLATE_FAILED", "当前服务器不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id, ch := s.hub.subscribe()
	defer s.hub.unsubscribe(id)

	// 首帧：告知客户端连接已建立（前端可据此把状态从"连接中"切到"已连接"）
	writeSSE(w, event{Type: "hello", Payload: map[string]any{"sse": true}})
	flusher.Flush()

	if s.opts.Log != nil {
		s.opts.Log.Info("API", fmt.Sprintf("SSE 客户端已连接（当前 %d 个）", s.hub.subscriberCount()))
	}

	// 补发断线期间漏掉的帧。
	//
	// EventSource 重连时会自动带上 Last-Event-ID 头（值就是上一个收到的
	// 帧的 `id:`），这就是 SSE 标准里补发的做法，不需要我们自己记状态。
	// 也接受 ?lastEventId= 查询参数：便于用 curl 手工验证补发。
	lastID := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if lastID == 0 {
		lastID = parseLastEventID(r.URL.Query().Get("lastEventId"))
	}
	if missed := s.hub.replayAfter(lastID); len(missed) > 0 {
		for _, ev := range missed {
			if err := writeSSE(w, ev); err != nil {
				return
			}
		}
		flusher.Flush()
		if s.opts.Log != nil {
			s.opts.Log.Info("API", fmt.Sprintf("SSE 补发 %d 帧（自 #%d 之后）", len(missed), lastID))
		}
	}

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return // 客户端断开

		case ev := <-ch:
			// 先补一帧「请重新对齐」。丢帧意味着前端已经漏了内容，
			// 而它自己看不出来——必须明确告诉它去重拉快照。
			if dropped, warn := s.hub.takeGap(id); dropped > 0 {
				if warn && s.opts.Log != nil {
					s.opts.Log.Warn("API", fmt.Sprintf(
						"SSE 订阅者 #%d 缓冲满，已丢弃 %d 帧（前端可能卡顿或断点在此刻）",
						id, dropped))
				}
				if err := writeSSE(w, event{
					Type:    "resync.required",
					Payload: map[string]any{"dropped": dropped},
				}); err != nil {
					return
				}
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()

		case <-ticker.C:
			// SSE 注释行作为心跳，不影响客户端解析
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseLastEventID 解析 Last-Event-ID，非法或缺失返回 0。
func parseLastEventID(raw string) int64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// writeSSE 写一帧。JSON 会把内容里的换行转义，因此不会破坏 SSE 的帧结构。
//
// `id:` 必须写在 `data:` **前面**：SSE 规范里字段顺序不限，但浏览器是在
// 处理完整个事件块之后才更新 Last-Event-ID 的，先写 id 只是让手工
// `curl -N` 看输出时更容易对上号。
func writeSSE(w io.Writer, ev event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if ev.ID > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.ID); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// readFileIfExists 读取可选资源文件（不存在返回 nil, nil）。
func readFileIfExists(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}
