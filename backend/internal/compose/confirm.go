// 发送确认：直接读聊天日志**文件**，看自己刚发的那条有没有出现。
//
// ⚠️ 为什么不复用 `ingest/chatlog` 那条流水线：它是一个 **Source**，
// 消息取走就没了。拿它来确认会把消息从翻译器手里抢走，用户就会觉得
// 「我自己发的消息怎么没被翻译」。V1 `compose_sender.py` 的模块文档专门
// 写了这一点，V2 沿用同样的做法——另开一个只读句柄，从文件**当前末尾**
// 往后读，与任何人都不冲突。
package compose

import (
	"bytes"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ingest/chatlog"
)

// ChatLogConfirmer 通过读聊天日志确认消息是否送达。
type ChatLogConfirmer struct {
	// Dir 是 ETS2 聊天日志所在目录。
	Dir string
	// SelfName 返回当前玩家名。**每次调用都重新取**，因为玩家名可以在设置里改。
	// 返回空串表示没配置——那时不校验说话人（见 Wait 里的说明）。
	SelfName func() string
	// Poll 是没有新行时的轮询间隔，0 表示用 DefaultPoll。
	Poll time.Duration
}

// Wait 等到 text 出现在聊天日志里为止，最多等 timeout。
//
// 只认**这次调用开始之后**写入的行：先把句柄定位到文件末尾，再看后续内容。
// 否则历史上任何一条相同文本都会让确认立刻「成功」。
func (c *ChatLogConfirmer) Wait(text string, timeout time.Duration) bool {
	normalized := normalize(text)
	if normalized == "" {
		return false
	}

	path := chatlog.FindLatestLog(c.Dir)
	if path == "" {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return false
	}

	poll := c.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	// pending 保存尚未收到换行的尾部：一次 Read 很可能正好把一行切成两半，
	// 直接当成完整行去解析会匹配失败，而且那一半数据已经被消费掉了。
	var pending []byte

	for time.Now().Before(deadline) {
		n, readErr := f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := string(pending[:idx])
				pending = pending[idx+1:]
				if c.matches(line, normalized) {
					return true
				}
			}
			// 有新数据就立刻继续读，不睡——一次读取可能拿到好几行。
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return false
		}
		time.Sleep(poll)
	}
	return false
}

// matches 判断一行聊天日志是不是我们要找的那条。
func (c *ChatLogConfirmer) matches(line, normalized string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}

	selfName := ""
	if c.SelfName != nil {
		selfName = strings.TrimSpace(c.SelfName())
	}

	ev := chatlog.ParseLine(line, selfName)
	if ev == nil {
		return false
	}
	if normalize(ev.Text) != normalized {
		return false
	}

	// 只在**配置了玩家名**时才校验说话人：没配置就无从判断是谁发的，
	// 硬要求匹配会让确认永远失败（V1 compose_sender.py:171 的处理相同）。
	//
	// 校验的意义是排除误报：别人正好说了同一句话时不该算作自己发送成功。
	if selfName == "" {
		return true
	}
	return ev.IsSelf
}
