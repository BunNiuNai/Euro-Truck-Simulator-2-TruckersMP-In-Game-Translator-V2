// Translator V2 — Native 进程入口
//
// 本文件只负责「传输与生命周期」：解析命令行、建管道、泵消息、收发帧。
// 所有消息处理在 src/dispatch.cpp，那里可以脱离管道单独自测。
//
// 已实现：管道、JSON、窗口、输入模拟、全局热键、系统托盘、WebView2 宿主。
// 逐个加入，每加一个都能单独验证。
//
// 注意：本进程**不得**向游戏进程注入任何代码（ADR-014）。
//
// 单线程 + 消息泵：
//   主循环用 MsgWaitForMultipleObjects 同时等「管道数据」与「窗口消息」。
//   若改成阻塞在 ReadFile 上，窗口会假死——拖拽和缩放全靠 WM_NCHITTEST
//   在窗口线程被处理，消息不泵就完全不响应，这是最容易踩的一个坑。
//
// 用法：
//   translator_native.exe                    启动管道服务（默认管道名）
//   translator_native.exe --pipe <name>      指定管道名（测试用，避免并行冲突）
//   translator_native.exe --version          打印协议版本
#include "../include/translator_native.h"

#include "dispatch.hpp"
#include "json.hpp"
#include "pipe.hpp"
// 单实例互斥体在 window 单元里（V1 也把它放在窗口相关的模块中），
// 这里显式包含，不靠 dispatch.hpp 的传递包含。
#include "window.hpp"

#include <windows.h>

#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

// 空闲等待上限：即使管道句柄不按预期置位，也能在 10ms 内被动探到数据。
// 100 次/秒的空转只是几次 PeekNamedPipe，开销可忽略。
constexpr DWORD kIdleWaitMs = 10;

// 退出时等 WebView2 子进程回收的上限（毫秒）。
//
// 取 5 秒的依据：实测本机收尾在 1 秒以内就能观察到浏览器进程退出（探针在
// 退出后 10 秒才去数，那时早就干净了）；留 5 秒是给「页面正在收尾、进程忙」
// 的余量。它同时是**硬上限**——到点就如实记日志继续退出，绝不把退出卡死，
// 也绝不改成固定 Sleep 硬等一段（那样既慢又说明不了任何事）。
constexpr unsigned kWebViewExitWaitMs = 5000;

using tn::Json;

// 泵一次线程消息，返回被触发的热键 id。
//
// ⚠️ 关键点：**不依赖窗口是否存在**。
// WM_HOTKEY 是投递到**线程**消息队列的（热键用 hWnd=nullptr 注册），
// 而热键常常在窗口建好之前就已设置（Go 可能先下发热键再建窗）。
// 如果只在 session.window.valid() 时才泵消息，那段时间的热键会静默失效。
std::vector<std::string> pumpMessages(tn::Session& session) {
    std::vector<std::string> fired;

    // 推进前端发起的拖动/缩放。放在最前面：本轮的位移要先应用，
    // 后面按新尺寸同步 WebView 才是对的。
    //
    // 每一轮都会走到这里，所以位移是跟着主循环逐帧推进的——主线程始终
    // 在跑，管道与心跳都不受影响（这正是不能用系统模态循环的原因，
    // 见 window.hpp 的 beginMoveResize 说明）。
    session.window.tickMoveResize();

    MSG msg;
    while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
        if (msg.message == WM_HOTKEY) {
            const std::string id = session.hotkeys.onMessage(msg.message, msg.wParam);
            if (!id.empty()) fired.push_back(id);
            // WM_HOTKEY 已由热键管理器消费，不再 Dispatch
            continue;
        }
        TranslateMessage(&msg);
        DispatchMessageW(&msg);
    }

    // 窗口被销毁（比如用户点了系统菜单的关闭）
    if (!session.window.valid() && session.window.wasCreated()) {
        std::printf("[native] 窗口已销毁\n");
        std::fflush(stdout);
    }

    // 用户点了设置窗口的 × → 释放它的 WebView。
    //
    // 判据用 childReady()：它只在「设置窗口的 WebView 确实存在」时为真，
    // 所以这段只会执行一次。若只看 wasCreated()，那个标志一旦置上就永远为真，
    // 每轮循环都会重复执行。
    //
    // 不释放的后果：再次 window.open 会复用一个已经失效的 WebView，
    // 表现为「右键设置没反应」。
    if (!session.settings.valid() && session.webview.childReady()) {
        session.webview.closeChild();
        std::printf("[native] 设置窗口已关闭，其 WebView 已释放\n");
        std::fflush(stdout);
    }

    // WebView **不会自己跟着窗口变**：窗口被拖动缩放后必须显式同步 bounds，
    // 否则页面还停在旧尺寸上（表现为右侧/下方露白）。
    // WebViewHost::setBounds 内部会判断尺寸是否真的变了，所以这里无脑调用即可。
    if (session.webview.ready() && session.window.valid()) {
        int x = 0, y = 0, w = 0, h = 0;
        if (session.window.getBounds(x, y, w, h)) {
            session.webview.setBounds(w, h);
        }
    }

    return fired;
}

