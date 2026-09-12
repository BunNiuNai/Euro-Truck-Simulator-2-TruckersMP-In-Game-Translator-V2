# 逐文件语言归属判定

> 本文档取代 `migration-matrix.md` 表 1 中较粗的归属划分。
> 依据的判定宪法：**读每一个文件 → 判断它到底在干什么 → 再决定用哪种语言替换**，
> 不允许按"某类东西一起划给某个语言"的粗粒度方式一刀切。

| 项目 | 内容 |
|---|---|
| 版本 | v1.0 |
| 日期 | 2026-08-15 |
| 上游文档 | `architecture.md` v1.3、`migration-matrix.md` v1.3 |

---

## 1. 判定规则（先定尺子，再量文件）

一个文件里往往混着好几件事，所以判定单位是**职责**而不是文件。对每一块职责问四个问题：

| 顺序 | 问题 | 是 → |
|---|---|---|
| 1 | 它直接操作 Windows 句柄 / 消息 / 内核对象吗？ | **C++** |
| 2 | 它是业务规则、IO、网络、并发、编排吗？ | **Go** |
| 3 | 它是画界面或接收用户输入吗？ | **TypeScript** |
| 4 | 它是**用户可能想改**的内容数据吗？ | **JSON** |

补充两条反例规则（避免把不该下沉的东西塞进 C++）：

- **技术常量不进跨语言共享层。** Win32 的 `VK_RETURN=0x0D` 这类常量，C++ 用 `windows.h`、
  Go 用 `syscall`，各语言原生化即可，**不需要为了"统一"而在语言之间搬运**。
- **纯函数谁用谁实现，但必须有一致性测试。** 例如热键字符串解析 Go 和 C++ 都要用，
  与其跨进程调用，不如各自实现 + 对拍测试（V2 已有对拍工具，见 §5）。

---

## 2. 逐文件判定表

> **阅读深度**：`全` = 通读全文；`主` = 读了主要部分与所有关键函数；`构` = 读了结构（类/函数清单）+ 关键片段。
> 标注深度是为了让这份判定可被复核——深度不足的地方结论也应当更谨慎。

| # | 文件 | 行数 | 它到底在干什么 | 拆成几块 | 各块归属 | 深度 |
|---|---|---|---|---|---|---|
| 1 | `main.py` | 1496 | 进程主控：单实例互斥体、模块接线、托盘菜单接线、**SettingsDialog（约 1135 行）**、**广告定时发送器**、入口 | ① 单实例互斥体 → **C++**<br>② App 生命周期 + native 监督 → **Go**<br>③ 托盘菜单项定义与动作 → **Go**<br>④ SettingsDialog 全部 → **TS**<br>⑤ 广告发送器（`_ad_*`）→ **Go**<br>⑥ `HotkeyCapture` 控件 → **TS** | C++ / Go / TS | 主 |
| 2 | `overlay.py` | 1017 | 悬浮窗：UI 布局与渲染、窗口操作、**全局热键（活的实现）**、**发送编排** | ① 布局/渲染/消息区/输入栏 → **TS**<br>② 拖拽·边缘缩放·鼠标穿透·位置记忆 → **C++**<br>③ 毛玻璃调用 → **C++**<br>④ **全局热键检测**（`_start_hotkey_poller`，`GetAsyncKeyState` 每 50ms 轮询）→ **C++**；解析 → **Go**（`internal/hotkeys`）<br>⑤ `_do_translate`/`_do_auto_send`/`_on_send_done` → **Go** | TS / C++ / Go | 主 |
| 3 | `settings_ui.py` | 822 | 新版设置窗口：Provider 增删改、预设选择、模型拉取、连通测试 | 全部 → **TS**（无业务逻辑） | TS | 构 |
| 4 | `translator.py` | 939 | 翻译引擎：批处理、混合语言拆分、语言检测、竞速、轮转、熔断、缓存、后处理、连通测试 | 全部 → **Go**（一行 Win32 都没有，纯业务） | Go | 主 |
| 5 | `config.py` | 401 | 配置模型、DPAPI 加解密、原子写、损坏备份、回退路径、注册表取文档目录 | ① 配置模型/原子写/回退 → **Go**<br>② DPAPI → **Go**（`syscall` 已实测可用）<br>③ 注册表读文档目录 → **Go**（同上） | Go | **全** |
| 6 | `chat_dictionary.py` | 399 | 五层术语处理 | ① 词典数据（俚语/短语/结构化/系统消息/ETS2）→ **JSON**<br>② 处理函数（`Lookup`/`NonTranslatable`/`LooksUntranslated`/`Mention`/`FixShorthand`/`GuessLang`）→ **Go** | JSON + Go | **全** |
| 7 | `input_sender.py` | 341 | **混了三件事**：Win32 输入/剪贴板原语、热键字符串解析、发送时序编排 | ① `SendInput` 与剪贴板原语 → **C++**<br>② 热键字符串解析 → **Go**（并合并重复实现，见 §4）<br>③ `send_chat_message` 时序 + 剪贴板保存/恢复 + 错误文案 → **Go** | C++ + Go | **全** |
| 8 | `tray_icon.py` | 324 | 托盘图标：GDI 画图标、`Shell_NotifyIcon`、隐藏窗口消息循环、右键菜单弹出 | ① 图标生成 + `Shell_NotifyIcon` + 消息循环 → **C++**<br>② 菜单项定义与点击动作 → **Go** | C++ + Go | 主 |
| 9 | `provider_presets.py` | 304 | 20 组 LLM 供应商预设 + 图标 + 分类 | 全部 → **JSON**（已生成 `providers.json`） | JSON | 主 |
| 10 | `monitor.py` | 267 | 聊天日志 tail：正则解析、去重、切档、昵称/服务器识别 | 全部 → **Go**（纯文件 IO，无 Win32） | Go | **全** |
| 11 | `acrylic_helper.py` | 261 | 毛玻璃：OS 版本探测 + Win11 Mica / Win10 Acrylic 两条路径 | 全部 → **C++**<br>⚠️ 其中 `_get_real_hwnd`（`GetParent(root.winfo_id())`）是 **tkinter 专有 hack，不迁移**——V2 的 native 直接持有 HWND | C++ | **全** |
| 12 | `message_display.py` | 244 | ~~消息渲染引擎~~ **实为死代码** | **不迁移**（见 §4.1） | — | **全** || 13 | `logger.py` | 238 | 文件日志轮转 + 内存环形缓冲 + 翻译日志格式 | 全部 → **Go** | Go | 主 |
| 14 | `win32_constants.py` | 216 | Win32 常量表 + 结构体 + 按键名映射 + `vk_code`/`mod_vk` | ① 常量与结构体 → **各语言原生**（C++ 用 `windows.h`，Go 用 `syscall`）——**不做跨语言共享**<br>② 按键名映射表 → **JSON**<br>③ `vk_code`/`mod_vk` → **Go** | C++ / JSON / Go | **全** |
| 15 | `hotkey_manager.py` | 203 | ~~`RegisterHotKey` + 隐藏消息窗口 + 消息循环~~ **实为死代码** | **不迁移**（见 §4.4）<br>⚠️ 真正的全局热键实现在 `overlay.py`（`GetAsyncKeyState` 轮询），见第 2 行 | — | **全** |
| 16 | `compose_sender.py` | 181 | 发送链路编排：校验 → 发送 → 回读日志确认 → 剪贴板恢复 | 全部 → **Go**（编排与判定都是业务逻辑，Win32 调用经 IPC 下沉） | Go | 主 |
| 17 | `model_fetcher.py` | 175 | `/v1/models` 拉取 + 连通性测试 | 全部 → **Go**（纯 HTTP） | Go | 主 |
| 18 | `message_types.py` | 31 | `DisplayMessage` / `TranslationStats` 数据类 | 全部 → **Go**（已迁移为 `internal/domain`） | Go | **全** |

