# V1 → V2 能力迁移对照表

| 项目 | 内容 |
|---|---|
| 文档版本 | **v1.3** |
| 日期 | 2026-08-15 |
| 配套文档 | `docs/architecture.md`（架构设计 v1.3） |
| V1 代码基线 | `ets2-translator` v2.3.1 — 生产 **7859 行** / 测试 **1267 行** / **99** 个用例 |
| 本表地位 | **V2 的验收基线。** 表中每一行都必须在 V2 有明确归属，否则视为功能丢失 |

**v1.1 修订记录**

| 变更 | 说明 |
|---|---|
| 归属变更 | 按 ADR-005，`hotkey_manager` / `input_sender` / `tray_icon` / `win32_constants` 由 Go 改为**下沉 native（C++）**；`acrylic_helper` 拆分 |
| 取消模块 | Go 侧 `internal/platform` **已取消**（Go 仅保留 DPAPI 与注册表读取，属配置业务数据） |
| 新增能力行 | W22（输入栏中文 IME）、W23（游戏内 DirectX 渲染） |
| 阶段重排 | C++ 自阶段 1 起在主链路；DirectX 渲染层单独排阶段 4 |

**v1.2 修订记录**

| 变更 | 说明 |
|---|---|
| **产品形态定性** | 翻译器为**独立 EXE，读取聊天日志**；不注入游戏、不做画面捕获、不做渲染管线接管 |
| **DirectX 层取消** | `native/overlay` 取消，原阶段 4 取消，阶段压缩为 0–4；W23 由「渲染进游戏画面」改为「透明置顶悬浮窗 + 鼠标交互」 |
| C++ 范围收缩 | `native/` 移除 `overlay` 与 `capture`，新增 `webview`（WebView2 宿主）；收敛为纯 Win32 能力层 |
| 配置兼容归零 | Q2 已决「可以重新配置」→ 不写 V1 配置兼容层；`api_endpoint`/`api_key`/`api_model` 三个遗留字段不再迁移 |
| 扩展方向收敛 | Q3 已决「都不做」→ `capture` 不实现；`Event` 不预留帧/音频字段；视频/语音翻译转为「不实现」 |
| 新增安全约束 | ADR-014：禁止任何形式的游戏进程注入与渲染 hook；新增 W24（独占全屏限制提示） |
| 基线调整 | B14 由「DirectX/WGC 帧率影响」改为「悬浮窗对游戏帧率影响」 |

**v1.3 修订记录**

| 变更 | 说明 |
|---|---|
| **新增首要原则 P1** | **功能对齐**：V2 功能范围 = V1 功能范围，不多不少。见下方第 0 节 |
| **撤销两项多写的功能** | v1.2 的 W23 含「点击消息复制」「消息悬停高亮」。经代码核查 **V1 均无此功能**，按 P1 撤销 |
| W24 取消 | 「独占全屏限制提示」属新增，按 P1 取消；改为写入用户文档（V1 同样受此限制） |
| L9 降级 | `native.status` 改为**内部机制**，不在 UI 新增展示 |
| 补充遗漏的 V1 能力 | 新增 W25：消息区右键菜单（Settings / Exit）—— V1 已有，v1.2 遗漏 |

---

## 0. 首要原则：功能对齐（P1）

> **V2 的功能范围 = V1 的功能范围，不多不少。**

- **不新增** V1 没有的用户可见功能。更换语言与架构不是加功能的理由。
- **不遗漏** V1 已有的任何用户可见能力；第 3 节是逐条清单。
- 本表中凡标注「**新增**」的用户可见条目一律视为违规，必须删除或改写。
- 唯一例外是**不可见的内部机制**（如 ADR-012 的期望状态重放、心跳、待重放缓冲）。它们不改变用户可见行为，且是「C++ 独立进程」这一分层带来的必要配套。此类条目一律显式标注「**内部机制**」。
- 第 7 节记录的 V1 既有缺陷属于**修复**（把本该有的行为修对），不算新增功能。

### 0.1 V1 悬浮窗实际交互清单（对齐基准）

经代码核查（`overlay.py`），V1 的悬浮窗交互仅下列 5 项，V2 必须逐项对齐且**不得超出**：

| # | 交互 | V1 证据 |
|---|---|---|
| 1 | 滚动消息 | `ttk.Scrollbar`（`overlay.py:235`）+ 滚轮 |
| 2 | 拖拽移动窗口 | `_on_mouse_down` / `_on_mouse_move`（`overlay.py:496/508`） |
| 3 | 边缘缩放窗口 | `_edge`（`overlay.py:477`） |
| 4 | 消息区右键菜单（仅 Settings / Exit） | `_build_context_menu`（`overlay.py:344`），**只有 2 项** |
| 5 | 热键复制译文到剪贴板 | `copy_hotkey`（`config.py:209`） |

**明确不做的（V1 没有）**：点击消息复制、消息悬停高亮、消息文字选中（`overlay.py:227` 为 `state=tk.DISABLED`，禁用文本无法选中拖选）。

---

## 1. 使用方式

- 阶段 0 先填第 6 节的基线表（当前仅 1 项有实测值，**未填完不得开始写业务代码**）。
- 每完成一个 V2 模块，回到第 2、3 节勾掉对应行，并核对第 5 节的测试用例已等价迁移。
- 第 7 节是本次核对中发现的 V1 既有缺陷，**V2 必须修掉，不得原样搬运**。
- 第 8 节是明确不迁移项，防止把已废弃实现一起搬过去。

---

## 2. 表 1：V1 模块 → V2 归属

> ⚠️ **本表的归属是粗粒度初版，已被逐文件细分判定取代。**
> 见 [`language-assignment.md`](language-assignment.md) —— 那里按「读每一个文件、判断它到底在干什么、
> 再决定用哪种语言」的原则，把 8 个混合文件拆到了职责级。
> 关键差异：整文件归属会把**发送时序、菜单决策、热键解析**这些非 Win32 的逻辑错误地塞进 C++，
> 让 C++ 层从「纯能力层」膨胀成「能力 + 业务混合层」——那正是 V1 的问题换个语言重演。

