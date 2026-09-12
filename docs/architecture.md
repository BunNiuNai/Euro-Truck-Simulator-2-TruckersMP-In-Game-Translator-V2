# Translator V2 架构设计

| 项目 | 内容 |
|---|---|
| 文档版本 | **v1.3** |
| 日期 | 2026-08-15 |
| 状态 | 待评审（ADR-005 已按决策更新） |
| 上游文档 | 《Translator V2 第二代架构重构详细需求文档》 |
| 配套文档 | `docs/migration-matrix.md`（V1→V2 能力迁移对照表，验收基线） |
| V1 代码基线 | `ets2-translator` v2.3.1（生产 7859 行 / 测试 1267 行 / 99 个用例） |

**v1.1 修订记录**

| 变更 | 说明 |
|---|---|
| ADR-005 改写 | C++ 层范围由「仅 DirectX + WGC」扩大为「**全部 Windows 底层能力 + DirectX 渲染**」（决策方要求） |
| ADR-001 修订 | C++ 不再是无状态能力层，改为「**期望状态由 Go 持有，实际状态由 C++ 持有**」 |
| 新增 ADR-011 | 显示层双轨：DirectX 只做展示，输入栏必须留在 WebView（中文 IME 约束） |
| 新增 ADR-012 | native 崩溃恢复协议：期望状态重放 + 心跳 |
| 阶段重排 | 原「阶段 4 条件性启动 C++」取消，C++ 自阶段 1 起在主链路（DirectX 渲染层单独排在阶段 4） |

**v1.2 修订记录**

| 变更 | 说明 |
|---|---|
| **产品形态定性** | 决策方明确：**翻译器是一个独立的 EXE，读取聊天日志**。不注入游戏、不做画面捕获、不做游戏内渲染管线接管 |
| ADR-011 重写 | 显示层由「DirectX + WebView 双轨」改为**单一 WebView 透明置顶层**；`window_mode` 回到 2 值（`overlay` / `standalone`） |
| ADR-005 范围收缩 | C++ 职责**移除 DirectX 渲染层与 WGC 捕获**，新增 WebView2 宿主；C++ 收敛为纯 Win32 能力层 |
| 新增 ADR-014 | **禁止任何形式的游戏进程注入与渲染 hook**（安全硬约束，已核实 TruckersMP 官方规则） |
| ADR-008 简化 | Q2 已决「可以重新配置」→ **不写 V1 配置兼容层**，删除 DPAPI 密文兼容、`v1→v2` 迁移等要求 |
| ADR-002 收敛 | Q3 已决「都不做」→ Source 只实现 `chatlog`；`native/capture` 不实现；`Event` 不预留帧/音频字段 |
| 阶段重排 | 原阶段 4（DirectX 渲染层）**取消**；阶段压缩为 0–4 |
| 风险调整 | 移除 R2（DirectX 调试成本）；R1（三工具链成本）缓解方式改写 |

**v1.3 修订记录**

| 变更 | 说明 |
|---|---|
| **新增首要原则 P1** | **功能对齐**：V2 的功能范围 = V1 的功能范围，不多不少；不新增 V1 没有的用户可见功能；V1 已有能力必须逐条对齐 |
| G1 降级 | 「多数据源可插拔」由功能目标改为**架构能力**（接口保留、只实现 `chatlog`），避免被误读为新增功能 |
| **撤回两项多写的功能** | v1.2 依据「需要鼠标交互」写入了**点击消息复制**与**消息悬停高亮**。经代码核查（`overlay.py:227` 的 `state=tk.DISABLED`、`_build_context_menu`、`<Motion>` 仅用于缩放光标），**V1 均无此功能**，按 P1 撤销 |
| V1 实际交互清单（对齐基准） | 滚动消息、拖拽移动窗口、边缘缩放、消息区右键菜单（仅 Settings / Exit）、热键复制译文（`copy_hotkey`）。**消息不可选中**，故无拖选复制 |
| 状态展示降级 | `native.status` 仅写日志，不在 UI 新增展示（V1 无此概念） |
| 独占全屏提示取消 | v1.2 的 W24 属新增提示，按 P1 撤销；改为在用户文档中说明（V1 同样受此限制） |

---

## 1. 文档定位

本文档把上游需求书从「方向共识」补成「可执行规格」。上游需求书定义了技术栈方向（Go + C++ + TypeScript），本文档负责回答它尚未回答的问题：

- 模块边界究竟怎么划分（需求书给出的清单是 V1 的 1:1 改名，未做边界重设计）
- 已通过验证的实时翻译链路如何保证功能一致（需求书缺少数据源模块与能力清单）
- Go / C++ / Frontend 三者的职责分界线在哪里（需求书存在双渲染层重叠）
- 数据外置后的 schema、版本与更新冲突如何处理
- 每个阶段怎么验收

**本文档不包含**任何代码实现，也不创建 `backend/`、`native/`、`frontend/` 脚手架。

---

## 2. 目标与非目标

### 2.1 目标

| # | 目标 | 说明 |
|---|---|---|
| **P1** | **功能对齐（首要原则）** | **V2 的功能范围 = V1 的功能范围，不多不少。** 不新增 V1 没有的用户可见功能；V1 已有的能力必须逐条对齐（对照表见 `migration-matrix.md`）。**更换语言与架构不是加功能的理由** |
| G1 | **数据源可插拔（架构能力，非新增功能）** | `Source` 接口抽象保留（成本为零），以便将来接入新来源时不必改动 Engine。**当前只实现 `chatlog`**（Q3 已决） |
| G2 | **UI 与业务逻辑彻底解耦** | 业务能力（含定时广告发送）不再寄生在 UI 回调里 |
| G3 | **数据与代码分离** | 词库、Provider 预设、默认配置移出代码，支持用户修改与在线更新 |
| G4 | **译文浮在游戏画面上方** | 透明置顶悬浮窗（免 alt-tab 切换），可鼠标交互 |
| G5 | **功能零丢失** | V1 全部用户可见能力迁移完成，验收方式见 `migration-matrix.md` |
| G6 | **可长期维护** | 单一事实源（配置/日志/状态），模块间靠接口通信，可独立测试 |

### 2.2 非目标（明确排除）

| # | 非目标 | 理由 |
|---|---|---|
| N1 | 以「提升运行效率」为主线 | V1 负载是 500ms 轮询日志 + HTTP 调 LLM，端到端瓶颈是模型响应 200ms~2s。语言更换带不来可感知收益 |
| N2 | 以「减小打包体积」为主线 | 实测 V1 产物 `dist/ETS2-TruckersMP翻译器-v2.3.1.exe` = **25 MB**，已经很小 |
| N3 | **把业务逻辑放进 C++** | C++ 承担 Windows 底层能力（见 ADR-005），但**翻译引擎、Provider 管理、缓存、词典、配置、日志、任务调度全部留在 Go**。业务逻辑跨语言拆分会导致「改一条翻译规则要动两种语言 + 维护 ABI」，比 V1 更难维护 |
| N4 | 支持非 Windows 平台 | 产品强绑定 Windows 桌面（注册表取文档目录、DPAPI、Win32 窗口样式与热键） |
| N5 | 一次性删除 Python | 采用渐进式迁移，见第 14 节 |

### 2.3 对上游需求书「当前 Python 架构问题」的实测校正

上游列出 6 条问题，经与代码核对，其中 3 条不成立或证据不足。校正结论决定了投入方向：

| 上游问题 | 实测证据 | 结论 |
|---|---|---|
| ① Python 运行效率有限 | 关键路径是日志轮询 + 网络 IO，非 CPU 密集 | ❌ 非真问题 |
| ② 打包后程序体积较大 | `dist/*.exe` = 25 MB | ❌ 非真问题 |
| ③ Windows 底层调用不够直接 | `config.py` / `input_sender.py` / `hotkey_manager.py` / `tray_icon.py` / `acrylic_helper.py` 已全程 ctypes 直调，无 pywin32 | ❌ 大部分不成立 |
| ④ UI 与业务逻辑耦合 | `overlay.py`（1017 行 UI）内直接实现 `_do_translate → _do_auto_send → _on_send_done` 编排；**定时广告发送器整条业务链**（`_ad_schedule_tick` / `_ad_send_current` / `_ad_start`，`main.py:1014-1131`）实现在设置窗口的 tk `after()` 回调里，关闭设置窗口即停摆 | ✅ **真问题** |
| ⑤ 数据和代码混合 | `provider_presets.py` 硬编码 20 组预设；`chat_dictionary.py` 硬编码五层词典（约 110 条俚语 + 短语/结构化/系统消息/ETS2 词汇） | ✅ **真问题** |
| ⑥ 扩展到 OCR/视频/窗口翻译架构不足 | 数据源在 `monitor.py` 中写死为单一日志文件 tail，Engine 与来源耦合 | ✅ **真问题，也是唯一根本重构理由** |

> **结论：V2 的驱动力是 G1/G2/G3/G4（解耦、扩展、游戏内渲染），不是性能与体积。**

---

## 3. 关键决策记录（ADR）

