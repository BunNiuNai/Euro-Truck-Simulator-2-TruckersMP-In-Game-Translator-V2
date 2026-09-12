#!/usr/bin/env python
"""Translator V2 — 生成项目骨架

按 docs/architecture.md v1.3 第 13 节的目录结构创建骨架文件。
生成的文件都是**真实源码文件**（不是占位符），脚本只是省去手写重复的
包文档与样板。已存在的文件默认不覆盖。

    python scaffold.py              # 生成缺失的文件
    python scaffold.py --force      # 覆盖已存在的文件（**手工改写的仍受保护**）
    python scaffold.py --list       # 只列出将要生成的文件

⚠️ `--force` **不会**覆盖已被手工改写的文件。

判据是「磁盘内容 ≠ 本脚本会生成的内容」，而不是一份手写的名单。
手写名单一定会过期——这里原本就有一份三条的名单，而它漏了
`native/src/main.cpp` 与 `native/CMakeLists.txt`：这两个文件在骨架之后被大幅
改写（消息泵、托盘命令、窗口几何上报，以及 CMakeLists 里新增的 8 个源文件）。
`--force` 覆盖它们会把程序打回「编译不过」或「起来是个空壳」的状态，
而且没有任何即时提示——用户以为只是「重新生成了骨架」。

被保护的文件会列在输出末尾。确认要丢弃它们时用 `--overwrite-handwritten`。

本脚本里的常量（如 FE_CLIENT_TS）是**初版骨架**，与真实实现已有很大差异。
新增文件请直接用编辑器写。

设计约束（来自 ADR-005「C++ 组织方式约束」）：
  · 每一块代码都要「小而自证」——C++ 首版保持到「一定能编译通过」的最小程度，
    不预先塞入无法编译验证的 COM / Win32 复杂逻辑。
  · 生成的 doc.go 明确写出每个包的职责、V1 来源与不变量，避免边界漂移。
"""
from __future__ import annotations

import argparse
import os
import sys

for _s in (sys.stdout, sys.stderr):
    if hasattr(_s, "reconfigure"):
        try:
            _s.reconfigure(encoding="utf-8", errors="replace")
        except (ValueError, OSError):
            pass

HERE = os.path.dirname(os.path.abspath(__file__))
V2 = os.path.abspath(os.path.join(HERE, ".."))   # Translator-V2/
REPO = os.path.dirname(V2)                       # 仓库根
MODULE = "github.com/BunNiuNai/ets2-translator-v2/backend"
PROTOCOL_VERSION = 1

# ═══════════════════════════════════════════════════════════════
#  Go 包文档：职责 / V1 来源 / 不变量
# ═══════════════════════════════════════════════════════════════