| # | V1 模块 | 行数 | 核心职责 | V2 归属 | 语言 | 阶段 |
|---|---|---|---|---|---|---|
| 1 | `main.py` | 1496 | `App` 主控、单实例互斥体、托盘接线、`SettingsDialog`（5 个 Tab，约 1135 行）、广告发送器、`main()` | `cmd/translator` + `internal/task` + `internal/capability` + `native/{tray,window}` + `frontend/settings` | Go + C++ + TS | 1–3 |
| 2 | `overlay.py` | 1017 | 悬浮窗 UI、消息渲染、输入栏、热键轮询、**发送编排**、位置记忆 | `internal/sink` + `frontend/pages/Overlay` + `internal/task/sendpipeline` + `native/{window,input}` | Go + C++ + TS | 2–3 |
| 3 | `settings_ui.py` | 822 | 新版设置窗口（`SettingsWindow` / `ProviderEditPanel` / `PresetSelectorDialog` / `ProviderListPanel` / `ModelFetchDialog`） | `frontend/settings` | TS | 3 |
| 4 | `translator.py` | 939 | 引擎：批处理、缓存、竞速、轮转、熔断、混合语言拆分、语言检测、后处理、连通测试、发送翻译 | `internal/engine` + `internal/providers` + `internal/cache` | Go | 1–2 |
| 5 | `config.py` | 401 | `AppConfig`(29 字段)、`ProviderConfig`(13 字段)、DPAPI、原子写、回退路径、v1→v2 迁移、注册表取文档目录 | `internal/config`（DPAPI 与注册表读取**留在 Go**） | Go | 1 |
| 6 | `chat_dictionary.py` | 399 | 五层术语处理（约 110 条俚语 + 短语 + 结构化 + 系统消息 + ETS2 词汇） | `internal/dictionary` + `resources/dictionary.json` | Go + JSON | 1 |
| 7 | `input_sender.py` | 341 | `SendInput` 键盘模拟、剪贴板读写、`send_chat_message`、`run_send_sequence`、`simulate_hotkey` | **`native/input`** | **C++** | 2 |
| 8 | `tray_icon.py` | 324 | 托盘图标（`Shell_NotifyIcon`）与右键菜单 | **`native/tray`** | **C++** | 1 |
| 9 | `provider_presets.py` | 304 | `ProviderPreset`(15 字段)、20 组预设、图标映射、4 个分类 | `resources/providers.json` + `internal/providers/presets` | JSON + Go | 1 |
| 10 | `monitor.py` | 267 | **聊天日志 tail（唯一数据源）**、正则解析、昵称/服务器识别、目录诊断 | `internal/ingest/chatlog` | Go | 1 |
| 11 | `acrylic_helper.py` | 261 | Win11 Mica / Win10 Acrylic（`DwmSetWindowAttribute` + `SetWindowCompositionAttribute`），含系统版本探测与回退 | **`native/window`**（native 持有的窗口）；WebView 窗口的毛玻璃见 ADR-013 | **C++** | 1 |
| 12 | `message_display.py` | 244 | 消息渲染（双行原文/译文、语言标签、时间戳右对齐、分割线） | `frontend/components/MessageItem` | TS | 3 |
| 13 | `logger.py` | 238 | 文件轮转（7 天 / 2MB）、内存环形缓冲 500、`translation_log` | `internal/logger` | Go | 1 |
| 14 | `win32_constants.py` | 216 | 常量、结构体、`vk_code`、`mod_vk` | **`native/window`**（Win32 常量与结构体定义） | **C++** | 1 |
| 15 | `hotkey_manager.py` | 203 | `RegisterHotKey` 系统热键 + 隐藏消息窗口 | **`native/hotkey`** | **C++** | 1 |
| 16 | `compose_sender.py` | 181 | `SendResult`(5 态)、`ComposeSender`（校验→发送→日志回读确认→剪贴板恢复） | `internal/task/sendpipeline`（编排在 Go，执行经 IPC 到 native） | Go + C++ | 2 |
| 17 | `model_fetcher.py` | 175 | `fetch_models`、`test_connectivity`、`FetchResult` | `internal/providers` | Go | 2 |
| 18 | `message_types.py` | 31 | `DisplayMessage`、`TranslationStats` | `internal/domain` | Go | 1 |
| | **合计** | **7859** | | | | |

**归属统计**（按 V1 文件的「可归属纯度」划分，避免虚构精度）：

| 类别 | 行数 | 说明 |
|---|---|---|
| **纯 Go 业务模块** | **2232** | `translator.py` 939 + `config.py` 401 + `monitor.py` 267 + `logger.py` 238 + `compose_sender.py` 181 + `model_fetcher.py` 175 + `message_types.py` 31 → 100% 迁 Go |
| **纯 C++ 能力模块** | **1345** | `input_sender.py` 341 + `tray_icon.py` 324 + `acrylic_helper.py` 261 + `win32_constants.py` 216 + `hotkey_manager.py` 203 → 100% 迁 native |
| **混合模块（需按职责拆分）** | **4282** | `main.py` 1496 + `overlay.py` 1017 + `settings_ui.py` 822 + `chat_dictionary.py` 399 + `provider_presets.py` 304 + `message_display.py` 244 → 拆到 Go / C++ / TS / JSON |
| 合计 | **7859** | |

混合模块的拆分原则：**UI 呈现 → TS**；**业务编排与术语处理函数 → Go**；**窗口与输入操作 → C++**；**词典与预设数据 → JSON**。`chat_dictionary.py` 与 `provider_presets.py` 属「数据 + 纯函数」结构，数据进 JSON、函数留 Go。

---

## 3. 表 2：用户可见能力 → V2 归属（防回归主清单）

### 3.1 核心翻译

| # | 能力 | V1 证据 | V2 归属 | 阶段 | 验收方式 |
|---|---|---|---|---|---|
| C1 | 实时聊天翻译（日志 → 译文） | `monitor.py:140` `ChatMonitor` → `translator.py:250` `Translator` | `ingest/chatlog` → `engine` | 2 | 实时接入真实日志，译文按序出现 |
| C2 | 反向翻译发送（中文 → 目标语言 → 游戏） | `translator.py:906` `translate_for_send`、`overlay.py:887` `_do_translate` | `task/sendpipeline` → `native/input` | 2 | 输入中文 → 游戏聊天出现译文 |
| C3 | 系统消息翻译 | `monitor.py:42` `SYSTEM_LINE_RE`、`is_system` 贯穿 | `ingest/chatlog` + `domain.Event` | 1 | TMP 系统通知被译为中文 |
| C4 | 全员翻译 + 跳过自己 | `monitor.py:97` `is_self`、`translator.py:329` `self_skipped` | `engine` | 1 | 自己的消息不翻译、不计数为已译 |
| C5 | 游戏昵称识别（自动 + 手动） | `main.py:105` `_check_self_name`、`_self_name_ref` | `ingest/chatlog` + `config` | 1 | 首次自动识别并持久化到配置 |
| C6 | 服务器名自动识别 | `monitor.py:49` `SERVER_CONNECT_RE`、`overlay.py:564` `_poll_server_name` | `ingest` → `sink` 更新 | 2 | 标题栏显示 `Connecting to X server...` 中的 X |
| C7 | 聊天去重（玩家+内容+时间戳） | `monitor.py` 去重逻辑 | `domain.Event.ID` | 1 | 重启/追加写入不产生重复行 |

### 3.2 智能翻译引擎