// 主动推送一个「载荷里只带一个 id」的事件。
//
// 热键事件与托盘命令事件的形状完全一样，只差 type，所以合成一个函数——
// 否则两处的日志文案会各自漂移，排查时对不上。
//
// 这类消息不是对某条请求的回复，所以带 type 不带 id，
// Go 侧按事件处理（internal/ipc 的 readLoop 会把无 id 的消息放进事件通道）。
bool sendIdEvent(tn::PipeServer& server, const std::string& type, const std::string& id,
                 const std::string& what) {
    Json p = Json::makeObject();
    p.set("id", Json(id));

    Json m = Json::makeObject();
    m.set("type", Json(type));
    m.set("payload", std::move(p));

    std::string err;
    if (!server.writeFrame(m.dump(), err)) {
        std::printf("[native] 推送%s失败: %s\n", what.c_str(), err.c_str());
        std::fflush(stdout);
        return false;
    }
    std::printf("[native] %s: %s\n", what.c_str(), id.c_str());
    std::fflush(stdout);
    return true;
}

// 上报某个窗口被用户改过之后的几何。
//
// 载荷是四个数而不是单个 id，所以没法复用 sendIdEvent。
// type 用参数传：悬浮窗与设置窗口是两套独立记住的几何。
bool sendGeometryEvent(tn::PipeServer& server, const std::string& type, const char* what,
                       int x, int y, int w, int h) {
    Json p = Json::makeObject();
    p.set("x", Json(static_cast<double>(x)));
    p.set("y", Json(static_cast<double>(y)));
    p.set("width", Json(static_cast<double>(w)));
    p.set("height", Json(static_cast<double>(h)));

    Json m = Json::makeObject();
    m.set("type", Json(type));
    m.set("payload", std::move(p));

    std::string err;
    if (!server.writeFrame(m.dump(), err)) {
        std::printf("[native] 推送%s失败: %s\n", what, err.c_str());
        std::fflush(stdout);
        return false;
    }
    std::printf("[native] %s: %d,%d %dx%d\n", what, x, y, w, h);
    std::fflush(stdout);
    return true;
}