GO_PACKAGES = {
    "domain": (
        "领域对象与错误码。",
        "message_types.py（DisplayMessage / TranslationStats）",
        "· Event / RenderedMessage / Stats 是 Source→Engine→Sink 之间唯一的通信载体\n"
        "  · 错误码文案必须与 V1 translator.py:_format_error 逐字一致\n"
        "  · 不得引入 V1 没有的用户可见字段（首要原则 P1）",
    ),
    "ingest": (
        "数据源层（Source 接口）与 chatlog 实现。",
        "monitor.py",
        "· V1 的双 glob 回退顺序不可改变：chat_*_log.txt → chat_*_log_*.txt → chat_*.txt\n"
        "  · 按 mtime 取最新；支持切档检测与增量读取\n"
        "  · 启动时跳过历史消息（只读增量）\n"
        "  · 去重键 = 玩家 + 内容 + 时间戳\n"
        "  · 必须识别系统消息、服务器名、玩家昵称\n"
        "  · V1 此层零测试覆盖，V2 必须补齐（见迁移对照表 D5）",
    ),
    "engine": (
        "翻译编排：批处理、混合语言拆分、语言检测、出口后处理。",
        "translator.py（Translator.run / _flush / _flush_llm / split_mixed_text / "
        "reassemble_mixed / detect_language）",
        "· 批处理窗口 0.3s，满 8 条立即 flush\n"
        "  · 批量分隔符为 \\n---\\n；LLM 未按分隔符回显时必须逐条回退翻译（不可回退此修复）\n"
        "  · 混合语言拆分：中文片段保留不译\n"
        "  · 非文字内容（标点/数字/emoji）直接跳过\n"
        "  · 出口后处理顺序：补译缩写 → 保留 @玩家名 → 未翻译校验（失败则回退下一家）",
    ),
    "providers": (
        "Provider 接口与注册表、并行竞速、轮转、熔断、模型拉取、连通性测试。",
        "translator.py（_call_provider / _call_api_internal / ProviderHealth）+ model_fetcher.py",
        "· 竞速语义：全部启用 Provider 并行，先返回且通过 looks_untranslated 校验者胜\n"
        "  · 熔断：连续 3 次失败进入冷却 min(30*2^(n-3), 120) 秒，成功即恢复\n"
        "  · 轮转分流：单 Provider 直连；多 Provider 按轮转索引分配\n"
        "  · 配置字段全部保留（ProviderConfig 13 字段），否则无法迁移用户配置\n"
        "  · api_format 支持 openai / anthropic 两种端点拼接",
    ),
    "dictionary": (
        "五层术语处理与 resources/dictionary.json 加载校验。",
        "chat_dictionary.py",
        "· 五层顺序不可调换：slang → phrases → structuredActions → promptMapping → ets2Terms\n"
        "  · pauseAfter 必须保留，它决定结构化短语的拼接边界\n"
        "  · 加载校验为条目级：非法项跳过并记 WARN，不使整个词库失效\n"
        "  · 三层合并优先级：用户覆盖 > 在线更新 > 内置",
    ),
    "cache": (
        "LRU 翻译缓存与同文本并发合并。",
        "translator.py（LRUCache / _in_flight / _in_flight_results）",
        "· 容量 1000 条\n"
        "  · 相同原文并发到达时合并为一次 API 调用（singleflight）\n"
        "  · 缓存命中与本地词典命中都必须计入 Stats，且不计为 translated",
    ),
    "config": (
        "配置模型、DPAPI 密钥加密、原子写、热重载。",
        "config.py",
        "· V2 使用全新配置目录，不读 V1 配置（ADR-008）\n"
        "  · 密钥用 DPAPI 加密，配置文件中不得出现明文\n"
        "  · DPAPI 解密失败时保留原密文值，绝不写空串（防止密钥永久丢失）\n"
        "  · 一律原子写；损坏时备份为 .corrupted.{timestamp} 后重建\n"
        "  · 主目录不可写时回退 %LOCALAPPDATA%\\ETS2 Translator\\\n"
        "  · 热重载：3 秒 mtime 轮询",
    ),
    "logger": (
        "文件日志（轮转）+ 内存环形缓冲。",
        "logger.py",
        "· 保留 7 个文件、单文件上限 2MB、内存缓冲 500 行\n"
        "  · 翻译日志格式：时间 - 厂商-模型名 - 原文 - 译文\n"
        "  · 线程安全；日志删除后能重新打开文件\n"
        "  · 定期清理任务每 6 小时一次，且与 UI 生命周期无关（ADR-007）",
    ),
    "task": (
        "后台任务队列：定时广告发送器、发送链路。",
        "main.py（_ad_*）+ compose_sender.py",
        "· AdSender 生命周期不得绑定任何 UI——V1 的缺陷是关掉设置窗口广告就停（D2）\n"
        "  · SendPipeline 互斥，并发时返回 BUSY（V1 SendResult 5 态语义保留）\n"
        "  · 发送确认通过回读聊天日志文件，不消费 engine 的消息队列",
    ),
    "sink": (
        "输出层（Sink 接口）：webview、log。",
        "overlay.py（显示部分）",
        "· 投递失败不得阻塞 Engine；队列满时丢弃最旧消息并计数\n"
        "  · native 不可用时 Available() 返回 false，消息进入待重放缓冲（ADR-012）\n"
        "  · 只呈现，不做业务决策",
    ),
    "capability": (
        "期望状态注册表与崩溃恢复重放（ADR-012）。",
        "新增（C++ 分层带来的必要配套，属内部机制）",
        "· Go 持期望状态（热键/托盘/显示），native 持实际状态\n"
        "  · native 重启后由 Replay 全量重放，幂等\n"
        "  · 与实际状态差异记 WARN 日志，不在 UI 新增展示（P1）",
    ),
    "ipc": (
        "Named Pipe 客户端、帧编解码、心跳、请求超时（ADR-004）。",
        "新增",
        "· 帧格式：4 字节小端长度 + UTF-8 JSON payload，上限 8MB\n"
        "  · 三种模式：请求/响应、服务端推送事件、心跳 ping/pong\n"
        "  · 心跳 1 秒一次，连续 3 次无 pong 判定 native 失联\n"
        "  · 单次调用超时不得阻塞 Engine",
    ),
    "api": (
        "REST 与 WebSocket 接口（前端唯一入口，ADR-009）。",
        "新增（替代 V1 的 UI 直连）",
        "· 契约见 docs/architecture.md 第 9 节\n"
        "  · 错误码与文案必须与 V1 一致\n"
        "  · 前端不做本地持久化，配置/日志/统计唯一事实源在 Go",
    ),
}

# ═══════════════════════════════════════════════════════════════
#  C++ 原生层
# ═══════════════════════════════════════════════════════════════

