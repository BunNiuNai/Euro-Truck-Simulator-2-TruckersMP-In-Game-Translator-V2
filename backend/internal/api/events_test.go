package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// ── 事件序号 ──────────────────────────────────────────────────

// 每帧都要带递增的 `id:`——它是 Last-Event-ID 补发的唯一依据。
// 序号不递增（或复用）会让补发悄悄错位：客户端以为拿到的是续集，其实是重播。
func TestHubAssignsMonotonicIDs(t *testing.T) {
	h := newHub()
	for i := 0; i < 5; i++ {
		h.publish(event{Type: "t"})
	}
	if len(h.recent) != 5 {
		t.Fatalf("应保留 5 帧，实际 %d", len(h.recent))
	}
	for i, ev := range h.recent {
		if ev.ID != int64(i+1) {
			t.Errorf("第 %d 帧的 ID 应为 %d，实际 %d", i+1, i+1, ev.ID)
		}
	}
}

// ── 补发 ──────────────────────────────────────────────────────

func TestHubReplayAfter(t *testing.T) {
	h := newHub()
	for i := 0; i < 10; i++ {
		h.publish(event{Type: "t"})
	}

	got := h.replayAfter(7)
	if len(got) != 3 {
		t.Fatalf("自 #7 之后应补发 3 帧（8/9/10），实际 %d 帧", len(got))
	}
	if got[0].ID != 8 || got[2].ID != 10 {
		t.Errorf("补发的序号不对：%d..%d", got[0].ID, got[2].ID)
	}

	// 全新连接（没有 Last-Event-ID，或值非法）**不**补发：
	// 把几十上百条历史一次性砸过去只会把界面瞬间刷满，
	// 而客户端本来就会自己拉一轮快照。
	for _, after := range []int64{0, -5} {
		if h.replayAfter(after) != nil {
			t.Errorf("after=%d 不该补发任何帧", after)
		}
	}

	// 已经是最新：没有可补的
	if h.replayAfter(10) != nil {
		t.Error("已收到最新帧时不该补发")
	}
}

// 环形缓冲必须有界：不封顶的话，常驻几天就会把内存吃光。
func TestHubReplayBufferBounded(t *testing.T) {
	h := newHub()
	total := replayBuffer + 50
	for i := 0; i < total; i++ {
		h.publish(event{Type: "t"})
	}

	if len(h.recent) != replayBuffer {
		t.Fatalf("缓冲应封顶在 %d 帧，实际 %d 帧", replayBuffer, len(h.recent))
	}
	// 保留的必须是**最新**的：序号 50+1 .. total
	if want := int64(total - replayBuffer + 1); h.recent[0].ID != want {
		t.Errorf("保留段的第一帧应是 #%d，实际 #%d", want, h.recent[0].ID)
	}
	if h.recent[len(h.recent)-1].ID != int64(total) {
		t.Errorf("最后一帧应是 #%d", total)
	}
}

// ── 丢弃 ──────────────────────────────────────────────────────

// 订阅者缓冲满时丢弃并**记账**。
//
// 丢弃本身是设计选择（不能因为前端慢就拖住翻译主流程），
// 但**默默丢弃不行**：前端会凭空少几条消息且自己看不出来。
// 所以必须留下可查询的痕迹，让 handler 补一帧 resync.required。
func TestHubCountsDropsAndReportsGap(t *testing.T) {
	h := newHub()
	id, ch := h.subscribe()

	const overflow = 7
	for i := 0; i < subBuffer+overflow; i++ {
		h.publish(event{Type: "t"})
	}

	if len(ch) != subBuffer {
		t.Fatalf("通道应恰好塞满 %d 帧，实际 %d", subBuffer, len(ch))
	}

	dropped, _ := h.takeGap(id)
	if dropped != overflow {
		t.Fatalf("应记下 %d 帧丢弃，实际 %d", overflow, dropped)
	}
	// 取一次就清零，否则 handler 每帧都会重复发 resync.required
	if again, _ := h.takeGap(id); again != 0 {
		t.Errorf("gap 取过之后应清零，实际 %d", again)
	}
}

// 反复重连不该让丢弃记账无限增长：键是自增的订阅者 id，永不复用，
// 不在退订时清掉的话，跑几天就会发现这两个 map 里有几万个死条目。
func TestHubUnsubscribeClearsDropBookkeeping(t *testing.T) {
	h := newHub()
	for i := 0; i < 20; i++ {
		id, _ := h.subscribe()
		for j := 0; j < subBuffer+1; j++ {
			h.publish(event{Type: "t"})
		}
		h.unsubscribe(id)
	}

	if len(h.drops) != 0 {
		t.Errorf("退订后 drops 应清空，实际还剩 %d 条", len(h.drops))
	}
	if len(h.lastDropWarn) != 0 {
		t.Errorf("退订后 lastDropWarn 应清空，实际还剩 %d 条", len(h.lastDropWarn))
	}
	if len(h.subs) != 0 {
		t.Errorf("退订后不该还有订阅者，实际 %d 个", len(h.subs))
	}
}

// ── SSE 帧格式 ────────────────────────────────────────────────

