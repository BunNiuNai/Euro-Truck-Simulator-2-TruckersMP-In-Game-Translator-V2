// 消息快照：让「刚连上来的前端」能把当前消息列表一次性拉回去。
//
// 为什么需要它——SSE 的两条边界都盖不住这个场景：
//   · `id:` + Last-Event-ID 补发（events.go）能补**断线期间**的帧，
//     但补不了「页面整个重新加载」：那时 Last-Event-ID 已经没了，
//     浏览器并不知道自己之前收到过什么。
//   · 前端的消息列表在 JS 内存里（useMessages.ts），刷新即清空。
//
// 而 V1 的悬浮窗消息列表是**进程内长期持有**的（overlay.py 的 self._messages），
// 刷新界面这个动作在 V1 里根本不存在。V2 把列表放在前端，就必须补一个
// 「从服务端重新取回来」的入口——否则用户按一次 F5（或 WebView 因故重载），
// 聊天记录就凭空清空了，而且再也拿不回来。
package api

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

const (
	// messageRingSize 是快照保留的消息条数。
	//
	// 比 V1 的 max_messages（默认 100，界面最多显示这么多）留一倍余量：
	// 前端可能同时展示「当前列表」并允许上翻，取 200 让它在
	// 配置调大 max_messages 时也不至于立刻取空。
	messageRingSize = 200

	// defaultMessageLimit 是 GET /api/messages 不带 limit 时返回的条数。
	defaultMessageLimit = 100
)

// messageRing 是最近消息的环形缓冲。
type messageRing struct {
	mu   sync.Mutex
	msgs []domain.RenderedMessage
}

func newMessageRing() *messageRing {
	return &messageRing{msgs: make([]domain.RenderedMessage, 0, messageRingSize)}
}

func (r *messageRing) push(m domain.RenderedMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, m)
	if len(r.msgs) > messageRingSize {
		// 一次裁掉一批：每帧裁一个会让底层数组一直往后爬，
		// 保留段永远占着前面已经没用的空间。
		r.msgs = append([]domain.RenderedMessage(nil), r.msgs[len(r.msgs)-messageRingSize:]...)
	}
}

// recent 返回最近 limit 条，按**时间正序**（老的在前）。
//
// 顺序很关键：前端是 append 到列表尾部的，倒序发过去会让整段历史
// 在界面上倒着长出来。
func (r *messageRing) recent(limit int) []domain.RenderedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(r.msgs)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]domain.RenderedMessage, n)
	copy(out, r.msgs[len(r.msgs)-n:])
	return out
}

func (s *Server) pushMessage(m domain.RenderedMessage) {
	if s.msgs != nil {
		s.msgs.push(m)
	}
}

func (s *Server) snapshotMessages(limit int) []domain.RenderedMessage {
	if s.msgs == nil {
		return nil
	}
	return s.msgs.recent(limit)
}

// handleMessages 返回最近的消息快照。
//
// 前端在**每次 SSE 连接建立**（含重连）与页面加载时调一次，
// 用来把断线/刷新期间丢掉的消息补齐。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, domain.ErrHTTPError, "405 Method Not Allowed")
		return
	}

	limit := defaultMessageLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		// 解析失败就用默认值：这是「多要几条历史」的辅助参数，
		// 为它返回 400 只会让前端因为一个笔误整块拿不到数据。
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	msgs := s.snapshotMessages(limit)
	if msgs == nil {
		msgs = []domain.RenderedMessage{}
	}
	writeOK(w, map[string]any{
		"messages": msgs,
		"count":    len(msgs),
	})
}