NATIVE_HEADER = r'''/* Translator V2 — C++ 原生层对外协议与 ABI
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

/* 管道名：\\.\pipe\<TN_PIPE_NAME> */
#define TN_PIPE_NAME L"ets2translator-native-v1"

/* ── Go -> Native（指令）──────────────────────────────────── */
#define TN_C2N_HELLO          "native.hello"     /* {protocolVersion}            */
#define TN_C2N_REPLAY         "native.replay"    /* {registry} 全量重放期望状态  */
#define TN_C2N_HOTKEY_SET     "hotkey.set"       /* {list[]{id,combo,enabled}}   */
#define TN_C2N_TRAY_SET       "tray.set"         /* {enabled,menu[],icon}        */
#define TN_C2N_DISPLAY_CREATE "display.create"   /* {mode,bounds,opacity,...}    */
#define TN_C2N_DISPLAY_UPDATE "display.update"   /* {messages[],stats,header}    */
#define TN_C2N_DISPLAY_VISIBLE "display.visible" /* {visible}                    */
#define TN_C2N_CLICK_THROUGH  "display.setClickThrough"
#define TN_C2N_INPUT_SEND     "input.send"       /* {text,hotkey,delayMs,confirm}*/
#define TN_C2N_INPUT_COPY     "input.copy"       /* {text}                       */
#define TN_C2N_CLIP_CACHE     "clipboard.cache"  /* {text} 供复制热键本地使用    */
#define TN_C2N_WINDOW_BLUR    "window.blur"      /* {hwnd,mode}                  */
#define TN_C2N_WINDOW_MOVE    "window.beginMoveResize"
/* {zone:"caption"|"left"|"right"|"top"|"bottom"|"topleft"|"topright"|"bottomleft"|"bottomright"} */
/* 打开设置窗口。托盘菜单的「Settings 设置」用它——那条路径上没有人调用
 * window.open，也就没有 NewWindowRequested 事件可蹭。 */
#define TN_C2N_SETTINGS_OPEN  "settings.open"
/* 把悬浮窗带到前台并获得键盘焦点（热键「呼出输入框」用）。 */
#define TN_C2N_WINDOW_FOCUS   "window.focus"
/* ⚠️ 这一条之前在生成器里漏了：头文件里有、生成器里没有，重新生成就会把
 * theme.set 整条抹掉，native 从此不认这条命令。生成器必须与产出一一对应。 */
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
'''

NATIVE_MAIN = r'''/* Translator V2 — Native 进程入口
 *
 * 阶段 0 的骨架刻意保持到「一定能编译通过」的最小程度（ADR-005 组织约束：
 * 单元要小且自证）。这里只做版本打印，Named Pipe 服务在阶段 1 实现——
 * 届时按 translator_native.h 的帧格式逐个消息类型补齐，每补一种即可独立验证。
 *
 * 注意：本进程不得向游戏进程注入任何代码（ADR-014）。
 */

#include "translator_native.h"

#include <cstdio>
#include <cstring>

int main(int argc, char** argv) {
    for (int i = 1; i < argc; ++i) {
        if (std::strcmp(argv[i], "--version") == 0) {
            std::printf("translator_native protocol=%d\n", TN_PROTOCOL_VERSION);
            return 0;
        }
        if (std::strcmp(argv[i], "--help") == 0) {
            std::printf(
                "translator_native — Translator V2 Windows capability layer\n"
                "  --version   print protocol version and exit\n"
                "  --help      this message\n"
                "  (no args)   start named pipe server (phase 1)\n");
            return 0;
        }
    }

    std::printf("translator_native protocol=%d\n", TN_PROTOCOL_VERSION);
    std::printf("pipe name: %ls\n", TN_PIPE_NAME);
    std::printf("[phase 0] named pipe server not implemented yet\n");
    return 0;
}
'''

NATIVE_CMAKE = r'''# Translator V2 — Native (C++) 构建
#
# 骨架阶段刻意极简：只用 C++ 标准库，不引入 WebView2 / DirectX 依赖，
# 保证「装了 MSVC 就能编译通过」（ADR-005 组织约束）。
#
# 构建：
#   cmake -S . -B build -G "Visual Studio 17 2022" -A x64
#   cmake --build build --config Release
#
# 阶段 1 起才加入：webview/(WebView2 宿主)、hotkey/、input/、tray/、window/、pipe/

cmake_minimum_required(VERSION 3.20)
project(translator_native LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(NOT WIN32)
    message(FATAL_ERROR "translator_native 仅支持 Windows（ADR-014 / 非目标 N4）")
endif()

add_executable(translator_native
    src/main.cpp
)

target_include_directories(translator_native PRIVATE include)

if(MSVC)
    target_compile_options(translator_native PRIVATE /W4 /utf-8)
else()
    target_compile_options(translator_native PRIVATE -Wall -Wextra)
endif()

# 输出到 dist/，与 Go 侧打包脚本约定一致
set_target_properties(translator_native PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY "${CMAKE_SOURCE_DIR}/../dist"
    RUNTIME_OUTPUT_DIRECTORY_DEBUG "${CMAKE_SOURCE_DIR}/../dist"
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_SOURCE_DIR}/../dist"
)
'''

