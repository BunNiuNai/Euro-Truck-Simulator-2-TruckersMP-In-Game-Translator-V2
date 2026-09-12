// Command nativeprobe 手工驱动 native 进程的协议，用于联调与排障。
//
// 它按固定顺序走一遍真实链路：握手 → 建窗 → 隐藏/显示 → 切毛玻璃 →
// 点击穿透 → 改尺寸 → 心跳 → 重放 → 关闭。每一步都打印 native 回报的
// **实际状态**，而不是「我以为它会怎样」。
//
// 用法：
//
//	go run ./cmd/nativeprobe                        # 默认管道，走完整流程
//	go run ./cmd/nativeprobe -pipe \\.\pipe\tn-x    # 指定管道
//	go run ./cmd/nativeprobe -hold 3s               # 窗口多留几秒，肉眼确认
//	go run ./cmd/nativeprobe -quiet                  # 只打印失败项
//
// 退出码 0 表示全流程无错。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/ipc"
)

type probe struct {
	client *ipc.Client
	quiet  bool
	fails  []string
	steps  int
}

func (p *probe) call(what, msgType string, payload any) map[string]any {
	p.steps++
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	resp, err := p.client.Call(ctx, msgType, payload)
	if err != nil {
		p.fail(what, "调用失败: "+err.Error())
		return nil
	}

	var body map[string]any
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, &body); err != nil {
			p.fail(what, "响应不是 JSON 对象: "+err.Error())
			return nil
		}
	}

	// event.error 是 native 明确的失败回报，不能当成成功
	if resp.Type == "event.error" {
		code, _ := body["code"].(string)
		msg, _ := body["message"].(string)
		p.fail(what, fmt.Sprintf("native 回报错误 [%s] %s", code, msg))
		return body
	}
	if ok, has := body["ok"]; has {
		if b, _ := ok.(bool); !b {
			p.fail(what, fmt.Sprintf("ok=false: %v", body["message"]))
			return body
		}
	}

	if !p.quiet {
		raw, _ := json.Marshal(body)
		fmt.Printf("  ✓ %-22s %s -> %s\n", what, msgType, string(raw))
	}
	return body
}

func (p *probe) fail(what, reason string) {
	p.fails = append(p.fails, what+": "+reason)
	fmt.Printf("  ✗ %-22s %s\n", what, reason)
}

func (p *probe) expect(what string, body map[string]any, key string, want any) {
	if body == nil {
		return
	}
	p.steps++
	got := body[key]
	if fmt.Sprint(got) != fmt.Sprint(want) {
		p.fail(what, fmt.Sprintf("字段 %s = %v，期望 %v", key, got, want))
		return
	}
	if !p.quiet {
		fmt.Printf("  ✓ %-22s %s = %v\n", what, key, got)
	}
}

// expectNested 读取 body[section][key]。
func (p *probe) expectNested(what string, body map[string]any, section, key string, want any) {
	if body == nil {
		return
	}
	sub, ok := body[section].(map[string]any)
	if !ok {
		p.fail(what, "响应缺少 "+section+" 段")
		return
	}
	p.expect(what, sub, key, want)
}