| # | 能力 | V1 证据 | V2 归属 | 阶段 | 验收方式 |
|---|---|---|---|---|---|
| E1 | **多模型并行竞速**（先返回有效译文者胜） | v2.3.1 核心；`test_translation_refactor.py` 有 2 个专项用例 | `providers/race` | 2 | 全部启用 Provider 并发，未通过 `looks_untranslated` 的结果被跳过 |
| E2 | 20 组 Provider 预设 | `provider_presets.py:94` `PRESETS`、`:79` `CATEGORIES` | `resources/providers.json` | 1 | 预设选择器列出 20 项并正确填充 endpoint/model |
| E3 | 模型列表拉取 | `model_fetcher.py:18` `fetch_models` | `providers` | 2 | `GET /api/providers/{id}/models` 返回模型列表 |
| E4 | Provider 熔断回退 | `translator.py:510` `_note_provider_result`（3 次失败 → `min(30*2^(n-3),120)` 秒） | `providers/breaker` | 2 | 连续失败进入冷却，成功即恢复 |
| E5 | 批量翻译（0.3s 窗口 / 满 8 条） | `translator.py:215` `BATCH_WINDOW=0.3` | `engine/batch` | 2 | 多条消息合并为一次请求 |
| E6 | **批量分隔符泄漏回退** | `translator.py:449-462` 拆分失败则逐条重译 | `engine/batch` | 2 | LLM 未回显分隔符时，不会把整段译文挂到第一条 |
| E7 | LRU 缓存 1000 条 | `translator.py:214` `CACHE_SIZE=1000`、`:228` `LRUCache` | `cache/lru` | 1 | 重复消息零 API 调用 |
| E8 | 混合语言智能拆分 | `translator.py:143` `split_mixed_text`、`:196` `reassemble_mixed` | `engine/mixedlang` | 1 | `你好 where are you` → `你好 你在哪里` |
| E9 | 同文本并发请求合并 | `translator.py` `_in_flight` / `_in_flight_results` | `cache`（singleflight） | 1 | 相同原文并发只发一次请求 |
| E10 | DPAPI 加密存储 API Key | `config.py:46/64` DPAPI、`:82` `_SECRET_FIELDS` | `config`（`go:build windows`） | 1 | 配置文件中密钥为 `dpapi:` 密文 |
| E11 | 发送目标语言（10 种） | `config.py:195` `send_target_language` | `config` + `engine` | 1 | 切换目标语言后发送译文随之变化 |
| E12 | 非文字内容跳过翻译 | `chat_dictionary.py:394` `is_non_translatable`、`translator.py:719` `_should_skip` | `dictionary` | 1 | 纯标点/数字/emoji 直接原样显示 |
| E13 | **五层术语处理** | `chat_dictionary.py:11/74/107/114/127/150` | `dictionary` | 1 | 俚语本地命中零 API；`rec ban` 结构化拼接正确 |
| ~~E14~~ | ~~多 Provider 轮转分流~~ | `translator.py:268` 的 `_rr_index` **只在 `__init__` 被设为 0，全项目无任何读取** | ❌ **不存在**：V1 只有并行竞速，没有轮转 | — | **不实现**（实现它等于新增功能，违反 P1）。见 D12 |
| E15 | Provider 权重排序 | `ProviderConfig.weight` | `providers` | 2 | 高 `weight` 优先 |
| E16 | API 格式双支持（OpenAI / Anthropic） | `ProviderConfig.api_format`、`provider_presets.py:28` `build_endpoint` | `providers` | 2 | ✅ 已实现，但**V1 的 Anthropic 分支是坏的**（见 D13）：V2 用顶层 `system` 字段 + `content[0].text` 解析修正。20 个预设中 19 个用 openai、1 个（anthropic）用 anthropic 格式 |
| E17 | 一键测试全部 Provider | `main.py:1379` `_test_all_providers`、`translator.py:755` `test_connection` | `providers` + API | 2 | 逐个返回连通状态与延迟 |
| E18 | 语言检测（14 种） | `translator.py:77` `detect_language`（`test_language_detect.py` 14 例） | `engine/langdetect` | 1 | 14 个用例全通过 |
| E19 | Provider 管理（增 / 删 / 改 / 上下移动 / 启用开关） | `main.py:1336/1347/1354/1359/1366`（`_add_provider_from_preset` / `_add_provider` / `_remove_provider` / `_move_provider` / `_toggle_provider`） | `frontend/settings` + API | 3 | 五项操作均生效并持久化到配置（v1.3 补充遗漏） |

### 3.3 窗口与交互

