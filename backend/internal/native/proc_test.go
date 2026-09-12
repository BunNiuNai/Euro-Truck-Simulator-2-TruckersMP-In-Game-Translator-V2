package native

import (
	"sync"
	"testing"
)

// fakeProc 是存活状态可控的假进程，用来模拟 native 崩溃。
type fakeProc struct {
	mu    sync.Mutex
	alive bool
	stops int
}

func (f *fakeProc) setAlive(v bool) {
	f.mu.Lock()
	f.alive = v
	f.mu.Unlock()
}

func (f *fakeProc) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeProc) proc() *Proc {
	return &Proc{
		stop: func() {
			f.mu.Lock()
			f.stops++
			f.alive = false
			f.mu.Unlock()
		},
		alive: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.alive
		},
	}
}

// native 崩溃后必须能被重新拉起。
//
// 这条挡的是「native 挂过一次，悬浮窗/热键/托盘就永久消失」。
// 早先 ensureProcess 的判据是 `s.launched != nil`，而 s.launched 只在
// teardown 里清空，teardown 又只在 Go 自己退出时才调。崩溃不走那条路，
// 于是 s.launched 一直非 nil，之后每次重连都认为「进程已经拉起来了」，
// Go 侧就一直在重试一个再也不会有人监听的管道名。
func TestEnsureProcessRelaunchesAfterCrash(t *testing.T) {
	var (
		launches int
		procs    []*fakeProc
	)
	s := &Supervisor{
		exePath:  "native.exe",
		pipeName: "test-pipe",
		launcher: func(exePath, pipeName string) (*Proc, error) {
			launches++
			f := &fakeProc{alive: true}
			procs = append(procs, f)
			return f.proc(), nil
		},
	}

	// 第一次连接：拉起来
	if err := s.ensureProcess(); err != nil {
		t.Fatalf("首次拉起失败: %v", err)
	}
	if launches != 1 {
		t.Fatalf("首次应拉起 1 次，实际 %d 次", launches)
	}

	// 进程还活着：不该重复拉，否则每次重连都会多出一个 native 进程
	if err := s.ensureProcess(); err != nil {
		t.Fatalf("重复调用出错: %v", err)
	}
	if launches != 1 {
		t.Errorf("进程仍在运行时不该重复拉起，实际 %d 次", launches)
	}

	// 模拟崩溃：进程死了，但**没有任何人调过 teardown**——这正是真实场景
	procs[0].setAlive(false)

	if err := s.ensureProcess(); err != nil {
		t.Fatalf("崩溃后拉起失败: %v", err)
	}
	if launches != 2 {
		t.Fatalf("进程已死时应重新拉起，实际只拉了 %d 次", launches)
	}
	if got := procs[0].stopCount(); got != 1 {
		// 死进程也要收掉：它还占着管道名，新进程会和它抢
		t.Errorf("死掉的旧进程应被 Stop 一次，实际 %d 次", got)
	}
}

// 已退出的进程要能如实报告，否则崩溃检测形同虚设。
func TestProcAliveReflectsExit(t *testing.T) {
	f := &fakeProc{alive: true}
	p := f.proc()

	if !p.Alive() {
		t.Error("进程存活时 Alive 应为 true")
	}
	p.Stop()
	if p.Alive() {
		t.Error("Stop 之后 Alive 应为 false")
	}
	if f.stopCount() != 1 {
		t.Errorf("Stop 应只执行一次，实际 %d 次", f.stopCount())
	}
}

// 空 Proc 不该 panic——ensureProcess 会在 launched == nil 的路径上碰到它。
func TestNilProcIsSafe(t *testing.T) {
	var p *Proc
	if p.Alive() {
		t.Error("nil Proc 的 Alive 应为 false")
	}
	p.Stop() // 不应 panic
}