---

## 3. 汇总

### 3.1 各语言承担量（估算）

| 语言 | 行数量级 | 内容性质 |
|---|---|---|
| **Go** | **约 3100 行** | 翻译引擎、配置、日志、监控、发送编排、任务调度、菜单决策、**热键解析** |
| **TypeScript** | **约 2570 行** | 设置窗口（1135 行来自 `main.py`）+ 新版设置 822 + 悬浮窗 UI |
| **C++** | **约 980 行** | 纯 Win32：输入、剪贴板、**全局热键检测**、托盘、毛玻璃、窗口操作 |
| **JSON** | **约 650 行** | 词库数据 + 20 组预设 + 按键映射表 |
| 死代码 | **447 行** | `message_display.py`(244) + `hotkey_manager.py`(203)，均不迁移 |

> 相比 `migration-matrix.md` 早先的「纯 C++ 1345 行」，细分后 **C++ 减少到约 980 行**——
> 减少的部分是热键解析、发送时序、菜单决策这些**本来就不是 Win32 的东西**，
> 以及 `hotkey_manager.py` 整个死掉的 203 行。

### 3.2 九成文件是"一个文件拆给多个语言"

18 个文件里只有 6 个是单一归属（`settings_ui` / `translator` / `provider_presets` / `monitor` /
`compose_sender` / `model_fetcher` / `logger` / `message_types` / `config` / `acrylic_helper` —— 实际 10 个）。
其余 8 个需要拆分：

| 文件 | 拆成几块 |
|---|---|
| `main.py` | 6 |
| `overlay.py` | 5 |
| `input_sender.py` | 3 |
| `hotkey_manager.py` | 3 |
| `win32_constants.py` | 3 |
| `tray_icon.py` | 2 |
| `chat_dictionary.py` | 2 |
| `message_display.py` | 0（死代码） |

**这正是"一刀切"会出错的地方**：整文件归属会把发送时序、菜单决策、热键解析这些业务逻辑
错误地塞进 C++，让 C++ 层从「纯能力层」膨胀成「能力 + 业务混合层」——那正是 V1 的问题，
只不过换了个语言重演。

---

## 4. 本次逐文件阅读发现的缺陷

