这个文件只是占位，用来让 `//go:embed all:frontend` 始终能成立。

构建脚本 tools/build-exe.ps1 会把 frontend/dist 的内容拷到这个目录里。
嵌入包用 index.html 是否存在判断「是否真的打包过」，
所以只有这个占位文件时 FrontendFS() 会返回 nil，调用方回退到外部目录。

不要删掉它，也不要往这个目录里手工放文件。