| # | 能力 | V1 证据 | V2 归属 | 阶段 | 验收方式 |
|---|---|---|---|---|---|
| W1 | 毛玻璃（Win11 Mica / Win10 Acrylic + 回退） | `acrylic_helper.py:40` `_get_build`、`:200` `apply_acrylic`、`:248` `remove_acrylic` | **`native/window`** | 1 | Win11 与 Win10 各自生效，失败有回退 |
| W2 | 四段式纵向布局（accent 线 / 标题栏 / 消息区 / 输入栏 / 统计栏） | `overlay.py:112` `_setup_ui`、`:181`/`:221`/`:270`/`:304` | `frontend/pages/Overlay` | 3 | 视觉结构一致 |
| W3 | 配色体系 | ⚠️ 见第 7 节 D1（三套并存） | `frontend/theme` | 3 | **统一为单一 token 集** |
| W4 | 系统级全局热键 | `hotkey_manager.py:28` `HotkeyManager` | **`native/hotkey`** | 1 | 游戏内不被拦截 |
| W5 | 热键自定义（点击捕获组合键） | `main.py:267` `HotkeyCapture` | `frontend/settings/HotkeyCapture` → `hotkey.set` | 3 | 捕获并保存，含 CapsLock 提示 |
| W6 | 5 个可配置热键 | `config.py:208-212` `chat/copy/paste/enter/send` | `config` + `native/hotkey` | 1 | 全部生效 |
| W7 | 系统托盘 + 菜单（显示/隐藏、切模式、穿透、设置、退出） | `tray_icon.py:112` `TrayIcon`、`main.py:164` `_setup_tray` | **`native/tray`**（显示）+ Go（决策，`event.trayCommand`） | 1 | ✅ 图标已挂上、菜单 7 项与 V1 逐字一致。⚠️ 点击后的实际动作需人工确认 |
| W8 | 输入框常驻 + 回车发送 | `overlay.py:304` `_build_input_area`、`:870` `_on_send_enter` | `frontend/components/InputBar` | 3 | 输入回车触发翻译 |
| W9 | 译文金色高亮 | `overlay.py:44` `TRANSL = "#FFD700"` | `frontend/theme` | 3 | 视觉一致 |
| W10 | 消息时间戳右对齐 | `overlay.py:686` `_pad_line`、`FG_TIMESTAMP` | `MessageItem` | 3 | 视觉一致 |
| W11 | 语言标签开关 | `config.py:214` `show_language_label` | `config` + `MessageItem` | 1/3 | 开关生效 |
| W12 | 原文显示开关 | `config.py:215` `show_original_text` | `config` + `MessageItem` | 1/3 | 开关生效 |
| W13 | 消息分割线 + 最大条数 | `config.py:198` `max_messages=50` | `Overlay` | 3 | 超限滚动，旧消息滚出 |
| W14 | 窗口透明度 | `config.py:196` `window_opacity=0.80` | **`native/window`** + `Overlay` | 1/3 | 0.10–1.00 生效 |
| W15 | 字体大小 | `config.py:197` `font_size=12` | `Overlay` | 3 | 即时生效 |
| W16 | 显示模式 | `config.py:200` `window_mode`（2 值） | 保持 **2 值**（ADR-011 v1.2） | 3 | `standalone` / `overlay` 均可用 |
| W17 | 鼠标穿透 | `config.py:201` `click_through`、`overlay.py:440` `_set_click_through` | **`native/window`** | 1 | 穿透后点击落到游戏。✅ 出口已具备：托盘菜单的「Click-Through 鼠标穿透」（W7，带勾选状态）可随时开关，与 V1 一致 |
| W18 | 拖拽移动 + 边缘缩放 | `overlay.py:474-538` | **`native/window`** + `frontend`（区域判定） | 1/3 | ✅ **已验证**：真实合成鼠标输入下拖动 (141,81)、缩放 (120,90)、右键菜单均通过（`tools/test-window-interaction.ps1`）。几何语义逐条对齐 V1（`BORDER=8`、`MIN=280×250`、增量+边算边钳制） |
| W19 | 窗口位置与大小记忆 | `config.py:202-205` `win_x/y/w/h`、`overlay.py:401` `_save_position` | `config` + `native/window` | 1 | ✅ 已实现（D18 已修）：拖动/缩放结束后 native 上报新几何，Go 去抖 1000ms 落盘 |
| W20 | 北京时间显示 | `overlay.py:539` `_update_header_clock` | `Header` | 3 | 每秒刷新 |
| W21 | 状态提示（启动诊断 / 翻译条数 / 发送状态） | `overlay.py:726` `_show_notice`、`main.py:119` `_startup_status` | `notice` 事件 | 2/3 | 提示可自动消失 |
| **W22** | **输入栏中文输入（IME）** | V1 由 tkinter 原生支持，无专门代码 | `frontend/components/InputBar` | 3 | 中文输入法可正常选词上屏（显示层统一为 WebView 后，该风险已归零） |
| W23 | 悬浮窗置于游戏画面之上 | `overlay.py`（`window_mode="overlay"`）+ `acrylic_helper.py:200` | `native/window`（透明置顶）+ `frontend/pages/Overlay` | 3 | 窗口化/无边框全屏下浮于游戏之上；外观与 V1 一致 |
| W24 | ~~独占全屏限制提示~~ | ❌ **取消（v1.3）**：属新增功能，违反 P1 | 改为用户文档 / FAQ 说明 | — | — |
| **W25** | **消息区右键菜单（Settings / Exit）** | `overlay.py:344` `_build_context_menu`（**仅 2 项**） | `frontend/pages/Overlay` | 3 | 右键弹出菜单，两项功能正确（v1.2 遗漏，v1.3 补） |
| **W26** | **设置窗口（独立窗口）** | `settings_ui.py:822` 行 `SettingsWindow`（tk `Toplevel`，`main.py:358` 起）；尺寸取自 `config.py` 的 `settings_win_w/h` | **`native/window`**（带边框窗口）+ `native/webview`（第二个 WebView2 Controller）+ `frontend/pages/Settings` | 3 | 右键 → Settings 能开出窗口；窗口可缩放、可关闭、可再次打开。⚠️ 代码已实现，但**端到端验证被环境阻塞**（见实现状态 §3.3） |

### 3.4 日志与诊断

| # | 能力 | V1 证据 | V2 归属 | 阶段 | 验收方式 |
|---|---|---|---|---|---|
| L1 | 统一日志（`时间 - 厂商-模型名 - 原文 - 译文`） | `logger.py` `translation_log` | `logger` | 1 | 格式一致 |
| L2 | 日志自动轮转（7 天 / 2MB） | `logger.py:12/13` | `logger` | 1 | 超限自动切分并清理 |
| L3 | 内存缓冲 500 行供 UI 展示 | `logger.py:14` | `logger` → WS 推送 | 1 | 前端日志页实时刷新 |
| L4 | 日志页：打开目录 / 删除 / 刷新 | `main.py:823` `_build_logs_tab`、`:1178/1195/1203` | `frontend/settings/LogsTab` + API | 3 | 三个操作均生效（V1 曾修复删除失效） |
| L5 | 日志目录诊断文案 | `monitor.py:126` `log_dir_status` | `ingest` → `source.status` | 1 | 目录不存在/无文件/最新文件信息三种文案 |
| L6 | 调试日志开关（`%TEMP%`） | `config.py:213` `debug_log` | `config` | 1 | 开关生效 |
| L7 | 启动自检与状态上报 | `main.py:128` `_startup_check` | `cmd/translator` + `native.status` | 1 | 启动后前端可见状态 |
| L8 | 定期日志清理（每 6 小时） | `main.py:97` `_schedule_log_cleanup` | `task` | 2 | 定时任务与 UI 无关 |
| L9 | native 存活状态（**内部机制**） | V1 无此概念（ADR-012 引入） | `native.status` 事件 → **仅写日志，不在 UI 新增展示** | 1 | native 崩溃重启后日志有记录；用户可见行为与 V1 一致 |

### 3.5 其他

| # | 能力 | V1 证据 | V2 归属 | 阶段 | 验收方式 |
|---|---|---|---|---|---|
| O1 | 配置热重载（3 秒） | `translator.py:289-303` | `config` + `config.reloaded` 广播 | 1 | 手改配置 3 秒内生效 |
| O2 | 配置原子写 + 损坏备份 | `config.py:369` `_atomic_save`、`:310-320` | `config` | 1 | 写入中断不产生半截文件 |
| O3 | 主目录不可写时回退 `%LOCALAPPDATA%` | `config.py:286-290`、`:393` `_fallback_save` | `config` | 1 | 回退路径可读写 |
| O4 | 单实例互斥体 | `main.py:29/32` | **`native/window`** | 1 | 二次启动提示已在运行 |
| O5 | **广告定时发送** | `main.py:861` `_build_ad_tab`、`:1014-1131` `_ad_*` | `task/adsender` | 2 | **关闭设置窗口后仍继续发送**（V1 缺陷已修，见 D2） |
| O6 | 发送链路日志回读确认 | `compose_sender.py:42` `ComposeSender` | `task/sendpipeline`（Go 编排 + `native/input` 执行） | 2 | 确认送达 → `OK_CONFIRMED`；超时 → `OK_UNCONFIRMED` |
| O7 | 发送并发互斥 | `compose_sender.py` `_busy_lock` | `sendpipeline` | 2 | 并发发送返回 `BUSY` |
| O8 | 剪贴板保存与恢复 | `input_sender.py:183/239` | **`native/input`** | 2 | 发送后剪贴板恢复原内容 |
| O9 | 剪贴板复制译文 | `config.py:209` `copy_hotkey` | **`native/hotkey` + `clipboard.cache`**（本地完成，零 IPC） | 1 | 热键复制成功 |
| O10 | 程序图标 / 打包 | `icon.ico`、`build_exe.py`（版本号从 `config.py` 读取） | 构建脚本 | 3 | V2 产物体积与 V1 基线对比 |
| O11 | 5 态发送结果枚举 | `compose_sender.py:21` `SendResult` | `domain` | 1 | 5 个状态语义保留 |

