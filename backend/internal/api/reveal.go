// 在系统文件管理器中打开目录。
//
// 单独成文件是为了把平台差异圈在一处：将来若要支持非 Windows，
// 只需在这里加分支，不必动 api.go。
package api

import (
	"os/exec"
	"runtime"
)

// openInFileManager 打开一个目录。
//
// ⚠️ explorer.exe 的行为有个坑：**即使成功打开，它的退出码也常常非零**。
// 所以这里用 Start() 只负责「把进程拉起来」，不看退出码——
// 若改成 cmd.Run() 并检查 error，会在明明成功时误报失败。
func openInFileManager(dir string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", dir).Start()
	case "darwin":
		return exec.Command("open", dir).Start()
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}
