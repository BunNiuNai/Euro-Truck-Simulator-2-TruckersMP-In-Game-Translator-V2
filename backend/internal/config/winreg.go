// Windows 注册表读取与环境变量展开的薄封装。
//
// 集中在这一个文件里，是为了让 config.go 保持可读，
// 并且**只有一份**注册表读取实现（V2 早期版本在 ingest/chatlog 里也有一份，已合并到此）。
package config

import (
	"os"
	"regexp"
	"syscall"
)

const keyRead = 0x20019 // KEY_READ

// winregKey 是注册表键句柄的类型别名。
type winregKey = syscall.Handle

// HKEY_CURRENT_USER 在 syscall 里是无类型常量，需显式指定类型才能作为 syscall.Handle 使用。
const hkeyCurrentUser syscall.Handle = syscall.HKEY_CURRENT_USER

var envVarRe = regexp.MustCompile(`%([^%]+)%`)

// expandEnvVars 展开 Windows 风格的 %NAME% 环境变量。
//
// 必须自己实现：Go 的 os.ExpandEnv 只认 $VAR / ${VAR} 语法，
// 而注册表里的「文档」路径是 REG_EXPAND_SZ 的 %USERPROFILE%\Documents。
// 用 os.ExpandEnv 会原样返回未展开的字符串，导致日志目录永远找不到——
// 而且**不报错**，属于最难查的静默失效。
func expandEnvVars(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[1 : len(m)-1])
	})
}

func utf16Ptr(s string) *uint16 { return syscall.StringToUTF16Ptr(s) }

func utf16ToString(u []uint16) string { return syscall.UTF16ToString(u) }

func regOpenKeyEx(key syscall.Handle, subKey *uint16, reserved, access uint32,
	out *syscall.Handle) error {
	return syscall.RegOpenKeyEx(key, subKey, reserved, access, out)
}

func regCloseKey(h syscall.Handle) error { return syscall.RegCloseKey(h) }

func regQueryValueEx(h syscall.Handle, name *uint16, reserved *uint32, typ *uint32,
	data *byte, cbData *uint32) error {
	return syscall.RegQueryValueEx(h, name, reserved, typ, data, cbData)
}

func lazyDLL(name string) *syscall.LazyDLL { return syscall.NewLazyDLL(name) }

// SystemPrefersDark 读 Win10/11 的「应用模式」开关，返回系统当前是否为深色。
//
// 用于解析配置里的 `theme = "system"`：前端用 prefers-color-scheme 自己解析出
// 深浅，Go 这边也必须得出**同一个答案**，否则「跟随系统」时会变成
// 「浅色界面配深色 Mica 材质」的错配。
//
// 读不到时返回 true —— 与 V2 的默认主题（深色）保持一致，宁可错成默认值，
// 也不要抛错让启动流程多一条失败路径。
func SystemPrefersDark() bool {
	var h winregKey
	sub := utf16Ptr(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`)
	if err := regOpenKeyEx(hkeyCurrentUser, sub, 0, keyRead, &h); err != nil {
		return true
	}
	defer func() { _ = regCloseKey(h) }()

	// AppsUseLightTheme: 1 = 浅色, 0 = 深色
	var typ, cb uint32
	var buf [4]byte
	cb = uint32(len(buf))
	if err := regQueryValueEx(h, utf16Ptr("AppsUseLightTheme"), nil, &typ, &buf[0], &cb); err != nil {
		return true
	}
	// REG_DWORD 是小端；手工拼而不是 unsafe.Pointer，免得多引入一个包
	v := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	return v == 0
}