---

## 4. 表 3：配置项迁移（29 字段 + 5 热键）

| 分组 | 字段 | 默认值 | V2 归属 | 备注 |
|---|---|---|---|---|
| 网络 | `api_endpoint` / `api_key` / `api_model` | `""` | `config` | 仅为 V1 兼容保留；实际以 `llm_providers` 为准 |
| 语言 | `target_language` | `zh-CN` | `config` | |
| 语言 | `send_target_language` | `en` | `config` | 10 种可选 |
| 外观 | `window_opacity` | `0.80` | `config` → `native/window` | 0.10–1.00 |
| 外观 | `font_size` | `12` | `config` | |
| 外观 | `max_messages` | `50` | `config` | |
| 外观 | `accent_color` | `#3b82f6` | `config` | ⚠️ **V1 中该字段无效**，见 D1 |
| 外观 | `show_language_label` | `true` | `config` | |
| 外观 | `show_original_text` | `true` | `config` | |
| 窗口 | `window_mode` | `standalone` | `config` → `native/window` | 保持 V1 的 **2 值**：`standalone` / `overlay`（ADR-011 v1.2） |
| 窗口 | `click_through` | `false` | `config` → `native/window` | |
| 窗口 | `win_x` / `win_y` | `-1`（自动居中） | `config` | |
| 窗口 | `win_w` / `win_h` | `620` / `360` | `config` | |
| 窗口 | `settings_win_w` / `settings_win_h` | `540` / `700` | `config` | 前端可改为自适应 |
| 身份 | `player_name` | `""` | `config` | 自动检测后回写 |
| 热键 | `chat_hotkey` | `y` | `config` → `native/hotkey` | |
| 热键 | `copy_hotkey` | `ctrl+c` | `config` → `native/hotkey` | |
| 热键 | `paste_hotkey` | `ctrl+v` | `config` → `native/hotkey` | |
| 热键 | `enter_hotkey` | `enter` | `config` → `native/hotkey` | |
| 热键 | `send_hotkey` | `shift+y` | `config` → `native/hotkey` | 全局热键 |
| 诊断 | `debug_log` | `false` | `config` | 写 `%TEMP%` |
| 广告 | `ad_messages` | `[""] * 5` | `config` | 5 条槽位 |
| 广告 | `ad_countdown` | `""` | `config` | 分钟 |
| Provider | `llm_providers` | `[]` | `config` | `ProviderConfig` 13 字段数组 |
| 版本 | `version` | `2` | `config` | V1 的 v1→v2 迁移依据（`config.py:222`） |

`ProviderConfig` 13 字段：`id` / `label` / `endpoint` / `api_key` / `model` / `enabled` / `preset_id` / `icon` / `api_format` / `weight` / `extra_headers` / `extra_body` / `timeout`（提示：`timeout` 默认 8 秒）。**全部保留**，否则用户配置无法无损迁移。

---

## 5. 表 4：测试用例迁移（99 例）

| V1 测试文件 | 用例数 | 覆盖对象 | V2 目标位置 | 优先级 |
|---|---|---|---|---|
| `test_chat_dictionary.py` | 13 | 词典命中/未命中、短语、结构化、ETS2 词、后处理、@mention 保留、未翻译校验、源语言检测、非文字 | `backend/internal/dictionary` | 高（术语质量） |
| `test_compose_sender.py` | 22 | `_normalize`、`_is_mostly_chinese`、`validate`、`wait_confirmation`、`SendResult` 5 态 | `backend/internal/task` | 高（发送可靠性） |
| `test_language_detect.py` | 14 | 14 种语言检测 | `backend/internal/engine` | 高 |
| `test_mixed_lang.py` | 21 | `detect_language`、`split_mixed_text`、`reassemble` | `backend/internal/engine` | 高（易回归） |
| `test_translation_refactor.py` | 8 | `max_output_tokens` clamp、system+user 提示结构、DeepSeek thinking 关闭、**竞速跳过未翻译结果**、**竞速并发调用全部 Provider**、批量拆分失败回退 | `backend/internal/engine` + `providers` | **最高**（v2.3.1 核心能力） |
| `test_logger.py` | 4 | 基础写入、缓冲上限、线程安全、日志目录 | `backend/internal/logger` | 中 |
| `test_log_deletion.py` | 7 | 日志删除、`translation_log` | `backend/internal/logger` | 中 |
| `test_phase1_config_fixes.py` | 5 | DPAPI 解密失败保留原值、损坏配置备份、原子写、回退路径、回退保存 | `backend/internal/config` | 高（数据安全） |
| `test_provider_config.py` | 5 | `ProviderConfig` 默认值、`llm_providers` 字段、旧字段迁移、密钥加密、多 Provider 存取 | `backend/internal/config` | 高 |
| | **99** | | | |

> **进展记录（阶段 1 / 阶段 2 进行中）**
>
> | 项 | 状态 |
> |---|---|
> | `internal/domain` | ✅ Event / RenderedMessage / Stats / 10 个错误码，文案与 V1 逐字一致 |
> | `internal/dictionary` | ✅ 迁移完成，**管线级保真度对拍 176 条逐条一致** |
> | `internal/engine` | ✅ 语言检测（14 种）+ 混合拆分 + 已是目标语言跳过 + 出口后处理 |
> | `internal/ingest/chatlog` | ✅ 日志 tail（双 glob / 切档 / 去重 / 半行缓冲 / 跳过历史）+ 注册表取文档目录 |
> | `internal/hotkeys` | ✅ 统一解析器 + `resources/keymap.json`（修 D7） |
> | `internal/cache` | ✅ LRU 1000 + 同文本并发合并（E7 / E9） |
> | `internal/config` | ✅ DPAPI + 原子写 + 损坏备份 + 热重载 + 目录回退 |
> | `internal/providers` | ✅ 并行竞速 + 熔断 + 连通测试 + 模型拉取（修 D13） |
> | `internal/logger` | ✅ 按天 + 按大小轮转、内存缓冲 500、翻译日志格式、删除（保留策略=7 天，不是 7 个文件） |
> | `internal/api` | ✅ REST（health/config/providers/presets/logs/stats/translate）+ **SSE 事件推送**（ADR-016） |
> | `internal/ipc` | ✅ Named Pipe 客户端 + 帧编解码（含超大帧拒绝、截断帧处理） |
> | **native `json`** | ✅ 极简 JSON DOM（**C++ 自测 75 项断言全过**，抓到过前导零 bug） |
> | **native `pipe`** | ✅ Named Pipe 服务端 + `native.hello`/`ping`/`shutdown` 分发 |
> | 跨语言验证 | ⚠️ Go↔C++ 的 Named Pipe 端到端测试**已写好**，但在受限沙箱里会 SKIP（沙箱禁止打开命名管道）——**需在普通终端运行以取得权威结果** |
> | 服务模式 | ✅ `-serve` 实测：`/api/translate` 返回真实词典译文、20 个预设来自资源文件、Provider 列表不泄露密钥 |
> | 端到端 | ✅ **Go 版能真翻译**：mock LLM 验证「本地可处理的消息绝不打网络」 |
> | 测试 | ✅ **Go 119 + C++ 75 项断言全绿**（10 个 Go 包） |
>
> 词库测试：V1 的 13 个用例全部通过 + 新增用例。
> **保真度对拍**：`tools/fidelity/dict_fidelity.py`（管线级 + 词库级，176 条真实消息逐条一致）。
>
> 对拍中发现并**刻意保留**的 V1 行为边界（不可"顺手修好"，否则偏离 P1）：
> 重复字母压缩只作用于**短语表**（`thank youuu`→谢谢），**不作用于俚语表**
> （`sryyy` 两边都未命中，而 `srry`/`srrry` 是俚语直接键，命中）。
> 另一条：全部 Provider 失败时用户看到的是**通用文案**「[翻译失败] 所有 Provider 翻译失败」，
> 具体错误码（认证失败/超时…）只进日志——这也是 V1 的真实行为。

