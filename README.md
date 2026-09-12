# Translator V2

Euro Truck Simulator 2 · TruckersMP 游戏内聊天翻译器 — 第二代架构。

> **首要原则 P1：功能与 V1 完全一致，只更换实现。**
> V2 的功能范围 = V1 的功能范围，不多不少。

## 技术栈与分层

```
TypeScript SPA (Vite)          ← native 宿主 WebView2，经 REST + WS 通信
        │
Go 业务大脑                     ← ingest / engine / providers / dictionary / cache
        │                          config / logger / task / sink / capability / ipc / api
        │  Named Pipe + 长度前缀 JSON（双向 + 心跳）
C++ 纯 Win32 能力层             ← pipe / webview / hotkey / input / tray / window
```

**边界铁律**：Go 是唯一业务大脑；C++ 只做 Windows 能力，不含业务逻辑（非目标 N3）。

## 判定宪法

> **迁移任何一个文件之前，先读它、判断它到底在干什么，再决定用哪种语言替换。**

不允许按「某类东西一起划给某个语言」的方式一刀切。一个文件里常混着好几件事，
判定单位是**职责**而不是文件——例如 `input_sender.py` 同时包含 Win32 原语（→ C++）、
热键解析（→ Go）、发送时序编排（→ Go）。

18 个生产文件里，10 个是单一归属，**8 个需要拆分**（其中 `main.py` 拆 6 块、
`overlay.py` 拆 5 块）。

## 目录

```
Translator-V2/
├── backend/     Go：业务大脑
├── native/      C++：Windows 能力层（阶段 1 起在主链路）
├── frontend/    TypeScript SPA
├── resources/   数据（词库 / 预设 / 默认配置）——与代码分离
├── docs/        架构与迁移文档
└── tools/       基线测量与资源生成脚本
```

## 工具链状态（阶段 0 已验证 ✅）

| 工具 | 状态 | 版本 / 路径 |
|---|---|---|
| **Go SDK** | ✅ 已验证 | `go1.27.1 windows/amd64` — `C:\Go\bin\go.exe` |
| **VS Build Tools 2022** | ✅ 已验证 | `17.14.37628.2` — `D:\Microsoft Visual Studio\2022\BuildTools` |
| └ MSVC 编译器 | ✅ | MSVC `14.44.35207`（`cl.exe` @ `Hostx64\x64`） |
| └ Windows SDK | ✅ | `10.0.26100.0` |
| CMake | ✅ | `4.3.2` |
| Node / npm | ✅ | `v24.9.0` / `11.11.0` |
| WebView2 Runtime | ✅ | 152.x |
| Python | ✅ | `3.14.3`（运行 V1 与基线脚本） |
| mingw-w64 / clang | ⚠️ **可选** | 装了才能用 `go test -race`，见下 |

> ⚠️ **`go test -race` 当前不可用**：竞态检测器需要 cgo，而 cgo 在 Windows 上只认
> gcc / clang，**不认 MSVC 的 `cl.exe`**。本机两者都没有，所以 `-race` 会报
> `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`。
>
> 这修正了一个早期判断：曾把「Go 自带竞态检测器」列为 Go 相对 C++ 的优势之一，
> 但在「只有 MSVC」的工具链下这个优势用不上。要用需额外装 mingw-w64 或 LLVM/clang
> （约 300MB~1GB），属于**可选**工具链。
>
> 因此 `internal/cache`（LRU + 并发合并）这类并发代码必须靠**显式设计 + 用例**保证正确，
> 不能指望 `-race` 兜底——测试里已覆盖并发合并计数、失败释放等待者、等待超时降级。

**验证记录（2026-08-15）**：

```text
$ go version
go version go1.27.1 windows/amd64

$ go build ./...     → 通过（13 个包全部编译成功）
$ go vet ./...       → 无问题
$ go run ./cmd/translator --version
ets2-translator (Go) v2.0.0-dev

$ cmake -S native -B native/build -G "Visual Studio 17 2022" -A x64
-- The CXX compiler identification is MSVC 19.44.35228.0
-- Selecting Windows SDK version 10.0.26100.0
-- Configuring done / Generating done

$ cmake --build native/build --config Release
translator_native.vcxproj -> dist\translator_native.exe

$ dist\translator_native.exe
translator_native protocol=1
pipe name: ets2translator-native-v1
[phase 0] named pipe server not implemented yet
```

> **注意**：Go 是解压 `.zip` 装到 `C:\Go` 的（不是 MSI），并手动把 `C:\Go\bin`
> 加进了系统 PATH。升级 Go 时把 `C:\Go` 整个替换掉即可，PATH 不用动。
>
> **注意**：VS Build Tools 的 `cl.exe` **不在系统 PATH 里**（这是正常的，MSVC 从设计上
> 就不进 PATH）。所以 `where cl` 会报找不到，但这**不代表装失败**。CMake 自己会通过
> 注册表找到它。判断 MSVC 是否可用请用：
>
> ```powershell
> & "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe" -latest -products * -property installationPath
> ```

## 常用命令

```powershell
# ── 构建 ──
cd backend;  go build ./... ; go test ./... ; go run ./cmd/translator --version
cmake -S native -B native/build -G "Visual Studio 17 2022" -A x64
cmake --build native/build --config Release

# ── 数据与骨架 ──
python tools/gen_resources.py            # 从 V1 数据生成 resources/*.json（只读 V1）
python tools/gen_resources.py --check    # 只校验 schema，不写盘
python tools/scaffold.py                 # 生成骨架（幂等，已存在不覆盖）
python tools/scaffold.py --list

# ── 基线测量 ──
powershell -File tools/baseline/measure-v1.ps1        # 进程级 B1/B2/B3/B9/B10/B14
python tools/baseline/pipeline_probe.py               # 管线级 B5'/B6'/B7/B8'
```

> ⚠️ `tools/baseline/measure-v1.ps1` 必须保持 **UTF-8 with BOM**：
> Windows PowerShell 5.1 会把无 BOM 的 UTF-8 脚本按本地代码页读取，导致中文注释破坏语法。
> `tools/` 下的 `.py` 脚本无此限制。

## 安全硬约束（ADR-014）

**禁止任何形式的游戏进程注入与渲染 hook**：不注入 DLL、不 hook swapchain、
不读写游戏内存、不拦截网络封包。只允许读取 TruckersMP 客户端自己写出的聊天日志，
以及创建独立的透明置顶窗口。

理由：已核实 [TruckersMP 官方规则](https://truckersmp.com/rules) —— §2.1 以
「any kind of tool」宽泛措辞禁止改变玩法的工具（6 个月至永久封禁），§1.11 规定
歧义解释权在 TruckersMP，§5.2 保留随时封禁的裁量权。只读悬浮窗虽未被明文禁止，
但**没有任何条款保护第三方工具**，因此按风险最小化处理。

由此产生的唯一功能限制：**独占全屏模式下悬浮窗不可见**（V1 同样如此），
需要用窗口化 / 无边框全屏。
