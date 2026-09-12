这个文件只是占位，用来让 `//go:embed all:resources` 始终能成立。

构建脚本 tools/build-exe.ps1 会把 resources/ 的内容拷到这个目录里
（dictionary.json / keymap.json / providers.json / config.json）。

不要删掉它，也不要往这个目录里手工放文件。