# ═══════════════════════════════════════════════════════════════
#  Frontend (TypeScript SPA)
# ═══════════════════════════════════════════════════════════════

FE_PACKAGE_JSON = r'''{
  "name": "ets2-translator-frontend",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "description": "Translator V2 前端 SPA — 由 native 宿主 WebView2，经 REST + WS 与 Go 通信（ADR-013）",
  "scripts": {
    "dev": "vite",
    "build": "tsc --noEmit && vite build",
    "preview": "vite preview",
    "typecheck": "tsc --noEmit"
  },
  "devDependencies": {
    "typescript": "^5.6.0",
    "vite": "^5.4.0"
  }
}
'''

FE_TSCONFIG = r'''{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noImplicitOverride": true,
    "noFallthroughCasesInSwitch": true,
    "isolatedModules": true,
    "skipLibCheck": true,
    "noEmit": true,
    "types": []
  },
  "include": ["src"]
}
'''

FE_VITE_CONFIG = r'''import { defineConfig } from 'vite'

// 前端由 native 宿主 WebView2 加载，资源由 Go 经本机 HTTP 提供（ADR-013）。
// 开发期直接在浏览器里对着 Go 调试即可，无需桌面框架。
export default defineConfig({
  server: {
    port: 5273,
    strictPort: true,
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
  },
})
'''

FE_INDEX_HTML = r'''<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>ETS2 Translator</title>
    <!-- 配色取自 V1 overlay.py:32-34（唯一品牌色来源，见迁移对照表 D1） -->
    <style>
      :root {
        --accent: #4494fc;      /* 主色调蓝 — 顶部高亮条、标题栏 */
        --border-blue: #60a8ff; /* 描边线蓝 — 外边框、分隔线 */
        --highlight: #70b8ff;   /* 高亮文字蓝 — 用户名、选中态 */
        --bg: #000000;
        --fg: #cccccc;
        --fg-dim: #858585;
        --fg-timestamp: #666666;
        --translation: #ffd700; /* 译文金色 */
        --self-green: #4ec9b0;
        --error-red: #f44747;
      }
      html, body { margin: 0; height: 100%; background: var(--bg); color: var(--fg); }
      body { font-family: "Microsoft YaHei", system-ui, sans-serif; font-size: 12px; }
      #app { height: 100%; display: flex; flex-direction: column; }
    </style>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.ts"></script>
  </body>
</html>
'''

FE_MAIN_TS = r'''/**
 * Translator V2 前端入口（骨架）
 *
 * 边界（ADR-013 / ADR-009）：
 *   · 本层只做呈现与输入，不持有业务状态
 *   · 与 Go 的唯一通道是 REST + WebSocket，不用任何桌面框架的私有绑定
 *   · 显示层只有一条路径：WebView 透明置顶窗口（ADR-011）
 *
 * 阶段 3 的实现清单见 docs/migration-matrix.md 第 3 节 W 组。
 */

import { createTranslatorClient } from './api/client'

const app = document.querySelector<HTMLDivElement>('#app')
if (!app) throw new Error('#app not found')

app.innerHTML = `
  <header style="padding:8px 10px;border-bottom:1px solid var(--border-blue)">
    <span style="color:var(--highlight)">ETS2 Translator V2</span>
    <span id="status" style="float:right;color:var(--fg-timestamp)">连接中…</span>
  </header>
  <main id="messages" style="flex:1;overflow-y:auto;padding:6px 10px"></main>
  <footer style="padding:6px 10px;border-top:1px solid var(--border-blue)">
    <input id="compose" placeholder="输入中文，回车发送"
           style="width:100%;background:#1e1e1e;border:0;color:var(--fg);padding:6px" />
  </footer>
`

const status = document.querySelector<HTMLSpanElement>('#status')!

const client = createTranslatorClient({
  onStatus: (text) => { status.textContent = text },
  onMessage: (msg) => {
    // ⚠️ 安全：玩家名与聊天内容来自联机服务器上的其他玩家，属于不可信输入。
    // V1 用 tkinter Text 控件渲染，天然没有 HTML 注入面；V2 换成 WebView 后
    // 这个面出现了。因此**一律用 textContent，绝不用 innerHTML 拼接动态值**。
    const row = document.createElement('div')
    row.style.marginBottom = '6px'

    const speaker = document.createElement('span')
    speaker.style.color = 'var(--highlight)'
    speaker.style.fontWeight = 'bold'
    speaker.textContent = msg.speaker

    const time = document.createElement('span')
    time.style.cssText = 'color:var(--fg-timestamp);float:right'
    time.textContent = msg.timestamp

    const text = document.createElement('div')
    text.style.color = 'var(--translation)'
    text.textContent = msg.translated

    row.append(speaker, time, text)
    document.querySelector('#messages')!.appendChild(row)
  },
})

client.connect()

// ── 以下为骨架占位，阶段 3 按对照表逐条实现并勾除 ──
// W2  四段式纵向布局（accent 线 / 标题栏 / 消息区 / 输入栏 / 统计栏）
// W3  配色统一（唯一 token 集，修掉 V1 的三套品牌色问题 D1）
// W8  输入栏 + 回车发送
// W9  译文金色高亮（已在上方样式体现）
// W10 时间戳右对齐
// W13 消息分割线 + 最大条数
// W20 北京时间显示
// W25 消息区右键菜单（仅 Settings / Exit）
// 0.1 交互：滚动 / 拖拽移动 / 边缘缩放——不得超出 V1 的 5 项（P1）
export {}
'''