### 5.1 V2 必须新增的测试（V1 无对应用例）

| 目标位置 | 覆盖内容 | 理由 |
|---|---|---|
| `backend/internal/ingest` | 日志解析、双 glob、切档、去重、系统消息、服务器名 | **唯一数据源，V1 零测试覆盖**（见 D5） |
| `backend/internal/capability` | 期望状态重放、native 重启后恢复、差异告警 | ADR-012 的核心机制 |
| `native/tests` | 帧编解码往返、粘包/半包、心跳超时、热键注册幂等、WebView2 宿主启动冒烟 | IPC 是新增跨进程边界；C++ 侧**无 `-race` 等价工具**，测试更关键；宿主代码需可独立跑通（ADR-005 组织约束） |
| `frontend/tests` | 两种显示模式切换（`standalone` / `overlay`）、输入栏 IME 路径、REST/WS 客户端错误处理 | 前端为全新实现（功能与 V1 对齐，不新增功能） |

---

## 6. 表 5：阶段 0 基线记录表

> **实测结果见 [`../tools/baseline/RESULTS-v1.md`](../tools/baseline/RESULTS-v1.md)。**
>
> 进度：**3 项已测**（B1 / B7 / B10）+ **4 项离线子指标已测**（B5' / B6' / B8'）。
> 其余需真实 Provider 配置、Win10 设备或人工参与。
> **在 B2 / B3 / B5 / B6 / B9 补齐前，不得开始阶段 1 的编码**（ADR-010）。
>
> 已测值得出的一个重要结论：**V1 真实管线里 42.0% 的消息不调用 API**（纯中文 22.7% +
> 词典 11.4% + 非文字 8.0%），且本地处理仅占端到端延迟的 0.0083%（LLM 往返按 500 ms 计）。
> 因此 V2 迁移时**必须逐字保留混合拆分与五层词典**，否则这 42% 会开始走 API，用户立刻感到变慢。
> ⚠️ 早先记录过「82.4%」是**探针测量错误**（把混合消息的外文片段也当成了跳过），
> 已勘误，见 `tools/baseline/RESULTS-v1.md`。基线必须按真实管线测。

| # | 指标 | 测量方法 | V1 实测 | V2 目标 | 状态 |
|---|---|---|---|---|---|
| B1 | 产物大小 | `dist/*.exe` 字节数 | **25.02 MB** | ≤ V1 + 5 MB（新增 native 层） | ✅ 已测 |
| B2 | 冷启动 → 窗口可见 | 秒表 / 日志时间戳差值 | | 不劣化 >20% | ⬜ 待测 |
| B3 | 空闲 RSS | `Get-Process` Working Set | | 不劣化 >20% | ⬜ 待测 |
| B4 | 负载 RSS（100 条/分钟） | 回放日志 + 采样峰值 | | 不劣化 >20% | ⬜ 待测 |
| B5 | 首条翻译延迟（日志写入 → 窗口显示） | 时间戳差值，p50/p95 | | 不劣化 | ⬜ 待测 |
| B6 | Provider 竞速延迟 p50/p95 | 引擎内部计时 | | 不劣化 | ⬜ 待测 |
| B7 | **真实管线零 API 调用率** | `tools/fidelity/dict_fidelity.py`（管线级对拍） | **42.0%**（176 条真实消息：纯中文 40 + 词典 20 + 非文字 14） | 不低于 V1 | ✅ 已测 |
| B8 | 缓存命中率 / 节省率 | `TranslationStats` 统计栏数值 | | 不低于 V1 | ⬜ 待测 |
| B9 | 闲置 CPU 占用 | 任务管理器采样 | | 不劣化 | ⬜ 待测 |
| B10 | 单元测试通过数 | `python -m pytest -q`（**须在干净环境**，受限沙箱会产生假失败） | **99 / 99（2.01s）** | ≥ 99（V2 侧） | ✅ 已测 |
| B11 | Win10 毛玻璃回退行为 | 在 Win10 上运行 | | 行为一致 | ⬜ 待测 |
| B12 | 设置窗口打开耗时 | 秒表 | | 不劣化 | ⬜ 待测 |
| **B13** | **热键响应延迟（按下 → 输入栏出现）** | 高帧率录屏逐帧比对 | **（V1 同进程，作为零 IPC 基准）** | 增量 ≤ 10 ms | ⬜ 待测 |
| **B14** | **悬浮窗对游戏帧率的影响** | 游戏内 FPS 采样（悬浮窗开启/关闭对照） | 不适用（V1 未测） | 掉帧 ≤ 5% | ⬜ 待测 |

> **B3/B4 是 ADR-003 / ADR-013 的复核依据。** 若 WebView2 方案内存劣化明显，需重新评估前端选型。
> **B13 是 ADR-005「IPC 开销」保留意见的验证点**：若实测增量超过 10 ms，说明热键下沉 C++ 的代价高于预估。

---

## 7. 表 6：迁移中发现的 V1 既有缺陷（V2 必须修，不得原样搬运）

