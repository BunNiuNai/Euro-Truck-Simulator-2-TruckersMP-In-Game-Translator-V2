module github.com/BunNiuNai/ets2-translator-v2/backend

go 1.23

// 依赖策略：阶段 0 不引入任何第三方模块，保证 `go build ./...` 无需网络即可通过。
// 阶段 1 起按需加入：
//   golang.org/x/sys/windows   Win32 调用（DPAPI、注册表）
//   golang.org/x/sync          singleflight（同文本并发合并）