| ID | 决策 | 状态 |
|---|---|---|
| ADR-001 | 三层分层：Go 业务核心 + C++ Windows 能力层 + WebView 前端。**Go 持有期望状态，C++ 持有实际状态** | 已定（v1.1 修订） |
| ADR-002 | **新增 `ingest/` 数据源层**，抽象 Source 接口；聊天日志是第一实现 | 已定 |
| ADR-003 | 前端**不使用 Wails / Tauri**：native 宿主 WebView2，Go 通过本机 HTTP 提供前端资源 | 已定（Q6 已决） |
| ADR-004 | Go ↔ C++ 用 **Named Pipe + 长度前缀 JSON**，不使用 gRPC | 已定 |
| ADR-005 | **C++ 承担全部 Windows 底层能力**：WebView2 宿主、窗口操作（透明置顶/毛玻璃/穿透/拖拽）、热键、输入模拟、托盘、剪贴板 | 已定（v1.2 收缩：移除 DirectX 与 WGC） |
| ADR-006 | 数据外置到 `resources/*.json`，采用三层合并 + 用户覆盖永不覆盖 | 已定 |
| ADR-007 | 业务逻辑退出 UI：广告发送器、发送链路进 Go 任务队列 | 已定 |
| ADR-008 | 配置与密钥延续 DPAPI 加密；V2 需能读入 V1 配置 | 已定 |
| ADR-009 | 单一事实源：配置、日志、统计只由 Go 持有，前端不做本地持久化 | 已定 |
| ADR-010 | 阶段 0 基线测量前不写任何业务代码 | 已定 |
| ADR-011 | **显示层单一化**：只有 WebView 透明置顶层；鼠标交互与中文输入均由 WebView 原生提供 | 已定（v1.2 重写） |
| ADR-012 | native 崩溃恢复：Go 重放期望状态，native 心跳上报 | 已定 |
| ADR-013 | **WebView2 宿主归 native**（C++ 创建 HWND 并承载 WebView2）；前端为纯 SPA，通过 REST + WS 与 Go 通信 | 已定（Q6 已决） |
| ADR-014 | **禁止任何形式的游戏进程注入与渲染 hook**；只允许独立的透明置顶层 | 已定（安全硬约束，见下） |
| ADR-015 | **全局热键用 `RegisterHotKey` 为主、轮询为降级** | **待确认（Q9）**，见下 |
| ADR-016 | **事件推送用 SSE 而非 WebSocket**（只监听回环地址） | 已定（见下） |

### ADR-001 三层分层与状态归属

Go 是业务大脑，持有全部业务状态与期望状态。C++ 是 Windows 能力层，持有 Windows 侧的实际状态（HWND、热键注册、托盘句柄、剪贴板句柄）。前端只做呈现与输入。

**v1.1 修订原因**：C++ 接管热键/托盘/输入后，它**必然是有状态的**（注册的热键、托盘图标句柄、剪贴板句柄都在 native 侧）。因此把「无状态」改为「**Go 持期望状态，C++ 持实际状态**」，并用期望状态重放来覆盖恢复场景（ADR-012）。

配合 N3 的硬约束：**状态可以分两层，业务逻辑不可跨语言。**

### ADR-002 新增数据源层（对上游需求书的关键补齐）

上游需求书的 Go 模块清单为 `api / translator / providers / dictionary / cache / config / logger`——**`monitor.py` 没有对应物**。而它是当前产品**唯一的数据来源**：tail `文档\ETS2MP\logs\chat_*.txt`，用正则解析 `[Channel] [HH:MM:SS] PlayerName (ID): Message`，并区分系统消息、识别服务器名与玩家昵称。

上游 C++ 层的 `capture/` 是 Windows Graphics Capture（屏幕抓取），与日志文件 tail 不可互换。若照需求书字面实现，实时翻译链路第一步即断，直接违反「保证功能一致」。

**决策**：引入独立 `ingest/` 层，定义 Source 接口，`chatlog` 为第一实现，未来 `screen`/`window`/`clipboard` 为并列实现，Engine 不感知来源。

> **v1.2 补充（Q3 已决「都不做，先把现有的做好」）**：Source 接口**只实现 `chatlog`**。保留接口抽象（成本为零，且是 G1 的落点），但 `Event` **不预留**帧时间戳 / 区域坐标 / 音轨分片等未使用字段；`native/capture` 不实现。将来真要做 OCR 时再扩字段，届时 `Event` 加可选字段是向后兼容的。

### ADR-003 不使用 Wails / Tauri（v1.1 修订）

上游推荐 Tauri；v1.0 曾建议改用 Wails（省掉 Rust 工具链）。两种框架都会带来同一个问题：**它们要自己拥有窗口（HWND）**。

而 ADR-005 已把全部 Windows 底层能力划给 C++。如果窗口由 Wails 创建、而毛玻璃/穿透/拖拽由 native 操作，就会出现「**一个 HWND 被两个进程分别操作**」——这正是 ADR-005 要消除的竞态。

**决策**：前端框架不承担窗口职责，改用 ADR-013 的方案。实测本机 Rust/cargo 未安装、WebView2 Runtime 152.0.4191.66 已就绪，该方案无需新增工具链。

> 需在阶段 0 实测 WebView2 方案的内存占用是否可接受（见 R3）。

### ADR-004 Named Pipe + 长度前缀 JSON

| 方案 | 评估 |
|---|---|
| gRPC | 需引入 protobuf + HTTP/2 + 代码生成 + 额外运行时，本机单用户进程间通信收益低 |
| WebSocket | 可用，但需占用端口，需处理端口冲突与本地回环暴露面 |
| **Named Pipe + 长度前缀 JSON** | 无端口、无依赖、Windows 原生、调试期可直接打印 JSON |

**决策**：Named Pipe，帧格式见第 8 节。若序列化开销成为瓶颈，可平滑替换为 MessagePack（帧格式不变，仅换 payload 编码）。

### ADR-005 C++ 承担全部 Windows 底层能力（v1.1 修订）

**决策**：C++ 层负责以下全部职责——

| 职责 | 归属 | 备注 |
|---|---|---|
| **WebView2 宿主** | **C++** | 创建 HWND 并承载前端 SPA（ADR-013） |
| **窗口操作** | **C++** | 透明置顶、鼠标穿透、拖拽、边缘缩放、毛玻璃、单实例互斥体 |
| 全局热键 `RegisterHotKey` | **C++** | 含消息循环（`GetMessage`），运行在固定 OS 线程 |
| 输入模拟 `SendInput` | **C++** | 含 Ctrl+V / Enter 组合键 |
| 剪贴板读写 | **C++** | `OpenClipboard` / `SetClipboardData` |
| 系统托盘 `Shell_NotifyIcon` | **C++** | 含右键菜单 |
| ~~DirectX 游戏内渲染层~~ | ❌ **取消（v1.2）** | 见 ADR-011 / ADR-014：不注入的前提下它相对 WebView 置顶层无额外能力 |
| ~~Windows Graphics Capture~~ | ❌ **取消（v1.2）** | Q3 已决「都不做」 |

