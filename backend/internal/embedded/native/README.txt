这个文件只是占位，用来让 `//go:embed all:native` 始终能成立。

构建脚本 tools/build-exe.ps1 会把 dist/translator_native.exe 拷到这个目录里。

不要删掉它，也不要往这个目录里手工放文件。