### 4.1 `message_display.py` 是死代码（244 行）

全项目搜索 `MessageDisplay`，只有两处命中：

- `message_display.py:17` —— 它自己的类定义
- `build_exe.py:42` —— 一行残留的 `--hidden-import message_display`

**没有任何文件 import 或实例化它。** `overlay.py` 自己内联实现了一套（`_build_message_area` /
`_setup_text_tags` / `_insert_one_at` / `_sync_display` / `_build_stats_bar`）。

**危害**：如果照搬，会去实现一个**程序根本不显示的 5 项右键菜单**
（`message_display.py:95` 定义了 Settings / Switch Mode / Hide / Exit / 分隔线），
而程序实际显示的是 `overlay.py:344` 那套**只有 2 项**的菜单。

**处置**：不迁移。同时把 `build_exe.py` 里那行残留记入待清理项。

### 4.2 热键字符串解析被实现了 4 次，且行为不一致

| 位置 | 实现 | 行为差异 |
|---|---|---|
| `win32_constants.py:195` `vk_code` | 查 `KEY_NAME_MAP` + `SPECIAL_VK` | **支持** `f1`~`f12`、`enter`、`esc` 等特殊键 |
| `hotkey_manager.py:63` `_parse_hotkey` | 只处理单字符 + 修饰键 | **不支持**特殊键，遇 `enter` 返回 vk=0 |
| `hotkey_manager.py:81` `_parse_hotkey_vk` | 查 `SPECIAL_VK` | 支持特殊键（与 `vk_code` 逻辑重复） |
| `input_sender.py:132` `_MOD_VK_MAP` | 自建修饰键映射副本 | 第三次复制同一张表 |

再加上 `win32_constants.py:108` 也有一份 `_MOD_VK_MAP` —— **同一张修饰键表存在两份**。

**危害**：`send_hotkey` 若配成 `"ctrl+enter"`，`RegisterHotKey` 路径（`_parse_hotkey_vk`）能识别，
但若走到 `_parse_hotkey` 路径就会静默得到 vk=0。配置同样的值，不同代码路径行为不同。

**处置**：V2 合并为**一个** `internal/hotkeys` 包，键名映射表外置到 JSON，
并对 C++ 侧使用同一份表做生成或对拍。

### 4.3 其他已在对照表中记录的缺陷

`D1` 三套品牌色 / `accent_color` 死配置、`D2` 广告发送器绑定设置窗口、
`D3` 设置 UI 双实现、`D4` 业务逻辑寄生 UI、`D5` ingest 零测试 —— 见 `migration-matrix.md` §7。
本次新增：

| ID | 缺陷 | 证据 |
|---|---|---|
| **D7** | 热键解析 4 处重复且行为不一致 | §4.2 |
| **D8** | `message_display.py` 死代码 244 行 + `build_exe.py` 残留导入 | §4.1 |
| **D9** | `config.py:217` 的 `accent_color` 之外，`overlay.py` 还有 `_setup_text_tags` 与 `message_display.py` 两套 tag 配色（其中一套已死） | 与 D1 同源 |

---

### 4.4 `hotkey_manager.py` 是死代码（203 行），且暴露「文档与代码不符」

全项目搜 `HotkeyManager` 只有两处命中：它自己的类定义（`hotkey_manager.py:28`）、
`build_exe.py:43` 一行残留的 `--hidden-import`。**`main.py` 完全没有引用它。**

而程序真正在用的全局热键是 `overlay.py:773` `_start_hotkey_poller`——
`GetAsyncKeyState` **每 50 毫秒轮询**一次修饰键与主键状态，在上升沿触发呼出。

**由此产生三个连带问题：**

| # | 问题 | 影响 |
|---|---|---|
| 1 | 发布说明声称「v1.3.0 热键升级为 `RegisterHotKey` 系统级热键」 | **该升级从未接线**，一直没生效 |
| 2 | `README.md:106` 写「`RegisterHotKey` 系统热键，不会被游戏拦截」 | 文档与实现不符（D11） |
| 3 | V1 的「活的」热键解析器（`overlay._parse_hotkey`）**不支持特殊键** | 配置界面允许按 F1，但配了不生效、不报错（D7） |

**处置**：`hotkey_manager.py` 不迁移；热键检测按 ADR-015 统一（该条正在等待决策）。

---

## 5. 这条宪法怎么落地执行

逐文件判定是**开工前的动作**，不是一次性的。落到 V2 的日常流程里：

1. **迁移某模块前，先在这份文档里登记判定**（文件 → 职责 → 语言），有异议先讨论再写代码。
2. **每块职责只归一个语言**，禁止"这块 Go 和 C++ 都写一点"。
3. **对拍工具兜底**：纯函数若在两侧都实现，必须有对拍。
   已建立：`tools/fidelity/dict_fidelity.py`（词库，176 条真实消息逐条比对通过）。
   待建立：热键解析对拍、显示格式对拍。
4. **判定可以改**：读得更深之后发现原判定不对，就改这份文档并写清原因——
   本文档本身就是 v1.0，预期会随阅读深度增加而修订。
