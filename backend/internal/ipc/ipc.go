// Package ipc Named Pipe 客户端与帧编解码（ADR-004）。
//
// 帧格式（与 native/src/pipe.cpp 严格一致）：
//
//	+----------------+------------------------------+
//	| 4 bytes LE     | N bytes                      |
//	| payload length | UTF-8 JSON payload           |
//	+----------------+------------------------------+
//
// 不变量：
//   - 帧长度上限 8MB，超限即断开（防止畸形长度导致巨量分配）
//   - 请求/响应按 id 配对；无 id 或 event.* 的消息走事件通道
//   - 事件通道满时**丢弃**，绝不阻塞读循环（与 ingest 队列同策略）
//   - 单次调用超时不得阻塞主流程
package ipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// ProtocolVersion 必须与 native/include/translator_native.h 的 TN_PROTOCOL_VERSION 一致
	ProtocolVersion = 1

	// MaxFrameBytes 与 TN_MAX_FRAME_BYTES 一致
	MaxFrameBytes = 8 * 1024 * 1024

	// DefaultPipeName 与 TN_PIPE_NAME 一致
	DefaultPipeName = `\\.\pipe\ets2translator-native-v1`

	eventBuffer = 256
)

// Message 是 IPC 信封。
type Message struct {
	Type    string          `json:"type"`
	ID      int64           `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// DecodePayload 把 payload 反序列化到 out。
func (m Message) DecodePayload(out any) error {
	if len(m.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(m.Payload, out)
}

// ── 帧编解码 ──────────────────────────────────────────────────

// WriteFrame 写一帧（4 字节小端长度 + JSON）。
func WriteFrame(w io.Writer, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("帧长度 %d 超过上限 %d", len(body), MaxFrameBytes)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame 读一帧。返回 io.EOF 表示对端正常关闭。
func ReadFrame(r io.Reader) (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return Message{}, fmt.Errorf("帧长度 %d 超过上限 %d", n, MaxFrameBytes)
	}
	if n == 0 {
		return Message{}, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("帧内容不是合法 JSON: %w", err)
	}
	return m, nil
}

// ── 客户端 ────────────────────────────────────────────────────

// Client 是到 native 进程的连接。
type Client struct {
	conn io.ReadWriteCloser

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan Message

	events   chan Message
	readErr  error
	closed   chan struct{}
	closeOne sync.Once
}

// Dial 连接 native 进程。
//
// 管道不存在时 Windows 返回 ERROR_FILE_NOT_FOUND、对端繁忙返回 ERROR_PIPE_BUSY，
// 因此这里带重试直到 timeout——native 可能还在启动。
//
// ⚠️ 必须用 openPipe（overlapped）而不是 os.OpenFile：后者在 Windows 上走 overlapped
// I/O，与 native 侧的同步管道实例不匹配，会导致「连上了但一句话都说不通」。
// 详见 syncpipe.go 的文件头。
func Dial(pipeName string, timeout time.Duration) (*Client, error) {
	if pipeName == "" {
		pipeName = DefaultPipeName
	}
	deadline := time.Now().Add(timeout)

	var lastErr error
	for {
		conn, err := openPipe(pipeName)
		if err == nil {
			c := &Client{
				conn:    conn,
				pending: map[int64]chan Message{},
				events:  make(chan Message, eventBuffer),
				closed:  make(chan struct{}),
			}
			go c.readLoop()
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("连接管道 %s 超时（%.1fs）: %w",
				pipeName, timeout.Seconds(), lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readLoop 是唯一的读方：按 id 分派响应，其余走事件通道。
func (c *Client) readLoop() {
	defer close(c.events)
	for {
		m, err := ReadFrame(c.conn)
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			c.Close()
			return
		}

		if m.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			if ok {
				delete(c.pending, m.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- m
				continue
			}
			// 没人等这个 id：当作事件处理，不丢弃
		}

		// 事件：通道满则丢弃（绝不阻塞读循环）
		select {
		case c.events <- m:
		default:
		}
	}
}

// Events 返回事件通道（native 主动推送的消息）。通道随连接关闭而关闭。
func (c *Client) Events() <-chan Message { return c.events }

// ReadErr 返回读循环结束的原因（nil 表示尚未结束）。
func (c *Client) ReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// Call 发一条消息并等待同 id 的响应。
func (c *Client) Call(ctx context.Context, msgType string, payload any) (Message, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg := Message{Type: msgType, ID: id}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Message{}, err
		}
		msg.Payload = b
	}

	c.mu.Lock()
	err := WriteFrame(c.conn, msg)
	c.mu.Unlock()
	if err != nil {
		return Message{}, fmt.Errorf("发送 %s 失败: %w", msgType, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return Message{}, fmt.Errorf("连接已关闭（等待 %s 响应时）", msgType)
		}
		return resp, nil
	case <-ctx.Done():
		return Message{}, fmt.Errorf("等待 %s 响应超时: %w", msgType, ctx.Err())
	}
}

// Ping 发一次心跳并等待 pong。
func (c *Client) Ping(ctx context.Context, seq int64) error {
	resp, err := c.Call(ctx, "ping", map[string]int64{"seq": seq})
	if err != nil {
		return err
	}
	if resp.Type != "pong" {
		return fmt.Errorf("心跳响应类型异常: %s", resp.Type)
	}
	return nil
}

// Handshake 执行握手，返回 native 报告的能力清单与版本是否匹配。
func (c *Client) Handshake(ctx context.Context) (capabilities []string, versionMatch bool, err error) {
	resp, err := c.Call(ctx, "native.hello",
		map[string]any{"protocolVersion": ProtocolVersion})
	if err != nil {
		return nil, false, err
	}
	if resp.Type != "native.ready" {
		return nil, false, fmt.Errorf("握手响应类型异常: %s", resp.Type)
	}
	var p struct {
		ProtocolVersion int      `json:"protocolVersion"`
		Capabilities    []string `json:"capabilities"`
		VersionMatch    bool     `json:"versionMatch"`
	}
	if err := resp.DecodePayload(&p); err != nil {
		return nil, false, err
	}
	return p.Capabilities, p.VersionMatch, nil
}

// Close 关闭连接（幂等）。
func (c *Client) Close() error {
	var err error
	c.closeOne.Do(func() {
		close(c.closed)
		err = c.conn.Close()
	})
	return err
}