func main() {
	pipe := flag.String("pipe", ipc.DefaultPipeName, "管道名")
	timeout := flag.Duration("timeout", 8*time.Second, "连接超时")
	hold := flag.Duration("hold", 6*time.Second, "窗口保持可见的时长")
	quiet := flag.Bool("quiet", false, "只打印失败项")
	sendShutdown := flag.Bool("shutdown", false, "结束时让 native 退出")
	flag.Parse()

	p := &probe{quiet: *quiet}

	client, err := ipc.Dial(*pipe, *timeout)
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	p.client = client
	defer client.Close()

	fmt.Println("=== native 协议联调 ===")

	// 1) 握手：拿到能力清单与 native 看到的实际状态
	hello := p.call("握手", "native.hello", map[string]any{"protocolVersion": ipc.ProtocolVersion})
	p.expect("握手", hello, "versionMatch", true)
	if hello != nil {
		caps, _ := hello["capabilities"].([]any)
		names := make([]string, 0, len(caps))
		for _, c := range caps {
			names = append(names, fmt.Sprint(c))
		}
		fmt.Printf("  · 能力清单: %v\n", names)
	}

	// 2) 建窗：放在主屏中上部，便于肉眼确认
	spec := map[string]any{
		"x": 120, "y": 80, "width": 620, "height": 360,
		"topmost": true, "opacity": 0.85, "blur": "auto",
		"title": "ETS2 Translator V2 — 联调窗口", "visible": true,
	}
	created := p.call("创建窗口", "display.create", spec)
	p.expectNested("创建窗口", created, "display", "exists", true)
	p.expectNested("创建窗口", created, "display", "visible", true)
	p.expectNested("创建窗口", created, "display", "width", 620)

	// 3) 隐藏、再显示
	hidden := p.call("隐藏窗口", "display.visible", map[string]any{"visible": false})
	p.expectNested("隐藏窗口", hidden, "display", "visible", false)
	shown := p.call("显示窗口", "display.visible", map[string]any{"visible": true})
	p.expectNested("显示窗口", shown, "display", "visible", true)

	// 4) 毛玻璃：显式要求 mica，回报里带着**实际**生效的模式
	blur := p.call("切换毛玻璃", "window.blur", map[string]any{"mode": "mica"})
	if blur != nil {
		if d, ok := blur["display"].(map[string]any); ok {
			fmt.Printf("  · 毛玻璃实际模式: %v\n", d["blur"])
		}
	}

	// 5) 点击穿透：开、读、关
	ctOn := p.call("开启穿透", "display.setClickThrough", map[string]any{"enabled": true})
	p.expectNested("开启穿透", ctOn, "display", "clickThrough", true)
	ctOff := p.call("关闭穿透", "display.setClickThrough", map[string]any{"enabled": false})
	p.expectNested("关闭穿透", ctOff, "display", "clickThrough", false)

	// 6) 改尺寸，验证期望状态能被完整重放
	resized := p.call("重建窗口", "display.create", map[string]any{
		"x": 200, "y": 140, "width": 800, "height": 420,
		"blur": "none", "opacity": 1.0, "visible": true,
	})
	p.expectNested("重建窗口", resized, "display", "width", 800)
	p.expectNested("重建窗口", resized, "display", "height", 420)

	// 7) 重放（ADR-012）：Go 重连后重建全部期望状态
	replay := p.call("重放期望状态", "native.replay", map[string]any{
		"registry": map[string]any{
			"display": map[string]any{
				"x": 60, "y": 60, "width": 640, "height": 300,
				"blur": "auto", "visible": true,
			},
			"hotkeys": []any{map[string]any{"id": "toggle", "combo": "ctrl+alt+t"}},
		},
	})
	if replay != nil {
		applied, _ := replay["applied"].([]any)
		skipped, _ := replay["skipped"].([]any)
		fmt.Printf("  · 重放已应用: %v / 已跳过: %v\n", applied, skipped)
		if len(applied) == 0 {
			p.fail("重放期望状态", "display 段没有被应用")
		}
	}

	// 8) 心跳：连续 3 次，验证应答序号回带正确
	pingOK := true
	for i := 1; i <= 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := p.client.Ping(ctx, int64(i))
		cancel()
		if err != nil {
			p.fail(fmt.Sprintf("心跳 #%d", i), err.Error())
			pingOK = false
			break
		}
	}
	if pingOK && !p.quiet {
		fmt.Println("  ✓ 心跳                   3/3 应答正常")
	}

	// 9) 未知能力必须回报错误，而不是静默假装成功
	//
	// ⚠️ 这里以前拿 hotkey.set 当「未实现」的例子——它早就实现了，探针会
	//    一直报假失败。现在协议里已没有未实现的命令，所以只能用一个真正
	//    未知的类型来验证这条不变量。
	unknown := p.call("未知能力回报", "totally.unknown", map[string]any{})
	p.expect("未知能力回报", unknown, "code", "UNSUPPORTED")

	// 留出时间让人肉眼看一眼窗口
	if *hold > 0 {
		fmt.Printf("  · 窗口保持 %.0f 秒，可肉眼确认拖动/缩放\n", hold.Seconds())
		time.Sleep(*hold)
	}

	// 收尾：不留窗口
	p.call("销毁窗口", "display.visible", map[string]any{"visible": false})

	if *sendShutdown {
		p.call("关闭 native", "shutdown", nil)
		// native 会先回包再退出，给它一点时间把回复写出来
		time.Sleep(300 * time.Millisecond)
	}

	fmt.Printf("\n共 %d 项检查，失败 %d 项\n", p.steps, len(p.fails))
	for _, f := range p.fails {
		fmt.Println("  -", f)
	}
	if len(p.fails) > 0 {
		os.Exit(1)
	}
	fmt.Println("=== 全流程通过 ===")
}