// 推送一条「悬浮窗实际状态」事件（event.stateChanged）。
//
// 用户点关闭（＝隐藏悬浮窗，见 window.cpp 的 WM_CLOSE）之后用它回报 Go。
// 为什么必须回报：Go 那边托盘「显示/隐藏」的取反、以及新消息到达时「要不要把
// 窗口叫回来」，读的都是**实际可见性**（cmd/translator/main.go:634-637、
// 977-983）。不报的话 Go 永远以为窗口还开着——托盘那一项点下去会继续隐藏
// （正好反了），新消息也不会再把窗口叫回来，悬浮窗看起来就是「再也回不来」。
// 复用已有的事件类型而不是新造一个，理由见 dispatch.hpp 的 displayStateEvent。
bool sendDisplayStateEvent(tn::PipeServer& server, const tn::Session& session,
                           const char* what) {
    std::string err;
    if (!server.writeFrame(tn::displayStateEvent(session), err)) {
        std::printf("[native] 推送%s失败: %s\n", what, err.c_str());
        std::fflush(stdout);
        return false;
    }
    std::printf("[native] %s\n", what);
    std::fflush(stdout);
    return true;
}

std::wstring pipeNameFromArgs(int argc, char** argv) {
    for (int i = 1; i + 1 < argc; ++i) {
        if (std::strcmp(argv[i], "--pipe") == 0) {
            const std::string name = argv[i + 1];
            const std::wstring wide(name.begin(), name.end());
            if (name.rfind("\\\\.\\pipe\\", 0) == 0) return wide;
            return std::wstring(L"\\\\.\\pipe\\") + wide;
        }
    }
    return std::wstring(TN_PIPE_NAME);
}

// 单实例互斥体名。
//
// ⚠️ 不带 `Global\` 前缀（V1 `main.py:29` 用的是 Global\\…）：native 是每个
//    会话里被 Go 拉起的子进程，会话内唯一就够了；而 Global\ 需要额外的
//    权限，在受限账户下会**创建失败**——那时 check 会把「建不出来」当成
//    「没有别的实例」，保护静默失效。测试里也是同样的理由（见 test_window）。
constexpr wchar_t kSingleInstanceName[] = L"ETS2TranslatorV2NativeSingleInstance";

// 把宽字符串转成 UTF-8。
//
// ⚠️ 为什么不用 std::wprintf：C 标准规定每个流有唯一的**方向（orientation）**，
//    第一次对 stdout 用 printf 之后就固定为字节导向，此后再调 wprintf 会
//    **静默失败**（返回 -1，什么都不输出，也不报错）。
//    表现是「日志里某些行凭空消失」，而且混用时偶尔还会输出半截，极难排查——
//    本项目就踩过：banner 的「管道: xxx」整行不见了。
//    统一走 printf + UTF-8 就没有这个问题。
std::string wideToUtf8(const std::wstring& w) {
    if (w.empty()) return std::string();
    const int need = WideCharToMultiByte(CP_UTF8, 0, w.c_str(),
                                         static_cast<int>(w.size()),
                                         nullptr, 0, nullptr, nullptr);
    if (need <= 0) return std::string();
    std::string out(static_cast<size_t>(need), '\0');
    WideCharToMultiByte(CP_UTF8, 0, w.c_str(), static_cast<int>(w.size()),
                        out.data(), need, nullptr, nullptr);
    return out;
}

void printBanner(const std::wstring& pipeName) {
    const std::vector<std::string> caps = tn::capabilities();

    std::string have;
    for (const std::string& c : caps) {
        if (!have.empty()) have += ", ";
        have += c;
    }

    // 尚未实现的能力按**已知全集**减去已实现的算出来。
    // 早期这里写死了一句「热键/托盘/输入/WebView 尚未实现」，实现热键之后
    // 那句话就变成了误导——排障的人会以为热键没做。
    const char* kKnown[] = {"pipe", "json", "window", "input", "hotkey", "tray", "webview"};
    std::string missing;
    for (const char* name : kKnown) {
        bool found = false;
        for (const std::string& c : caps) {
            if (c == name) { found = true; break; }
        }
        if (!found) {
            if (!missing.empty()) missing += ", ";
            missing += name;
        }
    }

    std::printf("[native] translator_native protocol=%d\n", TN_PROTOCOL_VERSION);
    std::printf("[native] 管道: %s\n", wideToUtf8(pipeName).c_str());
    if (missing.empty()) {
        std::printf("[native] 能力: %s（全部已实现）\n", have.c_str());
    } else {
        std::printf("[native] 能力: %s（尚未实现: %s）\n", have.c_str(), missing.c_str());
    }
    std::fflush(stdout);
}

}  // namespace

