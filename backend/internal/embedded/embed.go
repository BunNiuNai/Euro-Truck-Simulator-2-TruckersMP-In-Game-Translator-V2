// Package embedded 把前端产物与 native 二进制打进 Go 可执行文件，
// 目标是产出**单个 EXE**——用户双击就能跑，不需要旁边放着 dist 目录和
// translator_native.exe。V1 用 build_exe.py（PyInstaller）达到同样效果。
//
// ⚠️ 为什么用**目录**嵌入而不是直接嵌那两个具体文件：
//
//	//go:embed ../dist/translator_native.exe     ← 不允许（不能引用父目录）
//	//go:embed native/translator_native.exe      ← 文件不存在时**整个 go build 失败**
//
// 第二条是关键的坑：直接嵌一个具体文件的话，只要构建产物还没拷进来，
// `go build ./...`、`go test ./...`、`go vet ./...` 全部直接报错——
// 开发期每换一台机器、每次清理过一次 dist，整个后端就编译不过了，
// 而报错信息只会说「pattern matches no files」，看不出是打包脚本没跑。
//
// 所以这里嵌的是目录，目录里放一个占位文件让嵌入始终成立；
// 真产物没有时在**运行期**回退到外部文件（见 FrontendFS / NativeBinary 的 nil 语义）。
package embedded

import (
	"embed"
	"io/fs"
)

// 构建脚本（tools/build-exe.ps1）会把 frontend/dist 的内容拷进 frontend/。
//
//go:embed all:frontend
var frontendFS embed.FS

// 构建脚本会把 dist/translator_native.exe 拷进 native/。
//
//go:embed all:native
var nativeFS embed.FS

// 构建脚本会把 resources/ 的内容拷进 resources/。
//
// 这一份**必须**有：单 EXE 只嵌前端与 native 是不够的——程序起来第一件事
// 就是读 resources/dictionary.json，读不到会直接以
// 「加载词库失败：未加载到任何词库」退出。实测过：这样的「单 EXE」
// 拷到一个空目录里根本起不来，而它看起来是构建成功的。
//
//go:embed all:resources
var resourcesFS embed.FS

// frontendIndex 是「前端产物确实存在」的判据。
//
// 不能只看 fs.Sub 是否成功：占位状态下目录也在、Sub 也会成功，
// 那样会把一个只有 .keep 的空目录当成前端根目录交给 http.FileServer，
// 于是每个页面请求都返回 404，而启动日志一切正常——最难查的那种。
const frontendIndex = "index.html"

// nativeBinaryName 是内嵌的 native 可执行文件名，必须与构建脚本拷贝的名字一致。
const nativeBinaryName = "translator_native.exe"

// FrontendFS 返回内嵌的前端产物根目录；还没打包时返回 nil。
//
// 调用方拿到 nil 应当回退到 `-frontend` 指定的目录，
// 而不是当作「前端就是空的」。
func FrontendFS() fs.FS {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, frontendIndex); err != nil {
		return nil
	}
	return sub
}

// NativeBinary 返回内嵌的 native 可执行文件内容；还没打包时返回 nil。
func NativeBinary() []byte {
	b, err := nativeFS.ReadFile("native/" + nativeBinaryName)
	if err != nil {
		return nil
	}
	return b
}

// NativeBinaryName 返回内嵌 native 的文件名（供调用方决定解包后的落盘名字）。
func NativeBinaryName() string { return nativeBinaryName }

// resourcesMarker 是「资源确实打包过」的判据（与 frontendIndex 同理：
// 只看目录存不存在会把只有占位文件的空目录当成资源目录）。
const resourcesMarker = "dictionary.json"

// placeholderName 是各 embed 目录里为了让 `//go:embed` 成立而放的文件名。
//
// 它必须与三个目录下实际的文件名一致（frontend/、native/、resources/ 各一份）。
// 改成别的名字时三处都要跟着改，否则占位文件会被当成真产物。
const placeholderName = "README.txt"

// Resources 返回内嵌的资源文件（名字 → 内容）；还没打包时返回 nil。
//
// 返回 map 而不是 fs.FS：调用方要做的是「把它们解到数据目录」，
// 而解包需要遍历并逐个写文件，直接拿到内容比经过 fs.FS 再 ReadFile 更直白。
// 这几个文件加起来只有几十 KB，一次读进内存没有任何负担。
func Resources() map[string][]byte {
	entries, err := fs.ReadDir(resourcesFS, "resources")
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// 跳过占位文件。
		//
		// `//go:embed all:resources` 要求目录非空，所以放了一个 README.txt
		// 占位；但它不是真资源，不跳的话会被一起解到用户的
		// `<配置目录>/resources/README.txt`，在数据目录里凭空多一个没人读的文件。
		if e.Name() == placeholderName {
			continue
		}
		b, err := resourcesFS.ReadFile("resources/" + e.Name())
		if err != nil {
			continue
		}
		out[e.Name()] = b
	}
	if _, ok := out[resourcesMarker]; !ok {
		return nil // 占位状态
	}
	return out
}