| ID | 缺陷 | 证据 | 影响 | V2 处置 |
|---|---|---|---|---|
| **D1** | **三套品牌色并存，且 `accent_color` 配置项无效** | ① `overlay.py:32-34` 悬浮窗用 `#4494FC`/`#60A8FF`/`#70B8FF`（与 README 一致）② `main.py:339` 与 `settings_ui.py:37` 设置窗口用 `#007acc`（VS Code 蓝）③ `config.py:216` 默认 `accent_color="#3b82f6"`，且 `main.py:1448` 会保存它，但 `overlay.py` **从不读取**该字段 | 用户改 `accent_color` 不生效（死配置）；窗口与设置页观感不一致 | 统一为单一 token 集（以 `#4494FC`/`#60A8FF`/`#70B8FF` 为准）；`accent_color` 要么真正生效、要么删除，不留死字段 |
| **D2** | **广告发送器生命周期绑定设置窗口** | `main.py:861` `_build_ad_tab` 内的 `_ad_schedule_tick` 靠设置窗口的 tk `after()` 驱动；窗口关闭即停 | 定时广告无法在关窗后继续，与「定时发送」语义矛盾 | 迁移为 `task/adsender`，与 UI 生命周期解耦（ADR-007） |
| **D3** | **设置 UI 双实现并存** | `main.py:327` 标注 `# DEPRECATED: SettingsDialog is replaced by settings_ui.SettingsWindow`，但 `App._open_settings`（`main.py:250`）**仍调用旧版**；`settings_ui.SettingsWindow` 仅含 Provider 管理，缺热键/外观/日志/广告 Tab | 约 2000 行重复 UI；改一个功能要动两处；旧版承载广告发送器等独有逻辑 | V2 统一为单一 `frontend/settings`，删除双实现 |
| **D4** | 业务逻辑寄生在 UI 内 | `overlay.py`（UI 模块）内实现 `_do_translate` / `_do_auto_send` / `_on_send_done` 完整发送编排 | UI 无法独立替换（正是 G2 要解决的问题） | 迁移至 `task/sendpipeline` + API |
| **D5** | `ingest` 层无任何单元测试 | 9 个测试文件均不覆盖 `monitor.py` 的解析逻辑 | 唯一数据源的解析正确性无回归保护 | V2 补 `ingest` 测试（含双 glob、切档、去重、系统消息、服务器名） |
| **D6** | 窗口操作与业务逻辑处于同一进程，无隔离 | V1 全部能力在一个进程内，任意一处异常都可能拖垮全局 | 无法单独重启显示层 | V2 借 native 进程隔离显式处理降级（ADR-012），**这是 V1 没有的能力，非缺陷修复** |
| **D7** | **热键字符串解析有 4 处实现（2 活 2 死），行为不一致** | **活**：① `win32_constants.py:195` `vk_code`（支持 `enter`/`f1`~`f12`，输入模拟在用）② `overlay.py:761` `_parse_hotkey`（**不支持特殊键**，全局热键轮询在用）<br>**死**：③ `hotkey_manager.py:63` `_parse_hotkey` ④ `hotkey_manager.py:81` `_parse_hotkey_vk`（两者都随 D10 一起失效）<br>另：修饰键表存在**两份**（`win32_constants.py:108` 与 `input_sender.py:132`） | **静默失效**：配置界面 `HotkeyCapture` 允许按 F1 等特殊键，但真正生效的 `overlay._parse_hotkey` 只认单字符 → `vk=0` 直接 return，**热键配了不生效且不报错** | ✅ 已修复：V2 统一为 `internal/hotkeys` + `resources/keymap.json`（10 个测试覆盖，含全部修饰键别名）。特殊键与 `win` 修饰键现在真正生效——**属有意修复，验收时需知晓这是与 V1 的行为差异** |
| **D10** | **`hotkey_manager.py` 是死代码（203 行）** | 全项目搜 `HotkeyManager` 只有两处命中：它自己的类定义、`build_exe.py:43` 一行残留的 `--hidden-import`。**无任何 import 或实例化**；`main.py` 完全没引用它 | 发布说明声称「v1.3.0 热键升级为 `RegisterHotKey` 系统级热键」，但**这套代码从未被接线**——真正的全局热键一直是 `overlay.py` 的轮询实现 | 不迁移。**但由此产生一个待决问题：V2 该用 `RegisterHotKey` 还是沿用轮询？** 见 ADR-015 |
| **D11** | **README 与代码不符：热键实现方式** | `README.md:106` 写「⌨️ **系统级全局热键** \| `RegisterHotKey` 系统热键，不会被游戏拦截」；`release_notes_v1.5.2/v2.1.0` 亦如此宣称。实际生效的是 `overlay.py:773` `_start_hotkey_poller`——`GetAsyncKeyState` **每 50 毫秒轮询** | 用户以为在用系统热键，实际是轮询。轮询也是 B9「闲置 CPU 2.88%」的部分来源（每秒 20 次唤醒） | V2 按 ADR-015 统一；无论选哪种，README 都必须与实现一致 |
| **D12** | **`_rr_index` 残留变量 + README 的「轮转负载均衡」承诺已失效** | `translator.py:268` `self._rr_index = 0  # round-robin dispatch index` —— **全项目搜索只有这一处赋值，没有任何读取**。而 `README.md` 的 v2.1.0 条目写着「🔄 多模型轮转负载均衡 — 多个 Provider 按轮转顺序分配翻译任务」；v2.3.1 改用并行竞速后，轮转逻辑被移除，变量与文档却都留着 | 文档让用户以为有负载均衡，实际只有竞速。若 V2 照文档实现轮转，就是**新增了 V1 没有的功能**（违反 P1） | ✅ 已处置：`internal/providers` **只实现竞速**，不实现轮转；E14 标记为「不存在」；README 由 V2 重写 |
| **D13** | **`api_format: "anthropic"` 在 V1 里完全不可用** | `translator.py:534~599` `_call_provider` 对**两种格式**都构造 OpenAI 请求体（system 作为 messages 里的一条），并用 `data["choices"][0]["message"]["content"]` 解析响应。但 Anthropic 的 `/v1/messages`：① 不接受 messages 里的 `system` 角色（要求顶层 `system` 字段）→ 400；② 响应结构是 `content[0].text` → 即使请求成功也会 KeyError → `[响应格式错误]` | 预设列表里提供了 `anthropic`（`claude-sonnet-4-20250514`），用户选中后**必然失败**。20 个预设里 19 个不受影响 | ✅ 已修复：V2 按格式分流——anthropic 用顶层 `system` + 单条 user 消息，并解析 `content[0].text`。测试 `TestAnthropicFormatFixed` 覆盖。**属有意修复，验收时需知晓** |
| **D8** | **`message_display.py` 是死代码（244 行）** | 全项目搜 `MessageDisplay` 只有两处命中：它自己的类定义、`build_exe.py:42` 一行残留的 `--hidden-import`。**无任何 import 或实例化**；`overlay.py` 自己内联实现了一套 | 照搬会去实现 `message_display.py:95` 那个**程序根本不显示**的 5 项右键菜单（实际显示的是 `overlay.py:344` 的 2 项菜单） | 不迁移；清理 `build_exe.py` 残留导入 |
| **D9** | 消息区配色标签有两套实现（其中一套已死） | `overlay.py:249` `_setup_text_tags` 与 `message_display.py:57` `_setup_color_tags`，且两套的颜色值不同（前者 `PLAYER`/`TRANSL`，后者硬编码 `#569cd6`/`#FFD700`） | 与 D1 同源：配色无单一事实源，同一程序里存在多种"蓝"和多种"金" | V2 统一为 `frontend/theme` 单一 token 集 |