int main(int argc, char** argv) {
    for (int i = 1; i < argc; ++i) {
        if (std::strcmp(argv[i], "--version") == 0) {
            std::printf("translator_native protocol=%d\n", TN_PROTOCOL_VERSION);
            return 0;
        }
        if (std::strcmp(argv[i], "--help") == 0) {
            std::printf(
                "translator_native — Translator V2 Windows capability layer\n"
                "  --pipe <name>   使用指定管道名\n"
                "  --version       打印协议版本后退出\n"
                "  --help          本帮助\n");
            return 0;
        }
    }

    const std::wstring pipeName = pipeNameFromArgs(argc, argv);

    // ── 单实例保护（V1 `main.py:29-40` 的 `_ensure_single_instance`）──
    //
    // 为什么必须接在这里：native 由 Go 拉起，而 Go 崩溃或被杀时**不会**带走
    // native（它只等管道，自己不会退）。下一次启动就会出现两个 native 抢同一个
    // 管道名、同一批 Win32 资源（热键注册、托盘图标、剪贴板），
    // 表现是热键时灵时不灵、托盘上出现两个图标。
    // window.cpp 里的 SingleInstance 类一直有单测、却**没有任何调用者**，
    // 所以这层保护此前等于不存在。
    //
    // ⚠️ 放进 --version/--help 之后：那两个是查询，不启动服务，不应该被
    //    「已经有一个在跑」挡住（否则排障时连版本都问不出来）。
    tn::SingleInstance instance;
    if (!instance.acquire(kSingleInstanceName)) {
        // 必须 printf + UTF-8（见上面 wideToUtf8 的说明）：本进程已经用过
        // printf，再混 wprintf 会让这一行**静默消失**，而它恰恰是用户唯一
        // 能看到的解释。退出码与成功区分开，Go 侧才能把「已经有一个在跑」
        // 与「native 起不来」分开处理。
        std::printf("[native] 已有一个 translator_native 实例在运行，本次启动直接退出"
                    "（退出码 %d）\n", TN_EXIT_ALREADY_RUNNING);
        std::fflush(stdout);
        return TN_EXIT_ALREADY_RUNNING;
    }

    printBanner(pipeName);

    tn::PipeServer server;
    if (!server.create(pipeName)) {
        std::printf("[native] 创建管道失败，错误码 %lu\n", GetLastError());
        return TN_EXIT_STARTUP_FAILED;
    }

    tn::Session session;
    bool stop = false;

    while (!stop) {
        // 等待 Go 连接。返回 false 通常是「上一客户端刚断开」，重试即可。
        if (!server.waitForClient()) {
            const DWORD e = GetLastError();
            if (e != ERROR_PIPE_CONNECTED) {
                std::printf("[native] 等待连接失败，错误码 %lu\n", e);
                std::fflush(stdout);
                Sleep(200);
            }
            continue;
        }
        std::printf("[native] Go 已连接\n");
        std::fflush(stdout);

        bool connected = true;
        while (!stop && connected) {
            // 同时等「管道数据」与「窗口消息」。
            HANDLE h = reinterpret_cast<HANDLE>(server.rawHandle());
            DWORD wait = WAIT_TIMEOUT;
            if (h) {
                wait = MsgWaitForMultipleObjects(1, &h, FALSE, kIdleWaitMs, QS_ALLINPUT);
            } else {
                Sleep(kIdleWaitMs);
            }

            // ── 消息泵 ──
            //
            // 必须每轮都泵，且**不依赖窗口是否存在**：
            //   · 窗口消息（拖拽/缩放/重绘）靠它
            //   · WM_HOTKEY 也在这条线程队列里——热键往往在窗口建好之前
            //     就已经注册（Go 可能先设热键再建窗），若只在窗口有效时泵
            //     消息，热键会静默失效。
            std::vector<std::string> fired = pumpMessages(session);
            for (const std::string& hid : fired) {
                if (!sendIdEvent(server, TN_N2C_HOTKEY, hid, "热键触发")) {
                    connected = false;
                }
            }
            if (!connected) continue;

            // 托盘命令：托盘菜单跑在**独立线程**上（TrackPopupMenu 是模态的，
            // 放主线程会把管道读取一起堵死），所以它只把命令入队，
            // 由主线程在这里取走再上报——管道写入不是线程安全的。
            for (const std::string& cmd : session.tray.takeCommands()) {
                if (!sendIdEvent(server, TN_N2C_TRAY_COMMAND, cmd, "托盘命令")) {
                    connected = false;
                }
            }
            if (!connected) continue;

            // 用户拖动/缩放**结束**了 → 把新几何报给 Go。
            //
            // 几何是用户改的，Go 的期望状态并不知道；不报的话，重连重放会
            // 把窗口弹回旧位置（V1 用 `_schedule_save_position` 解决同一问题）。
            {
                int gx = 0, gy = 0, gw = 0, gh = 0;
                if (session.window.takeUserGeometryChange(gx, gy, gw, gh)) {
                    if (!sendGeometryEvent(server, TN_N2C_WINDOW_CHANGED, "悬浮窗几何",
                                           gx, gy, gw, gh)) {
                        connected = false;
                    }
                }
            }
            if (!connected) continue;

            // 用户把悬浮窗关掉（＝隐藏，见 window.cpp 的 WM_CLOSE）。
            //
            // 这是**用户**做出的事实，Go 的期望状态不知道：不报的话，托盘
            // 「显示/隐藏」会把「可见」取反成隐藏（正好反了），而新消息到达时
            // Go 也不会再调 SetVisible(true) 把窗口叫回来——悬浮窗看起来就是
            // 彻底没了。回报用的是 Go 已经在处理的 event.stateChanged。
            if (session.window.takeUserHide()) {
                if (!sendDisplayStateEvent(server, session, "悬浮窗已被用户隐藏，已回报实际状态")) {
                    connected = false;
                }
            }
            if (!connected) continue;

            // 设置窗口同理：它的位置尺寸是用户用系统标题栏改的，
            // 由 WM_SIZE/WM_MOVE 打标记，这里取走去上报。
            {
                int gx = 0, gy = 0, gw = 0, gh = 0;
                if (session.settings.takeUserGeometryChange(gx, gy, gw, gh)) {
                    if (!sendGeometryEvent(server, TN_N2C_SETTINGS_GEOMETRY, "设置窗口几何",
                                           gx, gy, gw, gh)) {
                        connected = false;
                    }
                }
            }
            if (!connected) continue;

            // ── 轮询降级路径 ──
            //
            // 内部按 50ms 节流，所以这里每轮调用（10ms 一次）不会打满 CPU；
            // 全部热键都走 RegisterHotKey 时它是空操作。
            for (const std::string& hid : session.hotkeys.poll()) {
                if (!sendIdEvent(server, TN_N2C_HOTKEY, hid, "热键触发")) {
                    connected = false;
                }
            }
            if (!connected) continue;

            // 不是被管道唤醒时（超时 / 窗口消息）主动探一次，
            // 不假设管道句柄一定会按预期置位。
            if (wait != WAIT_OBJECT_0) {
                const tn::PipeStatus st = server.poll();
                if (st == tn::PipeStatus::Broken) {
                    std::printf("[native] 对端已断开\n");
                    connected = false;
                    continue;
                }
                if (st == tn::PipeStatus::NoData) continue;
            }

            std::string raw;
            std::string err;
            if (!server.readFrame(raw, err)) {
                std::printf("[native] 读取结束: %s\n", err.c_str());
                std::fflush(stdout);
                connected = false;
                continue;
            }

            const std::string reply = tn::handleMessage(raw, session, stop);
            if (!reply.empty()) {
                std::string werr;
                if (!server.writeFrame(reply, werr)) {
                    std::printf("[native] 回复失败: %s\n", werr.c_str());
                    std::fflush(stdout);
                    connected = false;
                    continue;
                }
            }
            std::fflush(stdout);
        }

        // 断开即清空实际状态（ADR-012）：窗口内容全部来自 Go，
        // 留一个空玻璃框比没有窗口更糟；Go 重连时会用 native.replay 重建。
        //
        // ⚠️ 顺序是「先拆 WebView，再销毁窗口」，两个窗口都一样。
        //    WebView2 的 controller 绑在创建它时那个 HWND 上：窗口先没、controller
        //    还留着的话，下次 attach() 会走「已经建好」的早退分支——只重新 Navigate
        //    一次，而 controller 指向的是**已经销毁的窗口**，回包却照报 ready:true。
        //    表现就是「窗口在、页面没了」，上层完全看不出异常（dispatch.cpp 的
        //    applyDisplay 里已经为同一条路写过一次警告，这里是第二次踩它）。
        //
        // ⚠️ 设置窗口也必须一起收掉，不能只收悬浮窗。实测（native/tools/leak-probe.ps1
        //    -Mode reconnect -TwoWindows）：只销毁悬浮窗时，设置窗口的 HWND 还在、
        //    它的 controller 也没人关，于是**页面继续活着**——页面里那个
        //    EventSource 会一直重连，Go 重启后新后端立刻被这个看不见的旧页面订阅上，
        //    真正的前端反倒成了「第二个订阅者」（实测断开后仍有 1 个订阅者不退）。
        //
        // WebViewHost::close() 内部先关设置窗口的 controller、再关悬浮窗的，
        // 两个窗口的 WebView 一起拆（见 webview.cpp 的 closeChild/close）。
        session.webview.close();
        if (session.window.valid()) session.window.destroy();
        if (session.settings.valid()) session.settings.destroy();
        // 热键同理：连接没了就没人处理热键事件了，留着会让按键被白白吞掉
        session.hotkeys.release();

        server.disconnect();
        if (!stop) {
            std::printf("[native] 连接已断开，等待重连\n");
            std::fflush(stdout);
        }
    }

    server.close();

    // ── 退出前显式收掉 WebView2 ──
    //
    // 为什么不能只靠「进程退出，系统自然会回收」：WebView2 的浏览器进程
    // （msedgewebview2.exe 及它下面的 renderer/gpu/network/storage）是**独立
    // 进程**。进程一 ExitProcess，我们持有的 Controller/WebView/Environment
    // 就随进程一起消失，回收与否全看对方何时发现宿主没了——实测本机这条 OS
    // 路径也能收回（强杀后 <1s 归零），但那是**时机**而不是保证，而且事后无从
    // 判断：留下的进程只会让人对着进程表猜（这正是本次排查最初被误导的地方）。
    //
    // 所以这里显式关、显式等（事件 + 超时，绝不是固定 Sleep）：关掉两个窗口的
    // controller → 释放进程级共享 Environment → 等浏览器进程真的退出。
    // 超时不阻塞退出，但会写进日志。顺序与理由见 webview.hpp 的声明处。
    const bool webviewExited = session.webview.shutdownForExit(kWebViewExitWaitMs);
    std::printf("[native] WebView2 收尾：%s\n",
                webviewExited ? "子进程已回收" : "等待超时（见上面 [webview] 那行）");
    std::fflush(stdout);

    std::printf("[native] 已退出\n");
    return TN_EXIT_OK;
}
