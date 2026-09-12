package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	dir := t.TempDir()
	l := New(dir)
	// t.Cleanup 是 LIFO：这里注册的 Close 会在 t.TempDir 的目录删除**之前**执行。
	// 这不是洁癖——Windows 上文件被占用就删不掉，漏了它测试会在清理阶段报
	// "The process cannot access the file because it is being used by another process"。
	t.Cleanup(l.Close)
	return l, dir
}

// 普通日志行格式必须与 V1 一致：`YYYY-MM-DD HH:MM:SS [tag] [LEVEL] message`
func TestLineFormat(t *testing.T) {
	l, dir := newTestLogger(t)
	l.Info("SYS", "翻译器启动")
	l.Warn("LLM", "Provider 冷却")
	l.Error("TMP", "日志目录不存在")
	l.Close()

	raw := readFirstLog(t, dir)
	lineRe := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[SYS\] \[INFO\] 翻译器启动$`)
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d 行: %q", len(lines), raw)
	}
	if !lineRe.MatchString(lines[0]) {
		t.Errorf("普通日志行格式不符:\n  %q\n  期望形如 %q", lines[0], lineRe.String())
	}
	if !strings.Contains(lines[1], "[LLM] [WARN]") {
		t.Errorf("WARN 行 = %q", lines[1])
	}
	if !strings.Contains(lines[2], "[TMP] [ERROR]") {
		t.Errorf("ERROR 行 = %q", lines[2])
	}
}

// 翻译日志格式必须与 V1 逐字一致：`时间 - 厂商-模型名 - 原文 - 译文`
func TestTranslationLogFormat(t *testing.T) {
	l, dir := newTestLogger(t)
	l.Translation("DeepSeek", "deepseek-chat", "hello", "你好")
	l.Close()

	raw := readFirstLog(t, dir)
	want := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} - DeepSeek-deepseek-chat - hello - 你好$`)
	if !want.MatchString(strings.TrimSpace(raw)) {
		t.Errorf("翻译日志格式不符:\n  %q\n  期望形如 %q", strings.TrimSpace(raw), want.String())
	}
}

func TestFileNameUsesDate(t *testing.T) {
	l, dir := newTestLogger(t)
	l.Info("SYS", "x")
	l.Close()

	want := filePrefix + time.Now().Format("2006-01-02") + fileSuffix
	if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
		t.Errorf("应生成 %s: %v", want, err)
	}
}

// 内存缓冲上限 500 行（V1 BUFFER_SIZE）。
func TestBufferBoundedAt500(t *testing.T) {
	l, _ := newTestLogger(t)
	for i := 0; i < 800; i++ {
		l.Info("T", fmt.Sprintf("line-%d", i))
	}
	got := l.Recent(0)
	if len(got) != BufferSize {
		t.Fatalf("缓冲长度 = %d，期望 %d", len(got), BufferSize)
	}
	// 保留的应是**最新的** 500 行
	if !strings.Contains(got[len(got)-1], "line-799") {
		t.Errorf("最后一行 = %q，期望 line-799", got[len(got)-1])
	}
	if strings.Contains(got[0], "line-0") {
		t.Errorf("最早的行应已被挤出，实际首行 = %q", got[0])
	}
}

func TestRecentLimits(t *testing.T) {
	l, _ := newTestLogger(t)
	for i := 0; i < 10; i++ {
		l.Info("T", fmt.Sprintf("l%d", i))
	}
	if got := l.Recent(3); len(got) != 3 || !strings.Contains(got[2], "l9") {
		t.Errorf("Recent(3) = %v", got)
	}
	if got := l.Recent(0); len(got) != 10 {
		t.Errorf("Recent(0) 应返回全部，得到 %d 行", len(got))
	}
	if got := l.Recent(1000); len(got) != 10 {
		t.Errorf("Recent(1000) 应返回全部 10 行，得到 %d 行", len(got))
	}
}

// 超过单文件上限时改名成 _1.log（V1 _rotate_if_needed）。
func TestRotateWhenExceedingMaxSize(t *testing.T) {
	l, _ := newTestLogger(t)
	l.maxSize = 200 // 测试用小上限

	for i := 0; i < 20; i++ {
		l.Info("T", strings.Repeat("x", 40)) // 每行约 70 字节
	}
	l.Close()

	files := l.Files()
	hasRotated := false
	for _, f := range files {
		if strings.Contains(f, "_1"+fileSuffix) {
			hasRotated = true
		}
	}
	if !hasRotated {
		t.Errorf("超过上限后应产生 _1.log，实际文件: %v", files)
	}
}