FE_CLIENT_TS = r'''/**
 * 与 Go 后端通信的唯一入口（ADR-009）
 *
 * 契约见 docs/architecture.md 第 9 节：
 *   REST  /api/*
 *   WS    /ws     事件：message.translated / stats.updated / provider.health
 *                       log.appended / source.status / config.reloaded
 *                       native.status / notice
 */

export interface RenderedMessage {
  id: string
  speaker: string
  original: string
  translated: string
  lang: string
  timestamp: string
  isSelf: boolean
  isSystem: boolean
  provider: string
  model: string
  cacheState: 'local_dict' | 'hit' | 'miss'
  latencyMs: number
  errCode: string
}

export interface ClientOptions {
  baseUrl?: string
  onMessage?: (msg: RenderedMessage) => void
  onStatus?: (text: string) => void
  onNotice?: (text: string, level: string) => void
}

export interface TranslatorClient {
  connect(): void
  close(): void
  translate(text: string, mode?: 'receive' | 'send'): Promise<unknown>
  send(text: string): Promise<unknown>
  getConfig(): Promise<unknown>
  putConfig(cfg: unknown): Promise<unknown>
}

export function createTranslatorClient(opts: ClientOptions = {}): TranslatorClient {
  // 开发期直连 Go 的 HTTP 服务；生产环境由 native 注入实际端口
  const baseUrl = opts.baseUrl ?? 'http://127.0.0.1:8791'
  let ws: WebSocket | null = null

  async function rest<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetch(baseUrl + path, {
      headers: { 'Content-Type': 'application/json' },
      ...init,
    })
    if (!res.ok) throw new Error(`${path} -> HTTP ${res.status}`)
    return (await res.json()) as T
  }

  return {
    connect() {
      const url = baseUrl.replace(/^http/, 'ws') + '/ws'
      ws = new WebSocket(url)
      ws.onopen = () => opts.onStatus?.('已连接')
      ws.onclose = () => opts.onStatus?.('已断开')
      ws.onerror = () => opts.onStatus?.('连接错误')
      ws.onmessage = (ev) => {
        try {
          const env = JSON.parse(ev.data as string) as { type: string; payload: unknown }
          if (env.type === 'message.translated') {
            opts.onMessage?.(env.payload as RenderedMessage)
          } else if (env.type === 'notice') {
            const p = env.payload as { text: string; level: string }
            opts.onNotice?.(p.text, p.level)
          }
        } catch {
          /* 忽略无法解析的帧 */
        }
      }
    },
    close() { ws?.close(); ws = null },
    translate: (text, mode = 'receive') =>
      rest('/api/translate', { method: 'POST', body: JSON.stringify({ text, mode }) }),
    send: (text) =>
      rest('/api/send', { method: 'POST', body: JSON.stringify({ text }) }),
    getConfig: () => rest('/api/config'),
    putConfig: (cfg) => rest('/api/config', { method: 'PUT', body: JSON.stringify(cfg) }),
  }
}
'''

# ═══════════════════════════════════════════════════════════════
#  Go 入口
# ═══════════════════════════════════════════════════════════════

