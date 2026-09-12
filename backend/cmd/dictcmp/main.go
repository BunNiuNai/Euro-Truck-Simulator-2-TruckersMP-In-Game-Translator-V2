// dictcmp 是开发期对拍工具：读取「每行一条消息」的文件，
// 输出 Go 侧本地词典的判定结果，用于与 V1（Python）逐条比对。
//
// 这不是产品代码，不参与发布；放在 cmd/ 下是为了能直接用 go run 调用。
//
// 输出格式（TSV，与 tools/fidelity/dict_fidelity.py 约定一致）：
//
//	<序号>\t<是否非文字>\t<是否命中>\t<译文>
//
// 用法：
//
//	go run ./cmd/dictcmp <messages.txt> [resourcesDir]
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/engine"
)

// placeholderLLM 用固定占位符模拟 LLM，保证对拍两侧的文本与状态完全可比。
type placeholderLLM struct{}

func (placeholderLLM) Call(_ context.Context, _ string) (string, string, string, error) {
	return "〔LLM待接入〕", "STUB", "", nil
}

func main() {
	pipeline := flag.Bool("pipeline", false, "走完整管线（含跳过/拆分/后处理），输出状态+译文")
	flag.Parse()
	if len(flag.Args()) < 1 {
		fmt.Fprintln(os.Stderr, "用法: dictcmp [-pipeline] <messages.txt> [resourcesDir]")
		os.Exit(2)
	}
	msgPath := flag.Arg(0)

	resDir := "resources"
	if len(flag.Args()) >= 2 {
		resDir = flag.Arg(1)
	}
	dictPath := filepath.Join(resDir, "dictionary.json")

	dict, err := dictionary.Load(dictPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载词库失败: %v\n", err)
		os.Exit(1)
	}
	for _, w := range dict.Warnings {
		fmt.Fprintf(os.Stderr, "词库警告: %s\n", w)
	}

	f, err := os.Open(msgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开消息文件失败: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	tr := &engine.Translator{
		Target:         "zh-CN",
		BatchSeparator: "\n---\n",
		Dict:           dict,
		LLM:            placeholderLLM{},
	}

	i := 0
	for sc.Scan() {
		text := sc.Text()

		if *pipeline {
			msg := tr.Translate(context.Background(), &domain.Event{Text: text, Origin: "fidelity"})
			trans := strings.ReplaceAll(msg.Translated, "\t", " ")
			trans = strings.ReplaceAll(trans, "\n", " ")
			fmt.Fprintf(out, "%d\t%s\t%s\n", i, msg.CacheState, trans)
			i++
			continue
		}

		nonTrans := dictionary.NonTranslatable(text)
		trans, hit := dict.Lookup(text)
		trans = strings.ReplaceAll(trans, "\t", " ")
		trans = strings.ReplaceAll(trans, "\n", " ")
		fmt.Fprintf(out, "%d\t%t\t%t\t%s\n", i, nonTrans, hit, trans)
		i++
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dictcmp: 已处理 %d 条消息（pipeline=%v）\n", i, *pipeline)
}