// 连续多次轮转应产生递增序号。
func TestRotateSequence(t *testing.T) {
	l, _ := newTestLogger(t)
	l.maxSize = 150

	for i := 0; i < 40; i++ {
		l.Info("T", strings.Repeat("y", 40))
	}
	l.Close()

	files := l.Files()
	for _, want := range []string{"_1" + fileSuffix, "_2" + fileSuffix} {
		found := false
		for _, f := range files {
			if strings.Contains(f, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少轮转文件 %s，实际: %v", want, files)
		}
	}
}

// 保留策略：删除 mtime 超过 7 天的文件（**按时间，不是按文件个数**）。
func TestCleanupRemovesOldByMtime(t *testing.T) {
	dir := t.TempDir()

	old := filepath.Join(dir, filePrefix+"2020-01-01"+fileSuffix)
	recent := filepath.Join(dir, filePrefix+"2020-01-02"+fileSuffix)
	unrelated := filepath.Join(dir, "messages_2020-01-01.log")
	for _, p := range []string{old, recent, unrelated} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unrelated, past, past); err != nil {
		t.Fatal(err)
	}

	_ = New(dir) // 构造时执行清理

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("超过 7 天的 translator_ 日志应被删除")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("7 天内的日志不应被删除")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Error("非 translator_ 前缀的文件不应被删除")
	}
}

// 删除日志：必须先关句柄（Windows 上文件被占用删不掉），并清空内存缓冲。
func TestDeleteAllClosesHandleAndClearsBuffer(t *testing.T) {
	l, dir := newTestLogger(t)
	l.Info("T", "a")
	l.Info("T", "b")
	if len(l.Recent(0)) == 0 {
		t.Fatal("缓冲应非空")
	}

	deleted, errs := l.DeleteAll()
	if len(errs) != 0 {
		t.Errorf("删除报错: %v", errs)
	}
	if deleted != 1 {
		t.Errorf("删除文件数 = %d，期望 1", deleted)
	}
	if len(l.Recent(0)) != 0 {
		t.Error("删除后内存缓冲应被清空")
	}
	if files := l.Files(); len(files) != 0 {
		t.Errorf("目录应无日志文件，实际: %v", files)
	}

	// 删除后仍能继续写（惰性重开）
	l.Info("T", "after-delete")
	l.Close()
	if raw := readFirstLog(t, dir); !strings.Contains(raw, "after-delete") {
		t.Errorf("删除后应能继续写日志，实际内容: %q", raw)
	}
}

// 目录不可写时降级为「仅内存」，不 panic、不阻塞主流程。
func TestUnusableDirDegradesToMemoryOnly(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "no", "such", "dir", "\x00bad"))
	l.Info("T", "still works")
	if got := l.Recent(0); len(got) != 1 {
		t.Errorf("内存日志应可用，得到 %d 行", len(got))
	}
	l.Close()
}

// 并发写入不应丢行、不应 panic（无 -race 时的基本保障）。
func TestConcurrentWrites(t *testing.T) {
	l, dir := newTestLogger(t)

	const goroutines, perG = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				l.Info("T", fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	l.Close()

	if got := len(l.Recent(0)); got != goroutines*perG {
		t.Errorf("缓冲行数 = %d，期望 %d（并发下丢行）", got, goroutines*perG)
	}

	raw := readFirstLog(t, dir)
	if n := strings.Count(raw, "\n"); n != goroutines*perG {
		t.Errorf("文件行数 = %d，期望 %d", n, goroutines*perG)
	}
}

// 跨天应换文件（V1 _ensure_file_open 的日期判断）。
func TestDateRolloverOpensNewFile(t *testing.T) {
	l, dir := newTestLogger(t)
	l.Info("T", "day1")

	// 模拟跨天：把 curDate 改成昨天，下一次写入应换到今天的文件
	l.mu.Lock()
	l.curDate = time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	l.mu.Unlock()

	l.Info("T", "day2")
	l.Close()

	raw := readFirstLog(t, dir)
	if !strings.Contains(raw, "day2") {
		t.Errorf("跨天后应写入新文件，实际内容: %q", raw)
	}
	if len(l.Files()) != 1 {
		t.Errorf("两个日期都写进同一文件，文件列表: %v", l.Files())
	}
}

func readFirstLog(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), fileSuffix) && !strings.Contains(e.Name(), "_") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	// 退而求其次：任意一个
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), fileSuffix) {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			return string(b)
		}
	}
	return ""
}