GO_MAIN = r'''// Translator V2 — Go 侧进程入口
//
// 职责（docs/architecture.md v1.3 §7.1）：
//   · 单实例互斥体（由 native 提供，见 OF-1）
//   · 拉起并监督 native 子进程，心跳检测与期望状态重放（ADR-012）
//   · 启动 ingest → engine → sink 流水线
//   · 启动 REST / WebSocket 服务供前端使用（ADR-009）
//
// 硬约束：
//   · 业务逻辑全部留在 Go，不跨语言拆分（非目标 N3）
//   · 不新增 V1 没有的用户可见功能（首要原则 P1）
package main

import (
	"flag"
	"fmt"
	"os"
)

// version 由构建期通过 -ldflags "-X main.version=..." 注入
var version = "v2.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("ets2-translator (Go) %s\n", version)
		return
	}

	fmt.Printf("ets2-translator %s\n", version)
	fmt.Println("[phase 0] 骨架：ingest / engine / providers / api 尚未接入")
	fmt.Println("下一步见 docs/architecture.md §14 阶段 1")
	os.Exit(0)
}
'''

GO_MOD = f'''module {MODULE}

go 1.23

// 依赖策略：阶段 0 不引入任何第三方模块，保证 `go build ./...` 无需网络即可通过。
// 阶段 1 起按需加入：
//   golang.org/x/sys/windows   Win32 调用（DPAPI、注册表）
//   golang.org/x/sync          singleflight（同文本并发合并）
'''

# ═══════════════════════════════════════════════════════════════
#  README
# ═══════════════════════════════════════════════════════════════

ROOT_README = r'''# Translator V2

Euro Truck Simulator 2 · TruckersMP 游戏内聊天翻译器 — 第二代架构。

> **首要原则 P1：功能与 V1 完全一致，只更换实现。**
> V2 的功能范围 = V1 的功能范围，不多不少。验收基线见
> [`docs/migration-matrix.md`](docs/migration-matrix.md)（70 条 parity 能力 + 99 个测试用例 + 14 项基线指标）。

## 文档

| 文档 | 内容 |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | 架构设计 v1.3：14 条 ADR、Source/Engine/Sink 抽象、IPC 协议、API 契约 |
| [`docs/migration-matrix.md`](docs/migration-matrix.md) | V1→V2 能力迁移对照表（验收基线） |
| [`docs/language-assignment.md`](docs/language-assignment.md) | **逐文件语言归属判定**（读每个文件判断它干什么，再定语言） |

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
`overlay.py` 拆 5 块）。逐文件判定见 [`docs/language-assignment.md`](docs/language-assignment.md)。

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
'''

BACKEND_README = f'''# backend/ — Go 业务大脑

模块路径：`{MODULE}`（占位，可改；改后同步 CI 与打包脚本）

## 包职责

每个包的 `doc.go` 写明了「职责 / V1 来源 / 不变量」三件事，改代码前请先读它。

| 包 | 职责 | V1 来源 |
|---|---|---|
| `domain` | 领域对象与错误码 | `message_types.py` |
| `ingest` | Source 接口 + chatlog 实现 | `monitor.py` |
| `engine` | 批处理 / 混合语言 / 语言检测 / 后处理 | `translator.py` |
| `providers` | 竞速 / 轮转 / 熔断 / 模型拉取 / 连通测试 | `translator.py` + `model_fetcher.py` |
| `dictionary` | 五层术语处理 | `chat_dictionary.py` |
| `cache` | LRU 1000 + 同文本合并 | `translator.py` |
| `config` | 配置 / DPAPI / 原子写 / 热重载 | `config.py` |
| `logger` | 日志轮转 + 内存缓冲 | `logger.py` |
| `task` | 广告发送器 / 发送链路 | `main.py` + `compose_sender.py` |
| `sink` | 输出层 | `overlay.py` |
| `capability` | 期望状态重放（ADR-012，内部机制） | 新增 |
| `ipc` | Named Pipe 帧编解码 + 心跳 | 新增 |
| `api` | REST + WebSocket | 新增 |

## 依赖策略

阶段 0 **不引入任何第三方模块**，`go build ./...` 无需网络即可通过。
阶段 1 起按需加入 `golang.org/x/sys/windows`（DPAPI / 注册表）与
`golang.org/x/sync`（singleflight）。

## 构建

```powershell
cd backend
go build ./...
go test ./...
go run ./cmd/translator --version
```

## 待办

- **OF-1**：单实例互斥体归 native（`native/window`）。Go 启动时需等待
  `native.ready` 才能确定自己是否为唯一实例，见 `docs/architecture.md` §4.3 启动顺序。
'''