**产品形态定性（v1.2）**：翻译器是一个**独立的 EXE**，读取 TruckersMP 写在 `文档\ETS2MP\logs\` 的聊天日志，在自己进程内翻译，用**独立进程的透明置顶窗口**把译文浮在游戏画面上方。它不进入游戏进程、不接管游戏渲染、不读写游戏内存。

**收益（决策依据）**：所有 Windows 句柄（HWND / 热键 / 托盘 / 剪贴板 / 输入）集中在一个进程，**Win32 状态单一归属**，避免 Go 与 C++ 各持一半 Win32 状态导致的竞态；能力边界干净，C++ 与 Go 可由不同人并行开发、互不改对方代码。

**已记录的保留意见**（供未来维护者参考，非阻塞）：

1. **故障面扩大**：热键是用户呼出输入栏的唯一途径。native 崩溃时热键、输入、托盘同时失效，产品等价于变砖；V1 中这些能力在同一进程内，不存在此问题。缓解措施见 ADR-012。
2. **IPC 开销**：热键触发与每次按键输入都需一次进程间往返。本机 Named Pipe 往返约 0.1~0.5 ms，远低于人的感知阈值，**延迟不是问题**，因此该点不作为反对理由。唯一例外的优化见下方「热键事件流」。

**热键事件流与本地缓存优化**

| 热键 | 事件流 | 优化 |
|---|---|---|
| 呼出输入栏 `shift+y` | native 检测 → `event.hotkey` → Go → Go 显示并聚焦 WebView 窗口 | — |
| 复制译文 `ctrl+c` | native 检测 → `event.hotkey` → Go → Go 回发 `clipboard.set` | **优化**：Go 每次产生译文后主动推送「最近一条译文」到 native 缓存，复制热键由 native 本地直接完成，零 IPC |
| 发送确认 `enter` | native 检测 → `event.hotkey` | — |

**C++ 组织方式约束（Q8 已决）**

已确认 C++ 代码**主要由 AI 生成、由决策方负责使用与排错组装**。据此对 `native/` 施加以下工程约束：

1. **单元要小且自证**：每个模块必须能独立编译并附带一个可直接运行的验证入口（如 `hotkey/` 提供一个只注册一个热键并打印触发的最小可执行），避免"整块编译通过但不知道哪一步错了"。
2. **优先用库，不手写 COM**：WebView2 宿主优先采用成熟封装；确需直接调 `ICoreWebView2` 时，把 COM 交互集中隔离在 `webview/` 一个模块内。
3. **IPC 边界要可观测**：`pipe/` 必须支持把收发帧打印到文件（调试开关），因为跨进程问题无法用单步断点排查。
4. **失败要显式**：所有 Win32 调用检查返回值并回报 `event.error`，不使用静默忽略。

### ADR-006 数据外置与三层合并

V1 曾明确决策「词库硬编码 Python 模块，不引入 JSON」（见 `docs/specs/2026-08-13-translation-engine-refactor-design.md`）。V2 反转该决策，理由是 G3 与在线更新。**这是有意的决策反转，需记录在案。**

外置必须解决「在线更新会冲掉用户自定义」的问题，因此采用三层合并：

```
用户覆盖层   %LOCALAPPDATA%\ETS2 Translator\resources\override\*.json   （优先级最高，永不参与更新）
在线更新层   %LOCALAPPDATA%\ETS2 Translator\resources\*.json            （可被服务端更新覆盖）
内置层       随程序分发的 resources/*.json                              （只读，随版本升级）
```

### ADR-007 业务逻辑退出 UI

V1 的定时广告发送器靠 `main.py` 里设置窗口的 `tk after()` 驱动，关闭设置窗口即停摆——这是一个真实的架构缺陷。V2 将其改造为 Go 任务队列中的 `AdSender` 任务，生命周期与 UI 无关。

### ADR-008 配置与密钥（v1.2 简化）

延续 DPAPI（`CryptProtectData` / `CryptUnprotectData`）加密 `api_key`。**实现位置仍在 Go 侧**（配置是业务数据，见 N3），以 `go:build windows` 实现。

> **v1.2 简化（Q2 已决「可以重新配置」）**：V2 **不写 V1 配置兼容层**，使用全新配置目录，用户重新填写 Provider 与 API Key。因此以下工作全部取消：
>
> - 读入 V1 `config.json` 中已加密的 `dpapi:` 密文
> - V1 `v1→v2` 字段迁移（`config.py:_migrate_config_v2` 的 29 字段扁平化 → `llm_providers` 数组）
> - `api_endpoint` / `api_key` / `api_model` 三个仅用于兼容的遗留字段
>
> 仍保留：配置原子写、损坏时备份为 `.corrupted.{timestamp}`、主目录不可写时回退 `%LOCALAPPDATA%`（这些是健壮性要求，与兼容无关）。
>
> **注意**：TMP 聊天日志目录（`文档\ETS2MP\logs\`）是**游戏产出**，与 V1 配置无关，**不受此决策影响**——`ingest` 仍读同一路径，无需迁移。

### ADR-009 单一事实源

配置、日志、统计、Provider 健康状态只由 Go 持有。前端通过 REST 读取、通过 REST 写入，不做本地持久化，从而消除 V1 中「Python 侧配置 + UI 侧副本」的双份状态问题。

### ADR-010 基线优先

上游第 7 条要求「保证功能一致」，但当前**没有任何基线数据**，无法验收。因此阶段 0 只做一件事：测量并记录 V1 的基线指标（清单见 `migration-matrix.md` 第 6 节）。基线未记录前不写业务代码。

### ADR-011 显示层：单一 WebView 透明置顶层（v1.2 修订）

**v1.1 曾设计「DirectX 只读展示 + WebView 交互」双轨。该设计在 v1.2 取消**，理由见 ADR-014：

1. 不注入游戏进程的前提下，**独占全屏下任何独立窗口都无法显示**（DX 与 WebView 同样受限），而窗口化/无边框全屏下两者都能显示——DX 因此没有额外能力；
2. 决策方明确产品形态是「独立 EXE + 读取聊天日志」，不需要渲染管线接管；
3. V1 的悬浮窗已具备**滚动、拖拽移动、边缘缩放、右键菜单**（tkinter 原生能力），这些在 WebView 里同样原生支持，而 DX 需从零实现。

**决策**：显示层只有一条路径——**native 承载的 WebView2 透明置顶窗口**。

| 功能 | 归属 | 理由 |
|---|---|---|
| 消息流显示（浮在游戏画面上方） | WebView（native 宿主） | 透明置顶，浏览器原生渲染 |
| 鼠标交互（**滚动 / 拖拽移动 / 边缘缩放 / 右键菜单**） | WebView | 与 V1 的 tkinter 行为对齐；浏览器原生，零额外代码 |
| **输入栏（中文输入）** | WebView | 中文 IME 由 WebView 原生支持 |
| 设置窗口 | WebView | 表单、字体、日志列表 |
| 系统托盘菜单 | C++ | 见 ADR-005 |

**中文 IME 约束（v1.1 提出的硬约束，在 v1.2 中自动满足）**：

> DirectX 自绘窗口要支持中文输入，必须自行处理 `WM_IME_*` 消息、组合字符串状态、候选词窗口定位与光标跟随。**显示层统一为 WebView 后，该风险归零**——这正是取消 DX 层最直接的收益之一。

`window_mode` 因此**回到 V1 的 2 值**，不需要扩展：

```
"standalone"  标准窗口（保留 V1 行为）
"overlay"     透明置顶悬浮窗，浮在游戏画面上方（保留 V1 行为，可交互）
```

**使用前提（必须写入用户文档）**：游戏需运行在**窗口化 / 无边框全屏**模式下。独占全屏下任何独立窗口都无法显示（ADR-014）。

### ADR-012 native 崩溃恢复协议

C++ 持有实际状态后，必须在架构上解决「native 崩溃怎么办」。

**模式：Go 持期望状态，C++ 持实际状态，崩溃后重放。**

```
Go 侧 CapabilityRegistry（期望状态）
  hotkeys:  [{id, combo, enabled}]
  tray:     {enabled, menu[], icon}
  display:  {mode, visible, bounds, opacity, clickThrough}
  cache:    {lastTranslation}

恢复流程：
  1. Go 通过心跳（间隔 1s，连续 3 次无响应判定失联）检测 native 存活
  2. native 失联 → Go 启动新 native 进程
  3. 收到 native.ready → Go 全量重放 CapabilityRegistry
  4. native 回报实际状态 → Go 与期望状态比对，差异记 WARN 日志
  5. Go 向前端广播 notice（"显示层已恢复"）
```

**不变量**：翻译链路（Go 侧 ingest → engine → 日志写入）在 native 崩溃期间**不中断**；只有显示与输入能力暂时降级。Go 侧需缓存 native 不可用期间产生的消息，恢复后重放。

### ADR-013 WebView2 由 native 宿主，前端退化为纯 SPA

**背景**：ADR-005 把全部 Windows 底层能力划给 C++，其中包含窗口封送（`native/window`）。若 WebView 窗口仍由前端框架（Wails / Tauri）创建，则窗口所有权落在 Go 侧，毛玻璃、鼠标穿透、拖拽、边缘缩放会出现「一个 HWND 两个所有者」的问题。

**决策**：

| 项 | 归属 | 说明 |
|---|---|---|
| 窗口创建与所有 Win32 窗口操作 | **native（C++）** | `CreateWindowEx` + `ICoreWebView2` 宿主；毛玻璃、穿透、拖拽、缩放全在 native |
| 前端资源（HTML/JS/CSS 构建产物） | **Go** | 经本机 HTTP（`127.0.0.1:<port>`）提供，端口由 Go 分配并传给 native |
| 前端与 Go 的通信 | **REST + WebSocket** | 见第 9 节；**不使用框架私有绑定**，因此前端是纯 SPA |
| 前端构建 | **Vite（TypeScript）** | 无 Wails / Tauri 依赖 |

**由此产生的收益**：

1. HWND 单一所有者，与 ADR-005 完全一致；
2. 前端不依赖任何桌面框架，纯粹是一个用 REST + WS 通信的网页应用，可独立开发与测试（甚至可在浏览器里对着 Go 调试）；
3. 工具链只需 **Go + C++(MSVC) + Node**，不含 Rust 与框架 CLI。

**代价**：

1. native 需实现 WebView2 宿主（`ICoreWebView2Environment` / `Controller` / `ICoreWebView2_2` 等），这是一段有学习成本的 COM 代码；
2. 失去 Wails 的开发模式便利（热重载、Go↔JS 直接调用）。**缓解**：前端开发期可直接在浏览器访问 Go 的 HTTP 服务，热重载由 Vite 提供，实际体验损失很小。

**备选方案（未采用，保留备查）**：保留 Wails 拥有 WebView 窗口，Go 侧为此保留一个**最小**的 `internal/platform/webviewwin`（仅服务自己那个窗口的毛玻璃/穿透/拖拽）。此时窗口操作分处两个进程，需明确「WebView 窗口归 Go，托盘与热键归 native」，不得交叉。

> **Q6 已决（v1.2）**：采用主方案——native 宿主 WebView2，前端为纯 SPA，不引入 Wails / Tauri。

**补充决定（v1.4）：SDK 不得成为构建硬依赖**

**背景**：本机（Windows 11 build 26200）只装了 **WebView2 Runtime** 152.0.4191.66，没有 SDK —— 全盘搜索无 `WebView2.h`，无 NuGet 全局缓存。若把 SDK 设为必需，`native/webview` 就编译不了，整个 `translator_native` 也跟着编译不了。这直接违反 ADR-005 的「装了 MSVC 就能编译通过」。

**决策**：

| 项 | 决定 |
|---|---|
| 构建依赖 | **不依赖 WebView2 SDK 头文件**。原生 COM 接口声明由我们自己写最小子集 |
| 运行时依赖 | `WebView2Loader.dll` 由 `LoadLibraryW` **动态加载**，缺失时不影响进程启动 |
| 能力上报 | 加载器或运行时缺失时，`capabilities` **不含** `webview`，Go 侧据此降级而不是假装可用 |
| 可选加速 | 若构建时检测到 `Microsoft.Web.WebView2` NuGet 包，则改用官方头文件；两条路径产出同样的能力名 |

**理由**：

1. WebView2 的 API 本来就是纯 COM（`IUnknown` 派生），官方 SDK 头文件只是声明，不含实现；我们实际需要的接口只有 `ICoreWebView2Environment` / `ICoreWebView2Controller` / `ICoreWebView2` 三个，加一个 `CreateCoreWebView2EnvironmentWithOptions` 导出函数，声明量约 250 行；
2. 保持 ADR-005 的「装了 MSVC 就能编译」约束不被破坏；
3. 降级路径本来就必须有（用户可能没装 Runtime），所以「缺能力就上报缺能力」不是额外成本，而是既有设计。

**代价**：

1. 需要自己维护那份 COM 声明，升级 WebView2 新接口时要同步；
2. 失去 SDK 里的辅助宏与 `EventRegistrationToken` 便利封装，需手写 `add_*` / `remove_*`。

**验证前置**：`tools/fetch-webview2-sdk.ps1` 可在有网络的机器上取回官方 SDK 作为**可选**加速路径与对照实现。本开发环境网络被阻断，故 `native/webview` 暂缓实现，先完成不依赖它的部分。


### ADR-014 禁止游戏进程注入与渲染 hook（安全硬约束）

**背景**：已核实 TruckersMP 官方规则（[truckersmp.com/rules](https://truckersmp.com/rules)，最后更新 2026-08-23）：

| 条款 | 原文要点 | 对本项目的影响 |
|---|---|---|
| §2.1 Hacking/Bug/Feature abusing | 「Using **any kind of tool** to change gameplay, including but not limited to using the in-game console, trainers or cheat engine in order to bypass the speed limiter, to jump hack, no collision hack or anything similar.」处罚为 **6 个月至永久封禁** | 规则约束的是**改变游戏玩法**（限速、跳跃、无碰撞）。只读的翻译悬浮窗不改玩法，不在该条字面范围内 |
| §1.11 Ambiguity | 「Any ambiguity falls on the side of TruckersMP, and **not individual players**」 | 措辞宽泛（any kind of tool），**歧义解释权在 TruckersMP**，用户无保护 |
| §5.2 Staff and TruckersMP | 「Staff reserve the right to **kick or ban you at any point** if needed」「everyone is a guest on the servers」 | 无论规则怎么写，工作人员保留随时处置的裁量权 |

**结论**：规则**没有**明确禁止只读悬浮窗，但也**没有**任何条款保护第三方工具。因此本项目采取「风险最小化」策略，把安全边界写死为硬约束：

**允许**（与 V1 做法同源，实际运行多年）：

- 读取 TruckersMP 客户端自己写在 `文档\ETS2MP\logs\` 的聊天日志文件
- 创建**独立进程的透明置顶窗口**显示译文
- 用 `SendInput` 模拟键盘输入到游戏聊天框（等同于玩家自己打字）
- 通过 `RegisterHotKey` 注册系统热键

**绝对禁止**：

- ❌ 向 `eurotrucks2.exe` 或 TruckersMP 进程注入 DLL
- ❌ Hook 游戏的 D3D / DXGI swapchain 或 Present 调用
- ❌ 读写游戏进程内存、修改游戏资源或存档
- ❌ 拦截或伪造游戏网络封包
- ❌ 绕过服务器端自动踢出系统（§2.1 明文禁止）

**重要逻辑推论**（直接决定 DirectX 渲染层的取舍）：

> **独占全屏（exclusive fullscreen）模式下，任何独立置顶窗口——无论用 DirectX 还是 WebView 渲染——都无法显示。** 想让译文在独占全屏下可见，**唯一**的技术路径是注入游戏进程 hook 渲染，而这正是 ADR-014 明文禁止的行为。

因此游戏内显示的可行前提是：玩家的 ETS2 处于**窗口化 / 无边框全屏**模式。该模式下独立置顶窗口可正常显示，且 ETS2 与 TruckersMP 玩家普遍使用此模式（V1 的 overlay 模式即依赖于此并已实际可用）。

### ADR-015 全局热键：RegisterHotKey 为主、轮询为降级（待确认）

**背景（逐文件阅读时发现，见 D10 / D11）**：V1 **实际在用的是轮询**，不是系统热键。

| 事实 | 证据 |
|---|---|
| 活的实现是轮询 | `overlay.py:773` `_start_hotkey_poller`：`GetAsyncKeyState` 每 50ms 轮询，上升沿触发呼出 |
| `RegisterHotKey` 代码存在但**从未接线** | `hotkey_manager.py` 的 `HotkeyManager` 全项目零引用（仅 `build_exe.py` 一行残留 `--hidden-import`） |
| 文档却宣称用的是系统热键 | `README.md:106`、`release_notes_v1.3.0` |

**决策建议**：V2 用 **`RegisterHotKey`（C++）为主，注册失败时降级为轮询**。

理由：

1. **这是把文档承诺的行为真正实现**，不是新增功能——用户可见的热键功能（按下呼出）完全不变，不违反 P1。
2. **省 CPU**：轮询每秒 20 次唤醒，是 B9「闲置 CPU 2.88%」的一部分；系统热键是事件驱动、零轮询。
3. **V1 的"轮询"不是设计决策而是遗漏**：`RegisterHotKey` 那一整套（隐藏消息窗口 + 消息循环 + `WM_HOTKEY`）已经写好，只是没被 `main.py` 接上。
4. **必须保留轮询兜底**：V1 的 `hotkey_manager` 里有一段「若 `RegisterHotKey` 失败且含 SHIFT，则改用 ALT 重试」的逻辑，说明作者遇到过注册失败；V2 应把兜底做完整（注册失败 → 降级轮询）。

**待确认点（Q9 / Q10）**：

- Q9：是否同意改用 `RegisterHotKey` 为主？（若坚持与 V1 逐位一致，则应沿用轮询）
- Q10：降级顺序如何定？候选：① `RegisterHotKey` 原组合 → ② SHIFT 换 ALT 重试（V1 已有）→ ③ 降级轮询。

**无论选哪个，README 必须与实现一致**（D11）。

### ADR-016 事件推送用 SSE 而非 WebSocket

**背景**：上游需求书与本文档 v1.3 都写的是 WebSocket。实现 API 层时重新评估：

| 维度 | WebSocket | SSE |
|---|---|---|
| 本场景的实际需求 | 只需**服务端 → 前端**单向推送（命令走 REST） | 同样满足 ✓ |
| 代码量 | 握手 + 帧解析 + 掩码 + close + ping/pong ≈ 250 行，或引入第三方库 | ≈ 40 行 |
| 浏览器端重连 | 需自己写 | `EventSource` **自带重连** |
| 依赖 | 我这边无法拉取模块，**无法验证** | 零依赖 |
| 传输 | 二进制帧协议 | 纯 HTTP 文本流 |

**决策**：用 SSE（`GET /api/events`）。

**代价（已确认可接受）**：
1. 单向——若将来需要前端主动推流给 Go，需另开 HTTP 上传接口；当前无此需求。
2. SSE 只有文本——事件载荷本就是 JSON，无影响。
3. 部分代理会缓冲 SSE——本项目只监听 `127.0.0.1`，无代理。

**同时明确**：API 只监听回环地址。这是本机单用户工具，不对外暴露；若将来需要局域网访问，
必须先加认证（当前**没有**任何认证机制）。

**回退方案**：若后续确认需要双向，可在不改动 REST 契约的前提下把 `/api/events` 换成
WebSocket（前端只需把 `EventSource` 换成 `WebSocket` 并保留同样的 `{type,payload}` 信封）。

---

## 4. 总体架构

### 4.1 分层图

```
┌──────────────────────────────────────────────────────────────────────┐
│  Frontend (TypeScript SPA + WebView2，宿主见 ADR-013)                  │
│  设置(Settings) · 输入栏(InputBar) · 悬浮窗(Overlay) · 日志 · 广告    │
│  仅呈现与输入，不持有业务状态                                          │
└───────────────────────────┬──────────────────────────────────────────┘
                            │ REST + WebSocket（本机回环）
┌───────────────────────────▼──────────────────────────────────────────┐
│  Go Core（业务大脑 + 期望状态持有者）                                  │
│                                                                      │
│   ingest/     Source 接口 ─ chatlog(现有) / screen / window / clipboard│
│        │                                                             │
│        ▼  Event                                                      │
│   engine/     normalize → 本地词典 → 缓存 → Provider竞速 → 后处理      │
│        │                                                             │
│        ▼  RenderedMessage                                            │
│   sink/       Sink 接口 ─ webview(经 native) / log                    │
│                                                                      │
│   task/       AdSender · SendPipeline · 定时清理                      │
│   config/     DPAPI · 原子写 · 3s 热重载 · V1 兼容                     │
│   capability/ CapabilityRegistry（热键/托盘/显示 的期望状态）          │
└───────────────────────────┬──────────────────────────────────────────┘
                            │ Named Pipe + 长度前缀 JSON（双向，含心跳）
┌───────────────────────────▼──────────────────────────────────────────┐
│  Native (C++，阶段 1 起在主链路)                                       │
│   webview/   WebView2 宿主（创建 HWND 并承载前端 SPA）                │
│   hotkey/    RegisterHotKey + 消息循环                                │
│   input/     SendInput + 剪贴板                                       │
│   tray/      Shell_NotifyIcon + 右键菜单                              │
│   window/    Win32 封送：透明置顶、毛玻璃、穿透、拖拽、单实例互斥体    │
└──────────────────────────────────────────────────────────────────────┘
```

### 4.2 Source / Engine / Sink 三段抽象

这是本架构的核心，也是「重新设计模块边界」的落点。三段之间只通过领域对象通信：

```
Source  ──Event──▶  Engine  ──RenderedMessage──▶  Sink
（来源）            （翻译）                       （呈现）
```

新增一个数据源 = 实现一个 Source；新增一个呈现方式 = 实现一个 Sink。**Engine 永不修改。**

对照 V1：`monitor.py` → `ingest/chatlog`；`translator.py` + `chat_dictionary.py` → `engine/` + `dictionary/`；`overlay.py` 的显示部分 → `sink/webview`；`overlay.py` 的发送编排 + `compose_sender.py` → `task/sendpipeline`；`overlay.py` 的窗口操作 → `native/window`。

### 4.3 进程与通信拓扑

| 参与者 | 生命周期 | 持有状态 |
|---|---|---|
| Go Core | 主进程，单实例互斥体（由 native 提供） | **全部业务状态 + 期望状态** |
| Frontend | 由 Go 启动的 WebView2 窗口 | 无（仅视图状态） |
| Native | 阶段 1 起的主链路子进程，由 Go 拉起并监督 | **Windows 侧实际状态**（HWND / 热键注册 / 托盘句柄 / 剪贴板） |

**启动顺序**（必须严格按序，否则热键与托盘注册会失败）：

```
Go 启动
  → 拉起 native 进程
  → native.hello 握手（校验 protocolVersion）
  → native.ready（回报可用能力清单与实际状态）
  → Go 依据 CapabilityRegistry 注册热键、创建托盘、创建显示层
  → Go 启动 ingest / engine / api
  → Go 启动 WebView 前端
```

---

## 5. 核心接口定义

以下为接口契约（示意，非最终代码）。

### 5.1 Source

```go
// Source 产出统一领域事件。Engine 不感知具体来源。
type Source interface {
    ID() string                                  // "chatlog" | "screen" | "window" | "clipboard"
    Start(ctx context.Context, out chan<- Event) error
    Stop() error
    Status() SourceStatus                        // 供前端展示：目录状态/日志文件名/错误
}
```

`chatlog` 实现需保留 V1 全部行为：两种文件名 glob（`chat_*_log.txt` 与 `chat_*_log_*.txt`）及兜底 `chat_*.txt`、按 mtime 取最新、切档检测、增量读取、启动时跳过历史消息、按「玩家+内容+时间戳」去重、自动识别玩家昵称与服务器名。

### 5.2 Sink

```go
type Sink interface {
    ID() string                                  // "webview" | "log"
    Deliver(ctx context.Context, msgs []RenderedMessage) error
    Update(ch Update) error                      // 统计、服务器名、语言标签等
    Close() error
    Available() bool                             // native 崩溃期间 webview sink 返回 false
}
```

`webview` sink 通过 IPC 投递到 native 的 WebView2 宿主窗口；`Available()` 返回 false 时 Go 侧消息进入待重放缓冲（ADR-012）。

### 5.3 Provider

```go
type Provider interface {
    ID() string
    Label() string
    Format() string                              // "openai" | "anthropic"
    Translate(ctx context.Context, req TranslateRequest) (TranslateResult, error)
    FetchModels(ctx context.Context) ([]string, error)   // GET /v1/models
    Test(ctx context.Context) (TestResult, error)        // 连通性 + 延迟
}
```

Provider 插件化：新增供应商 = 新增实现 + 在 `resources/providers.json` 增加预设，不修改引擎。V1 的 `ProviderConfig` 13 个字段（`id/label/endpoint/api_key/model/enabled/preset_id/icon/api_format/weight/extra_headers/extra_body/timeout`）**全部保留**，以保证配置兼容。

### 5.4 Dictionary

```go
type Dictionary interface {
    Lookup(text string) (string, bool)           // 入口拦截，命中则零 API 调用
    PromptMapping() string
    FixLeftoverShorthand(text string) string
    PreserveMentionPrefix(in, out string) string
    LooksUntranslated(in, out, targetLang string) bool
    NonTranslatable(text string) bool
}
```

五层语义必须逐层保留：第 1 层俚语 token（含 `pauseAfter` 标记）、第 2 层短语精确匹配、第 3 层结构化短语（动作 + 名字拼接）、第 4 层 prompt 内嵌映射、第 5 层 ETS2 专有词汇。

### 5.5 Cache 与 Task

```go
type Cache interface {
    Get(key string) (string, bool)
    Put(key, value string)
}
// 同文本并发合并由 singleflight 承担，不进入 Cache 接口

type Task interface {
    ID() string
    Start(ctx context.Context) error
    Stop() error
}
```

### 5.6 CapabilityRegistry（ADR-012 的核心）

```go
// Go 持有期望状态；native 只执行并回报实际状态
type CapabilityRegistry interface {
    SetDesiredHotkeys(list []HotkeySpec) error      // 幂等
    SetDesiredTray(menu []TrayItem) error           // 幂等
    SetDesiredDisplay(spec DisplaySpec) error       // 幂等
    CacheLastTranslation(text string)               // 供 native 本地处理复制热键
    Replay(ctx context.Context) error               // native 重启后全量重放
    ActualState() NativeState                       // 实际状态快照，用于差异告警
}
```

---

## 6. 领域数据模型

```go
// Event：Source 产出的原始事件
type Event struct {
    ID        string    // 去重键：speaker + text + timestamp
    Time      time.Time
    Timestamp string    // 原始 HH:MM:SS，用于 UI 右上角对齐显示
    Speaker   string    // 玩家名；系统消息为 "[Channel]"
    Text      string
    Lang      string    // 启发式检测结果
    Origin    string    // "chatlog" | "screen" | ...
    IsSelf    bool
    IsSystem  bool
}

// RenderedMessage：交给 Sink 呈现的成品
type RenderedMessage struct {
    ID          string
    Speaker     string
    Original    string
    Translated  string
    Lang        string
    Timestamp   string
    IsSelf      bool
    IsSystem    bool
    Provider    string   // 实际生效的 Provider label
    Model       string
    CacheState  string   // "local_dict" | "hit" | "miss"
    LatencyMs   int
    ErrCode     string   // 非空表示这是错误提示行
}

// Stats：对应 V1 的 TranslationStats
type Stats struct {
    Translated   int
    Cached       int
    SelfSkipped  int
}
// total = Translated + Cached + SelfSkipped
// savings% = (Cached + SelfSkipped) / total
```

---

## 7. 模块清单与职责

### 7.1 backend/（Go）

| 模块 | 职责 | 从 V1 迁移自 |
|---|---|---|
| `cmd/translator` | 进程入口、生命周期、native 监督、优雅退出 | `main.py`（`App`、`_shutdown`） |
| `internal/ingest` | Source 接口 + `chatlog` 实现 | `monitor.py` |
| `internal/engine` | 编排、批处理、混合语言拆分、语言检测、后处理 | `translator.py`（`Translator.run` / `_flush` / `_flush_llm` / `split_mixed_text` / `reassemble_mixed` / `detect_language`） |
| `internal/providers` | Provider 接口与注册表、竞速、轮转、熔断、模型拉取、连通测试 | `translator.py`（`_call_provider` / `_call_api_internal` / `ProviderHealth`）+ `model_fetcher.py` |
| `internal/dictionary` | 五层术语处理 + `resources/dictionary.json` 加载校验 | `chat_dictionary.py` |
| `internal/cache` | LRU 1000 + 同文本合并 | `translator.py`（`LRUCache` / `_in_flight`） |
| `internal/config` | 配置模型、DPAPI、原子写、热重载、V1 兼容与迁移 | `config.py` |
| `internal/logger` | 文件轮转（7 天 / 2MB）+ 内存环形缓冲 500 | `logger.py` |
| `internal/task` | 任务队列：`AdSender`、`SendPipeline` | `main.py`（`_ad_*`）+ `compose_sender.py` |
| `internal/sink` | Sink 接口 + `webview`（经 IPC）+ `log` | `overlay.py`（显示部分） |
| `internal/capability` | **CapabilityRegistry：期望状态 + 重放 + 差异告警** | 新增（ADR-012） |
| `internal/ipc` | Named Pipe 客户端、帧编解码、心跳、请求超时 | 新增 |
| `internal/api` | REST + WebSocket + DTO | 新增（替代 UI 直连） |
| `internal/domain` | 领域对象与错误码 | `message_types.py` + 新增 |

> **注意**：`internal/platform`（Go 直调 Win32）**已取消**——按 ADR-005，热键/输入/托盘/剪贴板全部下沉到 `native/`。Go 侧仅保留 DPAPI 与注册表读取（属配置业务数据，见 N3）。

### 7.2 native/（C++，阶段 1 起）

| 模块 | 职责 | 从 V1 迁移自 |
|---|---|---|
| `pipe/` | Named Pipe 服务端 + 帧编解码 + 心跳 | 新增 |
| `webview/` | **WebView2 宿主**：创建 HWND、承载前端 SPA、DPI 与缩放处理 | 新增（ADR-013） |
| `hotkey/` | `RegisterHotKey` + 消息循环（固定 OS 线程） | `hotkey_manager.py` |
| `input/` | `SendInput` 组合键 + 剪贴板读写 | `input_sender.py` |
| `tray/` | `Shell_NotifyIcon` + 右键菜单 + 图标生成 | `tray_icon.py` |
| `window/` | Win32 封送：透明置顶、毛玻璃、鼠标穿透、拖拽缩放、单实例互斥体 | `acrylic_helper.py` + `win32_constants.py` + `overlay.py`（窗口操作部分） |
| `include/translator_native.h` | 对外稳定 C ABI | 新增 |
| ~~`overlay/`~~ | ❌ **取消（v1.2）** DirectX 渲染层 | — |
| ~~`capture/`~~ | ❌ **取消（v1.2）** Windows Graphics Capture | — |

### 7.3 frontend/（TypeScript SPA + Vite）

| 目录 | 职责 |
|---|---|
| `src/api/` | REST + WS 客户端（唯一与 Go 通信的入口） |
| `src/components/` | `MessageList`、`MessageItem`、`InputBar`（**中文 IME 在此**）、`StatsBar`、`Header` |
| `src/pages/` | `Overlay`（悬浮/标准窗口）、`Settings` |
| `src/settings/` | `ProviderList`、`ProviderEditor`、`PresetPicker`、`ModelPicker`、`HotkeyCapture`、`AppearanceTab`、`LogsTab`、`AdsTab` |
| `src/store/` | 翻译流、统计、配置的视图状态 |
| `src/theme/` | 三层蓝色（`#4494FC` 主色 / `#60A8FF` 描边 / `#70B8FF` 高亮）+ 毛玻璃 |

### 7.4 resources/（数据）

`dictionary.json`、`providers.json`、`config.json`，schema 见第 10 节。

---

## 8. IPC 协议（Go ↔ C++）

### 8.1 帧格式

```
┌────────────────┬──────────────────────────────┐
│ 4 bytes LE     │ N bytes                      │
│ payload length │ UTF-8 JSON payload           │
└────────────────┴──────────────────────────────┘
```

长度上限 8 MB，超限即断开并记录错误。payload 顶层含 `type` 与 `id`（请求/响应配对）。**协议需同时支持三种模式**：请求/响应（`input.send`）、服务端推送事件（`event.hotkey`）、心跳（`ping`/`pong`）。

### 8.2 消息表

**Go → Native（指令）**

| type | 关键字段 | 说明 |
|---|---|---|
| `native.hello` | `protocolVersion` | 握手，版本不匹配则拒绝启动 |
| `native.replay` | `registry` | **全量重放期望状态（ADR-012）** |
| `hotkey.set` | `list[]{id, combo, enabled}` | 幂等设置热键集合 |
| `tray.set` | `{enabled, menu[]{id, label}, icon}` | 幂等设置托盘 |
| `display.create` | `mode`(`overlay`\|`standalone`), `bounds`, `opacity`, `clickThrough`, `zTop` | 创建显示层窗口（WebView2 宿主） |
| `display.update` | `messages[]`, `stats`, `header` | 增量更新 |
| `display.visible` | `visible` | 显示/隐藏 |
| `display.setClickThrough` | `enabled` | 鼠标穿透 |
| `input.send` | `text`, `hotkey`, `delayMs`, `confirm` | 模拟输入（含剪贴板写入） |
| `input.copy` | `text` | 写剪贴板 |
| `clipboard.cache` | `text` | **缓存最近一条译文，供复制热键本地使用** |
| `window.blur` | `hwnd`, `mode`(`mica`\|`acrylic`) | 毛玻璃 |
| `ping` / `shutdown` | — | 保活 / 退出 |

**Native → Go（事件）**

| type | 关键字段 | 说明 |
|---|---|---|
| `native.ready` | `protocolVersion`, `capabilities[]`, `actualState` | 启动完成，回报可用能力与**实际状态** |
| `event.hotkey` | `id` | 热键触发 |
| `event.trayCommand` | `id` | 托盘菜单项被点击（`toggle`/`switchMode`/`clickThrough`/`settings`/`quit`） |
| `event.inputResult` | `ok`, `reason` | 输入模拟结果 |
| `event.displayReady` | `hwnd` | 显示层窗口就绪 |
| `event.stateChanged` | `actualState` | 实际状态变化（供差异比对） |
| `event.error` | `code`, `message` | 异常上报 |
| `pong` | `seq` | 心跳应答 |

### 8.3 托盘菜单的归属划分

托盘由 C++ 显示，**但决策权在 Go**（业务状态在 Go）：

```
用户点击托盘菜单项
  → native 弹出菜单（本地，低延迟）
  → 用户选择 → event.trayCommand{id}
  → Go 决策：显示/隐藏？（Go 通知 display.visible）
             切换模式？（Go 改配置 + 广播 config.reloaded）
             打开设置？（Go 唤起 WebView 窗口）
             退出？（Go 走优雅关闭）
```

---

## 9. API 契约（Frontend ↔ Go）

### 9.1 REST

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/translate` | 单条翻译（接收方向或发送方向） |
| `POST` | `/api/send` | 发送链路：翻译 → 输入 → 日志确认 |
| `GET` | `/api/health` | 存活 + Source/Sink/Provider/native 健康快照 |
| `GET` `PUT` | `/api/config` | 读取 / 保存配置（`PUT` 后触发变更广播） |
| `GET` `POST` `PUT` `DELETE` | `/api/providers` | Provider 增删改查、启用、排序（`weight`） |
| `POST` | `/api/providers/{id}/test` | 连通性测试（返回延迟） |
| `GET` | `/api/providers/{id}/models` | 拉取模型列表 |
| `GET` | `/api/presets` | 内置预设（按 `category` 分组） |
| `GET` `PUT` | `/api/dictionary` | 词库总览 / 写入用户覆盖层 |
| `GET` | `/api/logs` | 日志列表与内容 |
| `DELETE` | `/api/logs` | 删除日志 |
| `GET` | `/api/stats` | 翻译统计 |

`POST /api/translate` 在上游需求书基础上扩展：

```jsonc
// 请求
{
  "text": "hello",
  "source": "auto",              // "auto" 或具体语言代码
  "target": "zh",
  "mode": "receive",             // "receive" 接收翻译 | "send" 发送翻译
  "bypassCache": false
}
// 响应
{
  "id": "evt_01H...",
  "translation": "你好",
  "provider": "OpenAI",
  "model": "gpt-4o-mini",
  "latencyMs": 200,
  "cache": "miss",               // "local_dict" | "hit" | "miss"
  "detectedSource": "en"
}
```

### 9.2 事件推送 `GET /api/events`（SSE，见 ADR-016）

`Content-Type: text/event-stream`，每帧 `data: {"type":...,"payload":...}\n\n`，
每 15 秒发一行 `: keep-alive` 心跳。前端用 `EventSource` 消费，**自带断线重连**。

| 事件 | 载荷 | 触发时机 |
|---|---|---|
| `hello` | `{sse:true}` | 连接建立（前端据此把状态切到「已连接」） |
| `message.translated` | `RenderedMessage` | 每完成一条翻译 |
| `message.sent` | `{chinese, translated, result}` | 发送链路完成 |
| `stats.updated` | `Stats` | 统计变化 |
| `provider.health` | `{id, failures, cooling}` | 熔断状态变化 |
| `log.appended` | `{level, module, text}` | 新日志 |
| `source.status` | `{server}` | 服务器名等来源状态变化 |
| `config.reloaded` | `{changedKeys}` | 配置保存后 |
| `native.status` | `{alive, capabilities[], degraded[]}` | native 存活/降级（ADR-012） |
| `notice` | `{text, level, durationMs}` | 需要弹提示 |

> 订阅者缓冲 256 帧，**满了丢弃**——与 ingest 队列同策略，绝不因为前端慢而拖住翻译主流程。

### 9.3 错误码（必须与 V1 文案一致）

V1 `translator.py:_format_error` 定义的分类必须在 V2 中逐条保留，前端展示文案不变：

| code | V1 原文案 | 触发条件 |
|---|---|---|
| `NETWORK` | `[网络错误] 无法连接到 API 服务器，请检查地址和网络` | 连接失败 |
| `TIMEOUT` | `[请求超时] API 服务器响应超时，请稍后重试` | 超时 |
| `AUTH_FAILED` | `[认证失败] API 密钥无效，请检查设置` | HTTP 401 |
| `FORBIDDEN` | `[权限不足] 无权访问该 API，请检查密钥权限` | HTTP 403 |
| `RATE_LIMITED` | `[请求过于频繁] 请稍后重试` | HTTP 429 |
| `SERVER_ERROR` | `[服务器错误 {code}] API 服务器异常，请稍后重试` | 500 / 502 / 503 |
| `HTTP_ERROR` | `[HTTP 错误 {code}] {reason}` | 其他状态码 |
| `BAD_RESPONSE` | `[响应格式错误] API 返回了意外的数据结构` | 结构异常 / JSON 非法 |
| `TRANSLATE_FAILED` | `[翻译失败] {detail}` | 兜底 |
| `ALL_PROVIDERS_FAILED` | `所有 Provider 发送翻译失败` | 发送方向全部失败 |

API 响应中的 `message` 可附带 Provider 返回的细节，等价于 V1 的 `_parse_api_error`（` — {msg}`）。

---

## 10. 数据层设计

### 10.1 resources/dictionary.json

```jsonc
{
  "schemaVersion": 1,
  "meta": { "source": "builtin", "updatedAt": "2026-08-15" },
  "slang": {
    "wtf": { "text": "什么鬼", "pauseAfter": true },
    "pls": { "text": "请", "pauseAfter": false }
  },
  "phrases": { "thank you": "谢谢", "gute reise": "一路顺风", "o/": "挥手" },
  "structuredActions": { "rec": "已录屏", "report": "举报", "ban": "封禁", "kick": "踢出" },
  "systemMessages": [
    { "patterns": ["cannot connect to server", "can't connect to server"], "text": "无法连接到服务器，可能是网络连接问题。" }
  ],
  "ets2Terms": { "truck": "卡车", "trailer": "挂车", "no collision": "无碰撞区" },
  "promptMapping": "sry=抱歉, ty/thx=谢谢, rec=已录屏, wtf=什么鬼"
}
```

`pauseAfter` 必须保留——它影响结构化短语的拼接边界，丢失会导致「rec ban」这类消息译文错位。

### 10.2 resources/providers.json

镜像 `ProviderPreset` 全部 15 个字段（`id/name/website_url/api_key_url/endpoint/api_format/default_model/models_url/icon/icon_color/category/template_headers/template_body/description/recommended`）。`endpoint` 存 **base URL**（不带 `/chat/completions`），由 `build_endpoint()` 等价逻辑拼接，与 V1 行为一致（`anthropic` 拼 `/v1/messages`，其余拼 `/v1/chat/completions`）。

### 10.3 resources/config.json

默认配置模板 + `configVersion` 字段。V2 需能读入 V1 的 `config.json`（含 `version: 2`），并在读入 v1 格式时执行等价迁移（扁平字段 → `llm_providers` 数组）。`window_mode` 需扩展为四值（见 ADR-011）。

### 10.4 加载与合并策略

```
加载顺序（后者覆盖前者）：
  1. resources/dictionary.json        （内置，只读）
  2. <data>/resources/dictionary.json （在线更新，可被覆盖）
  3. <data>/resources/override/*.json （用户覆盖，永不参与更新）
```

`<data>` = `%LOCALAPPDATA%\ETS2 Translator\`（V1 的 `_FALLBACK_CONFIG_DIR` 同址）。

**校验**：加载时校验 `schemaVersion`；条目级非法项跳过并记 WARN 日志，不使整个词库失效。

---

## 11. 并发与任务模型

| 环节 | 模型 | V1 对应语义（必须保留） |
|---|---|---|
| 日志采集 | 每 Source 1 个 goroutine，500ms 轮询 | `monitor.py` 0.5s 轮询 |
| 事件通道 | 带缓冲 channel，容量 500 | `Queue(maxsize=500)`（满则丢弃并计数） |
| 批处理 | 300ms 窗口收集，满 8 条立即 flush | `BATCH_WINDOW=0.3`，`len(batch)>=8` |
| 批量分隔符 | `\n---\n`；LLM 未按分隔符回显时**逐条回退翻译** | 修复过的缺陷，不可回退 |
| 同文本合并 | `singleflight` | `_in_flight` / `_in_flight_results` |
| 缓存 | LRU 1000 | `CACHE_SIZE=1000` |
| Provider 竞速 | 全部启用 Provider 并行，**先返回通过 `looks_untranslated` 校验的译文胜出** | v2.3.1 的核心能力 |
| ~~轮转分流~~ | ❌ **不存在**：`_rr_index` 是残留变量，V1 只有竞速（见 D12）。实现轮转等于新增功能，违反 P1 | — |
| 熔断 | 每 Provider 连续 3 次失败进入冷却，`min(30 * 2^(n-3), 120)` 秒，成功即恢复 | `_note_provider_result` |
| 配置热重载 | 3 秒 mtime 轮询，变更则重载并广播 | 语义等价，实现可换 fsnotify |
| 任务队列 | `AdSender`（定时轮播）、`SendPipeline`（互斥，忙时返回 `BUSY`） | `_ad_*` + `ComposeSender._busy_lock` |
| **热键消息循环（C++）** | **独立线程 + `GetMessage`；不阻塞 IPC 读线程** | `hotkey_manager.py`（独立线程 + 消息窗口） |
| **IPC 心跳（Go）** | 1s 间隔，连续 3 次无 `pong` 判定 native 失联 | 新增（ADR-012） |
| **待重放缓冲（Go）** | native 不可用期间的 `RenderedMessage` 入缓冲，恢复后重放 | 新增（ADR-012） |

`sink` 投递失败不得阻塞 `engine`：投递队列满时丢弃最旧消息并计数，保证翻译链路不被显示层卡死。

---

## 12. 错误处理与降级

| 场景 | 处理 | 不变量 |
|---|---|---|
| 单个 Provider 失败 | 记入熔断计数，继续竞速/轮转 | 不影响其他 Provider |
| 全部 Provider 失败 | 该条消息以 `ALL_PROVIDERS_FAILED` 文案显示 | 消息不丢失，用户可见 |
| 配置读取损坏 | 备份为 `.corrupted.{timestamp}` 后重建默认配置 | 保留 V1 行为 |
| DPAPI 解密失败 | **保留原密文值，不写空串**，标记该字段待重填 | 防止密钥被永久覆盖（V1 `test_phase1_config_fixes.py` 已覆盖此用例） |
| 主配置目录不可写 | 回退 `%LOCALAPPDATA%\ETS2 Translator\` | 保留 V1 行为 |
| 日志目录不存在 | 内存缓冲可用，UI 提示 `目录不存在` | 保留 V1 `log_dir_status` 诊断文案 |
| **native 崩溃** | **Go 重启 native → 重放期望状态 → 广播 `native.status`；翻译链路不中断** | 见 ADR-012；热键/托盘/显示短暂降级 |
| **native 多次重启失败** | 停止重试（上限 3 次），降级为「仅 WebView 显示」，热键与托盘不可用，前端明确提示 | 翻译仍可用 |
| IPC 超时（单次调用） | 返回错误，不阻塞 engine；热键类调用记 WARN | 不因 IPC 卡死翻译 |
| 前端 WebView 崩溃 | Go 重建窗口，状态从内存重放 | 翻译不中断 |

---

## 13. 目录结构（最终目标）

```
Translator-V2/
├── backend/                          # Go
│   ├── cmd/translator/main.go
│   ├── internal/
│   │   ├── domain/       ingest/     engine/      providers/
│   │   ├── dictionary/   cache/      config/      logger/
│   │   ├── task/         sink/       capability/  ipc/
│   │   └── api/
│   ├── go.mod
│   └── main_test.go
├── native/                           # C++（阶段 1 起在主链路）
│   ├── CMakeLists.txt
│   ├── include/translator_native.h
│   ├── src/{pipe,webview,hotkey,input,tray,window}/
│   └── tests/
├── frontend/                         # TypeScript SPA + Vite
│   ├── src/{api,components,pages,settings,store,theme}/
│   └── index.html
├── resources/
│   ├── dictionary.json
│   ├── providers.json
│   └── config.json
├── docs/
│   ├── architecture.md               # 本文档
│   └── migration-matrix.md
└── tests/
    ├── backend/    native/    frontend/
```

---

## 14. 迁移阶段与验收

> v1.2：原阶段 4（DirectX 渲染层）**已取消**（ADR-011 / ADR-014）。阶段压缩为 0–4；WebView2 宿主与前端内容合并到阶段 3 一起交付。

| 阶段 | 内容 | 完成判据 |
|---|---|---|
| **0. 基线与工具链** | 记录 V1 基线指标（清单见对照表第 6 节）；安装 **Go SDK + VS Build Tools（含 C++ 工作负载）+ Node** | 基线表全部字段有实测值；`go` / `cl` / `npm` 均可执行 |
| **1. Go 核心 + native 骨架** | Go：迁 `config` + `logger` + `cache` + `dictionary`（无 UI、无状态、可纯单测）；定义 Source/Sink 接口与 `chatlog` 实现；`capability` 期望状态与重放。C++：`pipe` 帧编解码 + 心跳 + `hotkey` + `tray` + `window`（单实例互斥体） | V1 全部 99 个用例的等价 Go 测试通过；热键可注册触发；托盘菜单可弹出并把 `event.trayCommand` 送到 Go；**杀掉 native 后 Go 自动重启并重新注册热键**（ADR-012 验收） |
| **2. Engine + Providers + Task + 输入链路** | Go：翻译引擎、Provider 竞速/轮转/熔断、批处理、混合语言拆分、后处理；`AdSender` 与 `SendPipeline` 入任务队列。C++：`input`（`SendInput` + 剪贴板） | 端到端：真实聊天日志 → 译文出现；中文输入 → 游戏内出现译文；**关闭设置窗口后 `AdSender` 仍在运行**（V1 缺陷已修） |
| **3. Frontend + WebView2 宿主** | C++：`webview` 宿主 + `window`（透明置顶、毛玻璃、穿透、拖拽缩放）。TS：SPA——设置页 + 输入栏（**中文 IME 在此**）+ 透明置顶悬浮窗；删除 V1 双份设置实现 | 阶段 0 基线指标不劣化超过约定阈值；中文输入法在输入栏正常工作；悬浮窗在窗口化/无边框全屏下浮于游戏之上且可交互（点击复制、滚动、悬停） |
| **4. 移除 Python** | 删除 `ets2-translator/` 运行代码，保留为历史归档 | 全部 G1–G6 达成 |

**阶段 1 的实现约束**：不要做成「Python 调 Go、Go 再回调 Python」的双向纠缠——那会立刻引入双份配置、双份日志、进程生命周期管理三个坑。按能力切片迁移（先搬无 UI 无状态的模块），Python 侧只保留 UI 与游戏交互，通过本机 HTTP 调用 Go。

---

## 15. 风险与缓解

| ID | 风险 | 影响 | 缓解 |
|---|---|---|---|
| R1 | **三套工具链（Go/C++/TS）维护成本，单人开源项目负担重** | **高** | C++ 在 v1.2 已收缩为**纯 Win32 能力层**（无 DirectX、无捕获、无文字排版、无命中检测），代码量约 1345 行 + WebView2 宿主。缓解：严格守住 N3（业务逻辑不进 C++）；按 ADR-005「C++ 组织方式约束」以小而自证的单元组织，适配「AI 生成 + 人工排错」模式。**若未来仍感负担过重，砍掉 C++ 层是纯减法**（Go 可用 `golang.org/x/sys/windows` 平替全部能力） |
| R2 | ~~C++ DirectX 调试成本远高于 Python~~ | — | ✅ **v1.2 已消除**（DirectX 层取消） |
| R3 | **WebView2 内存占用可能高于 tkinter**，「降低内存」目标落空 | 中 | 阶段 0 实测 V1 RSS；若劣化明显，复核 ADR-003 |
| R4 | TruckersMP 日志格式变更 | 高 | 沿用 V1 的双 glob 回退 + 正则匹配 + `log_dir_status` 诊断提示（**按 P1 不新增**格式探测告警等新机制） |
| R5 | 在线词库更新冲掉用户自定义 | 中 | ADR-006 三层合并，用户覆盖层永不参与更新 |
| R6 | V1 用户配置（含 DPAPI 密文）无法被 V2 读取 | 中 | ADR-008 明确 V2 必须兼容读取 V1 配置与 `dpapi:` 密文 |
| R7 | 数据外置后启动期 JSON 解析失败导致词库整体失效 | 中 | 条目级校验，非法项跳过并 WARN，不整体失效 |
| R8 | 迁移中功能静默丢失 | 高 | 对照表逐条验收；阶段 0 基线作为「不劣化」判据 |
| **R9** | **native 有状态后崩溃导致热键/输入/托盘全失效，产品等价变砖** | **高** | ADR-012 期望状态重放 + 心跳 + 重启上限 3 次；翻译链路与显示解耦，native 不可用时仍可翻译并缓冲显示 |
| **R10** | ~~误在 DirectX 自绘层做中文输入，陷入 IME 泥潭~~ | — | ✅ **v1.2 已消除**（无 DX 层，输入栏天然在 WebView） |
| **R12** | **独占全屏模式下悬浮窗完全不可见，用户会当作 bug** | **中** | V1 同样受此限制（非 V2 引入）。缓解：在用户文档与 FAQ 写明「请将游戏设为窗口化/无边框全屏」（ADR-014 使该限制无法通过技术手段绕过）。**按 P1 不新增应用内提示** |
| **R13** | **HTML 注入：网页里的 V1 没有这个攻击面** | **中** | V1 用 tkinter `Text` 渲染，天然无 HTML 注入；换 WebView 后出现了，而聊天内容与玩家名全部来自联机服务器的**不可信输入**。缓解：渲染动态数据一律 `textContent`，禁用 `innerHTML`/`insertAdjacentHTML`/`outerHTML`；需要富文本时逐节点构造。前端 Lint 可加规则强制 |
| **R11** | 热键/输入经 IPC 后响应变慢 | 低 | 本机 Named Pipe 往返约 0.1~0.5ms，远低于感知阈值；复制热键由 native 本地缓存处理（ADR-005） |

---

## 16. 待决问题（Open Questions）

| ID | 问题 | 状态 |
|---|---|---|
| ~~Q1~~ | ~~是否要做「游戏画面内的 DirectX 渲染」？~~ | ✅ **已决（v1.2 反转）**：**不做**。产品定性为「独立 EXE + 读取聊天日志」，显示走透明置顶窗口；DirectX 层取消（ADR-011 / ADR-014） |
| ~~Q2~~ | ~~V2 是否要求直接复用 V1 的配置与日志目录？~~ | ✅ **已决：「可以重新配置」**。V2 使用全新配置目录，**不写 V1 配置兼容层**，用户重填 API Key。注意：TMP 聊天日志目录（`文档\ETS2MP\logs\`）是游戏产出，与 V1 无关，**不受此决策影响** |
| ~~Q3~~ | ~~OCR / 视频 / 语音翻译哪些是确定路线？~~ | ✅ **已决：「都不做，先把现有的做好」**。Source 只实现 `chatlog`；`native/capture`（WGC）**不实现**；`Event` 不预留帧时间戳 / 区域坐标 / 音轨字段 |
| Q4 | 是否有多人协作（决定是否上 CI、代码规范、PR 流程） | 待定。影响工程化投入（三套工具链的 CI 成本更高） |
| Q5 | 是否保留 `docs/superpowers/` 的 spec + plan 工作流用于 V2 | 待定。影响文档组织方式 |
| ~~Q6~~ | ~~WebView2 宿主由 native 还是 Wails 承担？~~ | ✅ **已决：native（C++）宿主 WebView2**，前端为纯 SPA |
| ~~Q7~~ | ~~在不能注入的前提下是否仍要做 DirectX 渲染层？~~ | ✅ **已决：不做**。决策方明确产品形态为「独立的 EXE，读取聊天日志」，DirectX 层取消，阶段 4 一并取消 |
| ~~Q8~~ | ~~是否需要为「AI 生成 + 人工排错」进一步压缩 C++ 复杂度？~~ | ✅ **已决**：见 ADR-005「C++ 组织方式约束」四条要求 |
| **Q9** | **全局热键用 `RegisterHotKey` 还是沿用 V1 实际在用的轮询？** 见 ADR-015 | **待定，需决策**（影响 V1 用户升级后的手感与闲置 CPU） |
| **Q10** | **`RegisterHotKey` 若在实机被占用（V1 代码里有 SHIFT→ALT 的降级逻辑，说明遇到过），降级顺序如何定？** | 待定，与 Q9 一起决策 |
