package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSSEOverflowEmitsResyncRequired 走**真实 HTTP**，造一个"连上但不读"的
// 订阅者，把 hub 的 subBuffer 帧缓冲撑爆，断言 handler **真的写出了一帧
// resync.required{dropped:N}**。
//
// 为什么单独写这一条：hub 级的丢弃记账已经被 TestHubCountsDropsAndReportsGap
// 覆盖了，但「记账 → 变成一帧写进 HTTP 响应」这段活在 events.go 的 handler
// 循环里（takeGap 之后那次 writeSSE），此前**没有任何测试跑过它**。
// implementation-status.md 的「可信度折扣清单」记的就是这一项：
// 「没造出真实的缓冲溢出帧（需要一个阻塞不读的消费端）」。
//
// 这不是 TDD 的红灯→绿灯（行为已经存在），而是**补一条把已有行为钉住的验证**。
func TestSSEOverflowEmitsResyncRequired(t *testing.T) {
	s, ts, _, _, _ := newTestServer(t)

	// ① 连上，但**不读响应体**——这就是"阻塞不读的消费端"。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("订阅应返回 200，实际 %d", resp.StatusCode)
	}

	// ② 等 handler 真正进入 select 循环（否则第一帧会落在订阅之前）
	waitForSubscribers(t, s, 1)

	// ③ 灌爆。两个要点：
	//    · 条数要远超 subBuffer——不够的话只是把通道填满，不会触发丢弃
	//    · **每帧要够大**：httpserver 的写会先进 OS 发送缓冲（通常 64 KB），
	//      载荷太小的话 handler 写得比我们灌得还快，通道永远填不满、
	//      也就永远测不到丢弃。这里每帧塞 4 KB，让发送缓冲迅速卡住。
	pad := strings.Repeat("x", 4096)
	const n = subBuffer * 4
	for i := 0; i < n; i++ {
		s.Publish("stats.updated", map[string]any{"pad": pad})
	}

	// ④ 开始读，找那一帧
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	found := false
	deadline := time.Now().Add(10 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.Contains(line, "resync.required") {
			found = true
			if !strings.Contains(line, "dropped") {
				t.Errorf("resync.required 应带 dropped 字段，实际: %s", line)
			}
			t.Logf("收到溢出帧: %s", line)
			break
		}
	}

	if !found {
		t.Fatalf("灌了 %d 帧（缓冲只有 %d）却没收 resync.required——"+
			"要么 handler 没把丢弃记账转成帧，要么量不够撑爆发送缓冲", n, subBuffer)
	}
}

// waitForSubscribers 轮询等待订阅者数达到 want，最多 3 秒。
//
// 为什么要等：Publish 是往 hub 的订阅者 map 里投递，订阅者还没注册时
// 灌进去的帧**不会进任何通道**，后面怎么灌都撑不爆。
func waitForSubscribers(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.SubscriberCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个订阅者超时（实际 %d）", want, s.SubscriberCount())
}