NATIVE_README = '''# native/ — C++ Windows 能力层

阶段 1 起在主链路。**不含任何业务逻辑**（非目标 N3）。

## 模块（按阶段推进）

| 模块 | 职责 | V1 来源 | 阶段 |
|---|---|---|---|
| `pipe/` | Named Pipe 服务端 + 帧编解码 + 心跳 | 新增 | 1 |
| `hotkey/` | `RegisterHotKey` + 消息循环（固定 OS 线程） | `hotkey_manager.py` | 1 |
| `tray/` | `Shell_NotifyIcon` + 右键菜单 | `tray_icon.py` | 1 |
| `window/` | 透明置顶、毛玻璃、穿透、拖拽缩放、单实例互斥体 | `acrylic_helper.py` + `win32_constants.py` | 1 / 3 |
| `input/` | `SendInput` + 剪贴板 | `input_sender.py` | 2 |
| `webview/` | WebView2 宿主（创建 HWND 并承载前端 SPA） | 新增（ADR-013） | 3 |

~~`overlay/`~~（DirectX 渲染层）与 ~~`capture/`~~（WGC 捕获）**已取消**，见 ADR-011 / ADR-014 / Q3。

## C++ 组织方式约束（ADR-005，因「AI 生成 + 人工排错」而定）

1. **单元要小而自证**：每个模块必须能独立编译，并附带一个可直接运行的验证入口。
2. **优先用库，不手写 COM**：确需直接调 `ICoreWebView2` 时，COM 交互集中隔离在 `webview/` 一个模块内。
3. **IPC 边界要可观测**：`pipe/` 必须支持把收发帧打印到文件（调试开关）——跨进程问题无法单步断点排查。
4. **失败要显式**：所有 Win32 调用检查返回值并回报 `event.error`，不静默忽略。

## 构建

```powershell
cmake -S native -B native/build -G "Visual Studio 17 2022" -A x64
cmake --build native/build --config Release
native/build/Release/translator_native.exe --version
```

输出到 `Translator-V2/dist/translator_native.exe`。

## 协议

对外协议与消息类型全部定义在 `include/translator_native.h`：
帧格式 `[4字节小端长度][UTF-8 JSON]`，上限 8MB。
消息表见 `docs/architecture.md` §8.2。
'''

FRONTEND_README = '''# frontend/ — TypeScript SPA

## 为什么不是 Wails / Tauri（ADR-003 / ADR-013）

两个框架都要自己拥有窗口（HWND）。而 ADR-005 已把全部 Windows 底层能力
（含窗口操作）划给 C++，若窗口由框架创建、毛玻璃/穿透/拖拽由 native 操作，
就会出现「一个 HWND 两个所有者」的竞态。

因此：**native 宿主 WebView2，前端退化为纯 SPA**，通过 REST + WebSocket 与 Go 通信。
好处是前端不依赖任何桌面框架，开发期可以直接在浏览器里对着 Go 调试。

## 开发

```powershell
cd frontend
npm install
npm run dev        # Vite dev server，浏览器直接打开即可调试
npm run typecheck
npm run build      # 产物给 native/Go 加载
```

## 边界（ADR-009）

**只做呈现与输入，不持有业务状态。** 配置、日志、统计的唯一事实源在 Go。

## 显示层（ADR-011）

只有一条路径：WebView 透明置顶窗口。V1 的两种模式保持不变：
`standalone`（标准窗口）/ `overlay`（透明置顶悬浮窗）。

交互范围**严格等于 V1 的 5 项**，不得超出（首要原则 P1）：
滚动消息、拖拽移动、边缘缩放、消息区右键菜单（Settings / Exit）、热键复制译文。

**明确不做**：点击消息复制、消息悬停高亮、消息文字选中——V1 均无此功能。

## 配色

唯一来源为 V1 `overlay.py:32-34`：`#4494FC` / `#60A8FF` / `#70B8FF`。
V1 存在的三套品牌色并存与 `accent_color` 死配置问题（对照表 D1）在 V2 中修复。

## ⚠️ V2 新引入的安全面：HTML 注入

V1 用 tkinter `Text` 控件渲染消息，**天然没有 HTML 注入面**。
V2 换成 WebView 之后这个面出现了——而聊天内容与玩家名**全部来自联机服务器上的
其他玩家**，是不可信输入。

规则：

- 渲染动态数据**一律用 `textContent`**，禁止 `innerHTML` / `insertAdjacentHTML` / `outerHTML`。
- 需要富文本时，用 `document.createElement` 逐节点构造，不要拼 HTML 字符串。
- 任何来自 `/api/*` 或 `/ws` 的字符串都按不可信处理。

这是 V2 相对 V1 的**新增风险**，不属于「功能对齐」范畴，必须在实现中守住。
'''


# ═══════════════════════════════════════════════════════════════
#  写盘
# ═══════════════════════════════════════════════════════════════

GITIGNORE = '''# 构建产物
backend/translator
backend/translator.exe
native/build/
dist/
frontend/node_modules/
frontend/dist/

# Go 构建缓存（默认在 %LOCALAPPDATA%\\go-build；沙箱环境用工作区内缓存）
.gocache/

# 基线测量的产物（勿提交；结果记录在 tools/baseline/RESULTS-v1.md）
tools/baseline/out/

# 运行期数据（真实配置在 文档\\ETS2 Translator\\，不在此目录）
*.log
'''