### V2 期间新记录的缺陷（D14 起）

| 编号 | 标题 | V1 的实际行为 | 影响 | V2 处置 |
|---|---|---|---|---|
| **D14** | **翻译结果缓存会把错误文案一起缓存** | `translator.py:428` / `465` 在翻译完成后**无条件** `self._cache.put(text, translated)`，而 `_call_api` 失败时返回的是 `"[网络错误] …"` 这类文案 | 网络恢复后，同一条消息在缓存淘汰前**一直显示旧错误**，用户以为程序坏了 | ✅ V2 明确**不缓存** `state == "error"` 的结果。属有意修复，验收时需知晓 |
| **D15** | **`show_original_text` 有配置项、悬浮窗也在用，但设置界面没有开关** | `overlay.py:646/673` 读 `self.cfg.show_original_text`；`config.py` 有该字段；但 `main.py:_build_appearance_tab`（728-822）只放了透明度/字号/消息数/昵称/窗口模式/穿透/语言标签，**没有它** | 用户无法通过界面开关「原文行」，只能手改 JSON | ✅ V2 在外观页补上该开关 |
| **D16** | **同一个「打开游戏聊天」动作，手动发送与广告发送用了不同的热键来源** | 手动发送用配置项：`compose_sender.py:96` `send_chat_message(english, self.cfg.chat_hotkey)`；广告发送硬编码：`main.py:1076` `send_chat_message(text, "y")` | 用户改了「呼出热键」后，手动发送跟着变、**广告发送仍用 `y` 而失效**，且没有任何提示 | ✅ V2 统一走 `cfg.ChatHotkey`（`task.AdSender` 的 `Options.Hotkey`） |
| **D17** | **广告发送的状态机寄生在设置窗口里** | `main.py:_ad_schedule_tick` 靠设置窗口的 tk `after()` 驱动，所以关掉设置窗口发送就停；V1 自己也知道这点，在界面上写了红字「⚠ 使用广告发送请不要关闭设置界面」 | 「定时循环发送」与「必须一直开着设置窗口」自相矛盾；用户被迫开着窗口 | ✅ V2 把状态机搬到 Go 侧（`internal/task`）：界面只是控制面板，关掉窗口发送照常继续。**这是消除 D2 根因的必要一步**，与 D2 同源 |
| **D18** | **V2 尚无窗口位置记忆的落盘链路** | V1 在 `overlay.py:415` `_schedule_save_position()` 里去抖 1000ms 后写回 `win_x/y/w/h`，拖动与缩放结束都会触发（`:528`/`:533`） | 拖动或缩放之后位置不落盘：Go 的期望状态不知道窗口已被挪走，重连重放（ADR-012）会把窗口**弹回旧位置**。启用拖拽后这个不一致才变得可触发 | ✅ **已修**：native 在拖动/缩放结束时上报 `event.windowChanged`，Go 更新期望状态并去抖 1000ms 落盘。注意落盘走 `NoteDisplayBounds` 而**不是** `SetDisplay`——后者会触发 `display.create`，而 native 是「先销毁再重建」，拖完会闪一下 |
| **D20** | **V1 的「切换模式」是个死功能** | `overlay.py:427` `_apply_mode()` 的文档字符串写着 *"Apply window mode: **always borderless overlay** with acrylic or alpha"*，函数体无条件 `overrideredirect(True)` + 置顶 + `WS_EX_TOOLWINDOW`，**从不读 `cfg.window_mode`** | 托盘菜单里的「Switch Mode 切换模式」点了只改配置值，窗口形态一点不变；用户以为切换生效了（配置里也确实变了），实际什么都没发生。属 D1「`accent_color` 死配置」同类：有配置项、有界面入口、就是没有效果 | ⚠️ **保持 V1 行为**（只翻转 `window_mode` 并保存 + 记日志），**不**凭空造一个 V1 没有的独立窗口模式——那会违反 P1。若将来要做真模式，应作为**新增功能**单独决策 |
| **D19** | **`click_through` 在 V2 里是单向开关（没有应用内出口）** | V1 的托盘菜单有「Click-Through 鼠标穿透」勾选项（`main.py:176`），任何时候都能关掉；且 V1 会记一条 `鼠标穿透: 开/关` 日志 | 悬浮窗一旦开启穿透就**收不到任何鼠标消息**，而 V2 的托盘未实现、设置窗口也还没建成真正的第二个窗口——用户点不动悬浮窗、也没有托盘可点，只能手改 `config.json`。这是一次真实故障的直接成因（详见实现状态 §3.2） | ⚠️ **已缓解未根治**：设置窗口（W26）已实现，穿透状态下可另开设置页关掉它；但「悬浮窗自己点不动」时用户未必想到去开设置页。托盘（W7）仍是 V1 的原始出口，应优先补齐 |

---

## 8. 表 7：明确不迁移 / 推迟项

| 项 | 处置 | 理由 |
|---|---|---|
| 百度翻译相关代码 | ❌ 不迁移 | V1 v2.2.0 已移除，翻译引擎回归纯 LLM |
| `build/`、`.pytest_cache/`、`*.spec` 等构建产物 | ❌ 不迁移 | `.gitignore` 已忽略，属本地产物 |
| `ETS2 Translator/config.json`（仓库内 14 字节残留） | ❌ 不迁移 | 真实配置在 `文档\ETS2 Translator\`，此为误导性残留 |
| `receive_prompt.txt` / `send_prompt.txt` | ❌ 不迁移 | V1 v2.3.0 起已不再被调用（见 `docs/specs/2026-08-13` 的记录） |
| `translator.py:692` `_call_api_legacy` 等遗留路径 | ⚠️ 仅在确认无引用后删除 | 发送方向 `translate_for_send` 在无 Provider 时会回退到 legacy 路径，需先确认该分支是否仍需保留 |
| Go 侧 `internal/platform`（原计划 Go 直调 Win32） | ❌ **取消** | 按 ADR-005 全部下沉 `native/`；Go 仅保留 DPAPI 与注册表读取 |
| Wails / Tauri 依赖 | ❌ **不引入**（Q6 已决） | 前端为纯 SPA，由 native 宿主 WebView2，经 REST + WS 与 Go 通信 |
| Windows Graphics Capture / OCR 数据源 | ❌ **不实现**（Q3 已决「都不做」） | `native/capture` 取消；Source 只实现 `chatlog` |
| DirectX 渲染层 | ❌ **取消**（Q1/Q7 反转） | 不注入游戏进程的前提下它相对 WebView 置顶层无额外能力（ADR-011 / ADR-014） |
| 视频翻译 / 语音翻译 | ❌ **不实现**（Q3 已决） | 上游需求书第十节为空，决策方明确「先把现有的做好」 |