func TestWriteSSEIncludesID(t *testing.T) {
	var sb strings.Builder
	if err := writeSSE(&sb, event{ID: 42, Type: "t"}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "id: 42\n") {
		t.Errorf("帧应以 `id: 42` 开头，实际 %q", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("帧必须以空行结束，实际 %q", out)
	}

	// 没有序号的帧（hello、resync.required）不写 id 行：
	// 它们不该推进客户端的 Last-Event-ID，否则补发窗口会被它们带偏。
	sb.Reset()
	if err := writeSSE(&sb, event{Type: "hello"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "id:") {
		t.Errorf("无序号帧不该写 id 行，实际 %q", sb.String())
	}
}

func TestParseLastEventID(t *testing.T) {
	cases := map[string]int64{
		"":        0,
		"abc":     0,
		"0":       0,
		"-3":      0,
		"17":      17,
		"9999999": 9999999,
	}
	for in, want := range cases {
		if got := parseLastEventID(in); got != want {
			t.Errorf("parseLastEventID(%q) = %d，期望 %d", in, got, want)
		}
	}
}

// ── 消息快照 ──────────────────────────────────────────────────

// 快照必须是**时间正序**：前端是往列表尾部 append 的，
// 倒序发过去会让整段历史在界面上倒着长出来。
func TestMessageRingKeepsChronologicalOrder(t *testing.T) {
	r := newMessageRing()
	for i := 1; i <= 5; i++ {
		r.push(domain.RenderedMessage{ID: string(rune('0' + i))})
	}

	got := r.recent(3)
	if len(got) != 3 {
		t.Fatalf("要 3 条应给 3 条，实际 %d 条", len(got))
	}
	want := []string{"3", "4", "5"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("第 %d 条应是 %q，实际 %q", i+1, id, got[i].ID)
		}
	}
}

func TestMessageRingBounded(t *testing.T) {
	r := newMessageRing()
	total := messageRingSize + 30
	for i := 0; i < total; i++ {
		r.push(domain.RenderedMessage{ID: string(rune(i % 1000))})
	}
	if len(r.msgs) != messageRingSize {
		t.Fatalf("快照环应封顶在 %d 条，实际 %d 条", messageRingSize, len(r.msgs))
	}
	if len(r.recent(messageRingSize)) != messageRingSize {
		t.Errorf("取满额时条数不对")
	}
	// limit 超过存量时给全部，不能越界
	if n := len(r.recent(messageRingSize * 10)); n != messageRingSize {
		t.Errorf("limit 超出存量时应返回全部 %d 条，实际 %d 条", messageRingSize, n)
	}
}

// 只有 message.translated 进快照。别的类型（stats、ad.status、hello…）
// 进去只会把环撑满、把真正的消息挤掉。
func TestOnlyTranslatedMessagesEnterSnapshot(t *testing.T) {
	s := &Server{opts: Options{}, hub: newHub(), msgs: newMessageRing()}

	s.Publish("message.translated", domain.RenderedMessage{ID: "msg-one"})
	s.Publish("stats.updated", map[string]any{"translated": 1})
	s.Publish("ad.status", map[string]any{"running": false})
	s.Publish("message.translated", domain.RenderedMessage{ID: "msg-two"})

	got := s.snapshotMessages(10)
	if len(got) != 2 {
		t.Fatalf("快照里应只有 2 条消息，实际 %d 条", len(got))
	}
	if got[0].ID != "msg-one" || got[1].ID != "msg-two" {
		t.Errorf("快照内容或顺序不对：%q, %q", got[0].ID, got[1].ID)
	}
}

// 载荷类型不符时宁可不存，也不能存半个进去
// （例如调用方改成传指针，断言就会失败）。
func TestSnapshotIgnoresWrongPayloadType(t *testing.T) {
	s := &Server{opts: Options{}, hub: newHub(), msgs: newMessageRing()}

	s.Publish("message.translated", map[string]any{"id": "not-a-rendered-message"})

	if got := s.snapshotMessages(10); len(got) != 0 {
		t.Errorf("类型不符时不该入快照，实际 %d 条", len(got))
	}
}

func TestMessagesEndpointServesSnapshot(t *testing.T) {
	s := &Server{opts: Options{}, hub: newHub(), msgs: newMessageRing()}
	s.Publish("message.translated", domain.RenderedMessage{ID: "msg-one"})
	s.Publish("message.translated", domain.RenderedMessage{ID: "msg-two"})

	rec := httptest.NewRecorder()
	s.handleMessages(rec, httptest.NewRequest(http.MethodGet, "/api/messages?limit=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "msg-one") {
		t.Errorf("limit=1 不该返回更老的那条，实际 %s", body)
	}
	if !strings.Contains(body, "msg-two") {
		t.Errorf("limit=1 应返回最新那条，实际 %s", body)
	}

	// 方法不对要明确拒绝，不能静默返回空快照——
	// 那会让前端以为「确实没有消息」
	rec = httptest.NewRecorder()
	s.handleMessages(rec, httptest.NewRequest(http.MethodPost, "/api/messages", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST 应返回 405，实际 %d", rec.Code)
	}

	// limit 是辅助参数：传了乱七八糟的值就用默认值，不要 400
	rec = httptest.NewRecorder()
	s.handleMessages(rec, httptest.NewRequest(http.MethodGet, "/api/messages?limit=abc", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("limit 非法时应回落默认值而不是报错，实际 %d", rec.Code)
	}
}