def collect_files() -> dict[str, str]:
    files: dict[str, str] = {}

    for pkg, (duty, origin, invariants) in GO_PACKAGES.items():
        files[f"backend/internal/{pkg}/doc.go"] = (
            f"// Package {pkg} {duty}\n"
            f"//\n"
            f"// V1 来源：{origin}\n"
            f"//\n"
            f"// 不变量（改动前请确认没有破坏这些行为）：\n"
            + "".join(f"//   {line.strip()}\n" for line in invariants.splitlines())
            + f"package {pkg}\n"
        )

    files["backend/go.mod"] = GO_MOD
    files["backend/cmd/translator/main.go"] = GO_MAIN
    files["backend/README.md"] = BACKEND_README

    files["native/include/translator_native.h"] = NATIVE_HEADER
    files["native/src/main.cpp"] = NATIVE_MAIN
    files["native/CMakeLists.txt"] = NATIVE_CMAKE
    files["native/README.md"] = NATIVE_README

    files["frontend/package.json"] = FE_PACKAGE_JSON
    files["frontend/tsconfig.json"] = FE_TSCONFIG
    files["frontend/vite.config.ts"] = FE_VITE_CONFIG
    files["frontend/index.html"] = FE_INDEX_HTML
    files["frontend/src/main.ts"] = FE_MAIN_TS
    files["frontend/src/api/client.ts"] = FE_CLIENT_TS
    files["frontend/README.md"] = FRONTEND_README

    files["README.md"] = ROOT_README
    files[".gitignore"] = GITIGNORE
    return files


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--force", action="store_true",
                    help="覆盖已存在的文件（**已被手工改写的文件仍会被保护**）")
    ap.add_argument("--overwrite-handwritten", action="store_true",
                    help="连手工改写过的文件也一起覆盖（危险，会丢失真实实现）")
    ap.add_argument("--list", action="store_true", help="只列出将生成的文件")
    args = ap.parse_args()

    files = collect_files()

    if args.list:
        for rel in sorted(files):
            exists = os.path.exists(os.path.join(V2, rel))
            print(f"  {'[已有]' if exists else '[新建]'} {rel}")
        print(f"\n共 {len(files)} 个文件")
        return

    created = skipped = protected = 0
    protected_names = []

    for rel, content in sorted(files.items()):
        path = os.path.join(V2, rel)
        if os.path.exists(path):
            if not args.force:
                print(f"  跳过（已存在）  {rel}")
                skipped += 1
                continue

            # ── 保护手工改写的文件 ──
            #
            # 判据是「磁盘上的内容与本脚本会生成的不一样」。
            #
            # 为什么不维护一份硬编码的「不要 --force」名单：那种名单一定会过期。
            # 这份脚本的文档字符串里原本就有一份三条的名单，而它**漏了
            # native/src/main.cpp 与 native/CMakeLists.txt**——这两个文件在
            # 骨架之后被大幅改写（消息泵、托盘命令、几何上报、8 个新增源文件），
            # `--force` 一次就会把程序打回编译不过或起来是空壳的状态，
            # 而且没有任何即时提示：用户以为只是「重新生成了骨架」。
            # 内容比对不需要维护，也就不会过期。
            try:
                with open(path, "r", encoding="utf-8", newline="") as fh:
                    current = fh.read()
            except (OSError, UnicodeDecodeError) as exc:
                print(f"  保护（读不了，不敢覆盖）  {rel}  [{exc}]")
                protected += 1
                protected_names.append(rel)
                continue

            # 换行归一后再比：脚本按 LF 写，Windows 上的编辑器可能存成 CRLF，
            # 那种差异不代表内容被改写过。
            if current.replace("\r\n", "\n") != content and not args.overwrite_handwritten:
                print(f"  保护（已被手工改写）  {rel}")
                protected += 1
                protected_names.append(rel)
                continue

        os.makedirs(os.path.dirname(path), exist_ok=True)
        # 统一 LF 换行；Go / CMake / TS 均可接受，且避免 Windows 上 CRLF 混入
        with open(path, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(content)
        print(f"  写入            {rel}  ({len(content):,} chars)")
        created += 1

    print(f"\n完成：新建/覆盖 {created} 个，跳过 {skipped} 个，保护 {protected} 个")
    if protected_names:
        print("\n以下文件是**真实实现**、不等于骨架内容，已拒绝覆盖：")
        for rel in protected_names:
            print(f"    {rel}")
        print("确认要丢弃它们的话，加 --overwrite-handwritten 重跑。")
    print(f"根目录：{V2}")

if __name__ == "__main__":
    main()
