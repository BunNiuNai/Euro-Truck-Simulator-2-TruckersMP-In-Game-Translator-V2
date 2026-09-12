/* Translator V2 — C++ 原生层对外协议与 ABI
 *
 * 对应文档：docs/architecture.md v1.3
 *   ADR-004  Named Pipe + 长度前缀 JSON
 *   ADR-005  C++ 承担全部 Windows 底层能力
 *   ADR-012  期望状态由 Go 持有，实际状态由 C++ 持有
 *   ADR-014  禁止游戏进程注入与渲染 hook（安全硬约束）
 *
 * 帧格式（双向一致）：
 *   +----------------+------------------------------+
 *   | 4 bytes LE     | N bytes                      |
 *   | payload length | UTF-8 JSON payload           |
 *   +----------------+------------------------------+
 *   payload 顶层含 "type" 与 "id"（请求/响应配对）
 *   长度上限 8MB，超限即断开并记录错误
 */

#ifndef TRANSLATOR_NATIVE_H
#define TRANSLATOR_NATIVE_H

#define TN_PROTOCOL_VERSION 1
#define TN_MAX_FRAME_BYTES  (8u * 1024u * 1024u)

/* ── 进程退出码 ───────────────────────────────────────────────
 * Go 侧据此区分「native 起不来」与「已经有一个在跑」：
 * 后者不是故障，重启也解决不了（要先收掉那个还在跑的 native）。
 *
 * 0  正常退出（含 --version / --help）
 * 1  启动失败（管道等资源创建不出来）
 * 2  已有实例在运行：单实例互斥体把人挡在了门外
 *    （V1 main.py:29-40 的等价行为；native 由 Go 拉起，Go 崩了不会带走
 *      native，不挡的话会出现两个进程抢同一个管道名与同一批 Win32 资源）
 */
#define TN_EXIT_OK               0
#define TN_EXIT_STARTUP_FAILED   1
#define TN_EXIT_ALREADY_RUNNING  2

/* 管道名：\\.\pipe\<TN_PIPE_NAME> */
#define TN_PIPE_NAME L"ets2translator-native-v1"

/* ── 修饰键位掩码 ─────────────────────────────────────────────
 * 取值与 Win32 RegisterHotKey 的 MOD_* 以及 Go 侧
 * internal/hotkeys 的 ModAlt/ModControl/ModShift/ModWin **完全一致**，
 * 因此 Go 解析出的 Spec.Mods 可以直接放进 IPC 载荷。
 *
 * 前缀必须是 TN_：windows.h 已经定义了同名的 MOD_ALT / MOD_CONTROL /
 * MOD_SHIFT / MOD_WIN（给 RegisterHotKey 用），直接用会宏冲突。
 */
#define TN_MOD_ALT      0x0001u
#define TN_MOD_CONTROL  0x0002u
#define TN_MOD_SHIFT    0x0004u
#define TN_MOD_WIN      0x0008u

/* ── Go -> Native（指令）──────────────────────────────────── */
#define TN_C2N_HELLO          "native.hello"     /* {protocolVersion}            */
#define TN_C2N_REPLAY         "native.replay"    /* {registry} 全量重放期望状态  */
#define TN_C2N_HOTKEY_SET     "hotkey.set"       /* {list[]{id,combo,enabled}}   */
#define TN_C2N_TRAY_SET       "tray.set"         /* {enabled,menu[],icon}        */
#define TN_C2N_DISPLAY_CREATE "display.create"   /* {mode,bounds,opacity,...}    */
#define TN_C2N_DISPLAY_UPDATE "display.update"   /* {messages[],stats,header}    */
#define TN_C2N_DISPLAY_VISIBLE "display.visible" /* {visible}                    */
/* display.opacity {opacity} —— 只改窗口级 alpha，**不重建窗口**。
 *
 * 为什么需要单独一条而不是复用 display.create：create 对 native 意味着
 * 「先销毁再重建窗口」，用户每拖一下不透明度滑块窗口都会闪一下；
 * 而这里要的只是一次 SetLayeredWindowAttributes。
 *
 * 补这条是因为 `display.create` 只在**建窗时**读一次 opacity，
 * 于是运行期改配置要重启才生效——用户拖滑块看到的是「毫无反应」。 */
#define TN_C2N_DISPLAY_OPACITY "display.opacity"
#define TN_C2N_CLICK_THROUGH  "display.setClickThrough"
#define TN_C2N_INPUT_SEND     "input.send"       /* {text,hotkey,delayMs,confirm}*/
#define TN_C2N_INPUT_COPY     "input.copy"       /* {text}                       */
#define TN_C2N_CLIP_CACHE     "clipboard.cache"  /* {text} 供复制热键本地使用    */
#define TN_C2N_WINDOW_BLUR    "window.blur"      /* {hwnd,mode}                  */
/* {zone:"caption"|"left"|"right"|"top"|"bottom"|"topleft"|"topright"|"bottomleft"|"bottomright"} */
#define TN_C2N_WINDOW_MOVE    "window.beginMoveResize"
/* 打开设置窗口。托盘菜单的「Settings 设置」用它——那条路径上没有人调用
 * window.open，也就没有 NewWindowRequested 事件可蹭。 */
#define TN_C2N_SETTINGS_OPEN  "settings.open"
/* 把悬浮窗带到前台并获得键盘焦点（热键「呼出输入框」用）。 */
#define TN_C2N_WINDOW_FOCUS   "window.focus"
#define TN_C2N_THEME_SET      "theme.set"        /* {dark} 窗口材质深浅          */
#define TN_C2N_PING           "ping"
#define TN_C2N_SHUTDOWN       "shutdown"

/* ── Native -> Go（事件）──────────────────────────────────── */
#define TN_N2C_READY          "native.ready"     /* {protocolVersion,capabilities[],actualState} */
#define TN_N2C_HOTKEY         "event.hotkey"     /* {id}                         */
#define TN_N2C_TRAY_COMMAND   "event.trayCommand"/* {id}                         */
#define TN_N2C_INPUT_RESULT   "event.inputResult"/* {ok,reason}                  */
#define TN_N2C_DISPLAY_READY  "event.displayReady"
#define TN_N2C_STATE_CHANGED  "event.stateChanged"
/* {x,y,width,height} 用户拖动/缩放**结束**后上报的新几何。
 * Go 据此更新期望状态并去抖落盘（V1 overlay.py:415 `_schedule_save_position`）。 */
#define TN_N2C_WINDOW_CHANGED "event.windowChanged"
/* {x,y,width,height} 用户移动/缩放**设置窗口**后上报的新几何。
 * 与 windowChanged 分开是因为两个窗口的几何各自独立存储。 */
#define TN_N2C_SETTINGS_GEOMETRY "event.settingsGeometry"
#define TN_N2C_ERROR          "event.error"      /* {code,message}               */
#define TN_N2C_PONG           "pong"

/* ── 命令行 ───────────────────────────────────────────────── */
/* translator_native.exe --version   打印协议版本后退出（首版唯一功能）
 * translator_native.exe             启动 Named Pipe 服务（阶段 1 实现）
 */

#endif /* TRANSLATOR_NATIVE_H */
