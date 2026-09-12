// 消息分发实现。协议定义见 include/translator_native.h。
#include "dispatch.hpp"

#include "../include/translator_native.h"
#include "hotkey.hpp"
#include "input.hpp"
#include "json.hpp"
#include "webview.hpp"

#include <windows.h>

#include <cstdio>
#include <string>

namespace tn {

namespace {

using tn::Json;

// ── 信封与错误 ────────────────────────────────────────────────

Json envelope(const std::string& type, double id, Json payload) {
    Json m = Json::makeObject();
    m.set("type", Json(type));
    if (id != 0) m.set("id", Json(id));
    m.set("payload", std::move(payload));
    return m;
}

// 无请求 id 的错误（帧本身就不合法，配不上对）。
Json errorEvent(const std::string& code, const std::string& message) {
    Json p = Json::makeObject();
    p.set("code", Json(code));
    p.set("message", Json(message));
    return envelope(TN_N2C_ERROR, 0, std::move(p));
}

Json errorReply(const std::string& code, const std::string& message, double id) {
    Json p = Json::makeObject();
    p.set("ok", Json(false));
    p.set("code", Json(code));
    p.set("message", Json(message));
    return envelope(TN_N2C_ERROR, id, std::move(p));
}

// ── 字段读取：缺失或类型不符时回落到默认值 ─────────────────────
// 不用 asInt 读 bool 字段：类型不符时 asInt 会静默返回默认值，
// 那样 "topmost": true 会被当成「没给」，错误会变得很难查。

int intField(const Json& o, const char* key, int def) {
    const Json* v = o.find(key);
    return (v && v->isNumber()) ? v->asInt(def) : def;
}

double numField(const Json& o, const char* key, double def) {
    const Json* v = o.find(key);
    return (v && v->isNumber()) ? v->asNumber(def) : def;
}

bool boolField(const Json& o, const char* key, bool def) {
    const Json* v = o.find(key);
    return (v && v->type == JsonType::Bool) ? v->asBool(def) : def;
}

std::string strField(const Json& o, const char* key, const std::string& def) {
    const Json* v = o.find(key);
    return (v && v->isString()) ? v->asString() : def;
}

std::wstring utf8ToWide(const std::string& s) {
    if (s.empty()) return std::wstring();
    const int need = MultiByteToWideChar(CP_UTF8, 0, s.c_str(),
                                         static_cast<int>(s.size()), nullptr, 0);
    if (need <= 0) return std::wstring();
    std::wstring out(static_cast<size_t>(need), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, s.c_str(), static_cast<int>(s.size()),
                        out.data(), need);
    return out;
}

// ── 实际状态（ADR-012：期望状态由 Go 持有，这里只回报真实情况）──

// 解析热键列表。hotkey.set 与 native.replay 用的是同一形状，共用一份解析——
// 两处各写一遍迟早会漂移。
std::vector<HotkeySpec> parseHotkeyList(const Json& list) {
    std::vector<HotkeySpec> specs;
    if (!list.isArray()) return specs;

    for (const Json& item : list.array) {
        if (!item.isObject()) continue;
        HotkeySpec s;
        s.id = strField(item, "id", "");
        s.vk = static_cast<unsigned int>(intField(item, "vk", 0));
        s.mods = static_cast<unsigned int>(intField(item, "mods", 0));
        s.enabled = boolField(item, "enabled", true);
        if (!s.id.empty()) specs.push_back(s);
    }
    return specs;
}

// 前端传来的区域名 → HitZone。
//
// 前端只说语义（"bottomright"），不碰 HTBOTTOMRIGHT 这类 Win32 常量——
// 让界面层知道 Windows 的命中码是把平台细节漏到了错误的层。
HitZone zoneFromName(const std::string& name) {
    if (name == "caption") return HitZone::Caption;
    if (name == "left") return HitZone::Left;
    if (name == "right") return HitZone::Right;
    if (name == "top") return HitZone::Top;
    if (name == "bottom") return HitZone::Bottom;
    if (name == "topleft") return HitZone::TopLeft;
    if (name == "topright") return HitZone::TopRight;
    if (name == "bottomleft") return HitZone::BottomLeft;
    if (name == "bottomright") return HitZone::BottomRight;
    return HitZone::None;
}

// 托盘的实际状态。
//
// 托盘是**异步**创建的（图标必须在托盘线程里挂），所以 apply 返回成功只
// 表示「已发起」。真实情况看这里：active 表示图标确实挂上了，
// error 非空表示挂失败了（比如没有桌面会话）。
Json trayState(const TrayIcon& t) {
    Json o = Json::makeObject();
    o.set("active", Json(t.active()));
    const std::string err = t.lastError();
    if (!err.empty()) o.set("error", Json(err));
    return o;
}

// 解析 Go 下发的托盘规格。
//
// 缺 id 或 label 的项直接丢掉：没有 id 就无法把点击映射回命令，
// 留在菜单里只会变成一个点了没反应的死项。
void parseTraySpec(const Json& j, TraySpec& out) {
    out.enabled = boolField(j, "enabled", false);
    out.tip = strField(j, "tip", "");
    out.menu.clear();

    const Json* menu = j.find("menu");
    if (menu == nullptr || !menu->isArray()) return;

    for (const Json& item : menu->array) {
        TrayMenuItem mi;
        if (boolField(item, "separator", false)) {
            mi.separator = true;
            out.menu.push_back(mi);
            continue;
        }
        mi.id = strField(item, "id", "");
        mi.label = strField(item, "label", "");
        if (mi.id.empty() || mi.label.empty()) continue;
        mi.checked = boolField(item, "checked", false);
        mi.isDefault = boolField(item, "default", false);
        out.menu.push_back(mi);
    }
}

Json windowState(const OverlayWindow& w) {
    Json o = Json::makeObject();
    o.set("exists", Json(w.valid()));
    if (!w.valid()) return o;

    int x = 0, y = 0, width = 0, height = 0;
    if (w.getBounds(x, y, width, height)) {
        o.set("x", Json(static_cast<double>(x)));
        o.set("y", Json(static_cast<double>(y)));
        o.set("width", Json(static_cast<double>(width)));
        o.set("height", Json(static_cast<double>(height)));
    }
    o.set("visible", Json(w.isVisible()));
    o.set("clickThrough", Json(w.isClickThrough()));
    o.set("blur", Json(w.appliedBlur()));
    o.set("dark", Json(w.isDark()));
    return o;
}

Json actualState(const Session& s) {
    Json o = Json::makeObject();
    o.set("connected", Json(true));
    o.set("display", windowState(s.window));
    o.set("tray", trayState(s.tray));
    return o;
}

Json webviewState(const WebViewHost& wv) {    Json o = Json::makeObject();
    // compiled 是**编译期**事实：本次构建有没有把显示层编进来。
    // 与 ready 分开，才能区分「没编」「在编」「编好了」。
    o.set("compiled", Json(WebViewHost::compiled()));
    o.set("ready", Json(wv.ready()));
    o.set("attaching", Json(wv.attaching()));
    // 设置窗口的 WebView 是否已就绪。与悬浮窗分开报：它们是两个独立的
    // Controller，一个就绪不代表另一个也好了。
    o.set("settingsReady", Json(wv.childReady()));
    const std::string& e = wv.lastError();
    if (!e.empty()) o.set("error", Json(e));
    return o;
}

// window.open 的承载窗口：设置窗口。
//
// raiseAboveTopmost 把窗口提到**所有置顶窗口之上**并激活它。
//
// 两件事都必须做，少一件都不生效：
//
//  ① `HWND_TOPMOST`：游戏窗口本身是 `WS_EX_TOPMOST` 的，只抢前台的话设置窗口
//     仍然被压在游戏下面。
//  ② **`AttachThreadInput` 借前台权限**：Windows 的前台锁只允许「当前前台窗口
//     所属的进程」或「刚收到用户输入的进程」抢前台。我们是后台进程，裸调
//     `SetForegroundWindow` 会被**静默忽略**（返回 FALSE、没有异常）。
//     与 window.cpp:460-475 里 `focus()` 的做法完全一致——那一段就是为同一个
//     问题写的，我第一版这里却自己写了个裸调用，结果就是"抬了但没到前面"。
//
// 失败时不抛错：这些都是尽力而为的视觉行为，失败最多是窗口没浮上来，
// 不该让 settings.open 整个报错。
void raiseAboveTopmost(HWND h) {
    if (h == nullptr) {
        return;
    }

    if (!IsWindowVisible(h)) {
        ShowWindow(h, SW_SHOWNOACTIVATE);
    }
    SetWindowPos(h, HWND_TOPMOST, 0, 0, 0, 0,
                 SWP_NOMOVE | SWP_NOSIZE | SWP_SHOWWINDOW);
    BringWindowToTop(h);

    const HWND fg = GetForegroundWindow();
    const DWORD fgThread = (fg != nullptr) ? GetWindowThreadProcessId(fg, nullptr) : 0;
    const DWORD ownThread = GetCurrentThreadId();

    bool attached = false;
    if (fgThread != 0 && fgThread != ownThread) {
        attached = AttachThreadInput(ownThread, fgThread, TRUE) != FALSE;
    }
    SetForegroundWindow(h);
    SetActiveWindow(h);
    SetFocus(h);
    if (attached) {
        AttachThreadInput(ownThread, fgThread, FALSE);
    }
}

// provideSettingsWindow 把设置窗口建出来并**带到最前面**。
//
// 本函数只负责「把窗口建出来并显示、把它提到前面」；往那个 HWND 里放
// WebView 是 WebViewHost 的事（它才知道怎么共享 Environment）。
//
// 返回 nullptr 表示拒绝——调用方会取消这次 window.open。宁可不弹，
// 也不能弹一个不受我们控制的浏览器窗口。
void* provideSettingsWindow(Session& s, const std::wstring& uri) {
    // URL 不在这里用：WebView2 会把回填的 WebView 自己导航到请求的 URI
    // （文档明确说 NewWindow 里给的 WebView 「cannot be navigated」）。
    (void)uri;

    if (s.settings.valid()) {
        // 已经开着：提到所有置顶窗口之上，而不是再建第二个窗口
        raiseAboveTopmost(static_cast<HWND>(s.settings.hwnd()));
        std::printf("[native] 设置窗口已存在，前置\n");
        std::fflush(stdout);
        return s.settings.hwnd();
    }

    WindowSpec spec;
    spec.framed = true;      // 普通带边框窗口（V1 用的是 tk.Toplevel）
    spec.topmost = false;
    spec.opacity = 1.0;
    spec.dark = s.window.isDark();   // 跟随悬浮窗的材质深浅，避免深色主题配白标题栏
    spec.title = L"ETS2 Translator — 设置";
    // 位置与尺寸用上次记住的（Go 从 config.settings_win_* 下发）。
    // x/y 为 -1 时 create() 会居中——首次打开就是这个情况。
    spec.x = s.settingsX;
    spec.y = s.settingsY;
    spec.width = s.settingsWidth;
    spec.height = s.settingsHeight;

    if (!s.settings.create(spec)) {
        std::printf("[native] 创建设置窗口失败，错误码 %lu\n", GetLastError());
        std::fflush(stdout);
        return nullptr;
    }

    // ⚠️ 把悬浮窗设为设置窗口的 **owner** —— 这是 V1 的做法，也是唯一稳的。
    //
    // V1 `main.py:353-361`：
    //     self.top = tk.Toplevel(parent)   # parent = 悬浮窗
    //     self.top.transient(parent)       # 转成悬浮窗的 transient
    //     self.top.grab_set()              # 模态
    //
    // `transient` 在 Tk 里就是 Win32 的**从属窗口（owned window）**，而
    // **从属窗口永远显示在它的 owner 之上**。悬浮窗是 TOPMOST 的，
    // 于是设置窗口天然就压在游戏上面——不需要抢前台、不需要 AttachThreadInput、
    // 也不需要反复置顶。
    //
    // 我前四轮都在"抬升"上做文章（补路径、加撤销、借前台权限、复用分支），
    // 本质上都是在跟 z 序打架；而 V1 根本没有打架——它让设置窗口**从属于**
    // 悬浮窗，层级关系由系统自己维持，稳定得多。
    //
    // GWLP_HWNDPARENT 对**非子窗口**就是设 owner（对子窗口才是设 parent），
    // 所以这里设的是从属关系而不是把它变成子控件。
    if (s.window.valid()) {
        SetWindowLongPtrW(static_cast<HWND>(s.settings.hwnd()),
                          GWLP_HWNDPARENT,
                          reinterpret_cast<LONG_PTR>(static_cast<HWND>(s.window.hwnd())));
        std::printf("[native] 设置窗口已从属于悬浮窗（owner 关系，随悬浮窗层级）\n");
        std::fflush(stdout);
    }

    // 设置窗口尺寸一变就同步它的 WebView。参数先按客户区实算，
    // WebViewHost 建好 controller 之后会自己再摆一次。
    s.settings.setResizeHandler([&s](int w, int h) { s.webview.setChildBounds(w, h); });

    s.settings.show();
    std::printf("[native] 设置窗口已创建 %dx%d\n", spec.width, spec.height);
    std::fflush(stdout);
    return s.settings.hwnd();
}

// 应用 display.create / replay 的 display 段。
//
// 期望状态可能变过（重放、改配置），所以先销毁再按新规格重建——
// create() 在窗口已存在时会早退，不重建的话新规格会静默不生效。
bool applyDisplay(const Json& spec, Session& s, std::string& err) {
    if (s.window.valid()) {
        // ⚠️ 必须**先**把 WebView 拆掉，再销毁窗口。
        //
        // WebView2 的 controller 是绑在创建它时那个 HWND 上的。窗口被销毁后
        // 它还留着的话，下面 attach() 会走「已经建好」的早退分支——只重新
        // Navigate 一次，controller 依旧指向那个**已经销毁的窗口**，而回包
        // 照报 ready:true。表现就是「窗口在、页面没了」，而且上层完全看不出
        // 异常。每次断线重连、每次 display.create 都会命中这条路。
        s.webview.close();
        s.window.destroy();
    }

    WindowSpec ws;
    ws.x = intField(spec, "x", -1);
    ws.y = intField(spec, "y", -1);
    ws.width = intField(spec, "width", 620);
    ws.height = intField(spec, "height", 360);
    ws.topmost = boolField(spec, "topmost", true);
    ws.clickThrough = boolField(spec, "clickThrough", false);
    ws.opacity = numField(spec, "opacity", 0.8);
    ws.blur = strField(spec, "blur", "auto");

    // dark 决定 Mica/Acrylic 材质的深浅。默认 true 是刻意的：
    // 老版本的调用方不带这个字段，保持与之前一致的行为。
    ws.dark = boolField(spec, "dark", true);

    const std::string title = strField(spec, "title", "");
    if (!title.empty()) ws.title = utf8ToWide(title);

    if (!s.window.create(ws)) {
        err = "创建窗口失败，错误码 " + std::to_string(GetLastError());
        return false;
    }
    if (boolField(spec, "visible", true)) s.window.show();

    // 窗口尺寸一变就同步 WebView（含原生缩放循环里的逐帧变化）。
    //
    // 捕获的是 &s.webview 而不是 &s：Session 里 window 声明在 webview **之前**，
    // 成员按声明逆序析构，所以 webview 会先走——而 OverlayWindow::destroy()
    // 会把回调清空，两重保险。
    s.window.setResizeHandler([&s](int w, int h) { s.webview.setBounds(w, h); });

    // 设置窗口的初始位置与尺寸来自 Go 的期望状态（config.settings_win_*）。
    // x/y 为 -1 时 native 会自己居中（老配置没有这两个字段就是这种情况）。
    s.settingsX = intField(spec, "settingsX", -1);
    s.settingsY = intField(spec, "settingsY", -1);
    s.settingsWidth = intField(spec, "settingsWidth", 540);
    s.settingsHeight = intField(spec, "settingsHeight", 700);

    // window.open 的承载窗口提供者。不挂的话 NewWindowRequested 会被直接
    // 取消，网页里 window.open 拿到 null，前端只能弹错误提示——设置入口
    // 就彻底断了（这正是之前的状态）。
    s.webview.setChildWindowProvider([&s](const std::wstring& uri) -> void* {
        return provideSettingsWindow(s, uri);
    });

    // 页面地址由 Go 决定——它才知道自己监听哪个端口。
    //
    // 没给 url 时不建 WebView：那样窗口就是一块空玻璃，与 V1 的悬浮窗
    // 完全不像，但至少不会显示一个来路不明的页面。
    const std::string url = strField(spec, "url", "");
    if (url.empty()) return true;

    if (!WebViewHost::compiled()) {
        // 窗口建出来了，但显示层没编进来。不是失败（窗口本身可用），
        // 但要如实说清楚，别让人以为「显示正常只是没内容」。
        std::printf("[native] 未编译显示层，窗口内不会显示页面\n");
        std::fflush(stdout);
        return true;
    }

    std::string werr;
    if (!s.webview.attach(s.window.hwnd(), utf8ToWide(url), werr)) {
        // WebView 起不来不该把窗口也废掉：Mica 材质与拖拽缩放仍然可用，
        // 用户至少能拖动它、看到材质。所以这里只记录，不返回失败。
        std::printf("[native] WebView 创建失败: %s\n", werr.c_str());
        std::fflush(stdout);
    }
    return true;
}

}  // namespace

std::string displayStateEvent(const Session& session) {
    // 载荷形状与 native.hello / display.create 里的 actualState 保持一致：
    // 只带 display 一段，id 为 0（事件不是对某条请求的回复，Go 侧按事件处理）。
    Json p = Json::makeObject();
    p.set("display", windowState(session.window));
    return envelope(TN_N2C_STATE_CHANGED, 0, std::move(p)).dump();
}

std::vector<std::string> capabilities() {
    // 只列**已实现**的。夸大能力会让 Go 以为某个功能可用。
    std::vector<std::string> caps = {"pipe", "json", "window", "input", "hotkey"};

    // 托盘依赖桌面会话：没有会话时图标挂不上，此时不该报这个能力。
    if (TrayIcon::hasDesktopSession()) {
        caps.push_back("tray");
    }

    // 显示层是**编译期可选**的（没找到 WebView2 SDK 就不编）。
    // 所以这里必须按实际情况报，不能写死——否则 Go 会以为页面能显示，
    // 而窗口其实是一块空玻璃。
    if (WebViewHost::compiled()) {
        caps.push_back("webview");
    }
    return caps;
}

std::string handleMessage(const std::string& raw, Session& session, bool& stop) {
    Json msg;
    std::string err;
    if (!Json::parse(raw, msg, err)) {
        std::printf("[native] 收到非法 JSON，已忽略: %s\n", err.c_str());
        std::fflush(stdout);
        return errorEvent("BAD_FRAME", "不是合法 JSON: " + err).dump();
    }

    const Json* typeNode = msg.find("type");
    const std::string type = typeNode ? typeNode->asString() : std::string();
    const Json* idNode = msg.find("id");
    const double id = idNode ? idNode->asNumber() : 0.0;

    if (type.empty()) {
        return errorEvent("BAD_FRAME", "缺少 type 字段").dump();
    }

    // ── native.hello：握手，校验协议版本 ──
    if (type == TN_C2N_HELLO) {
        const Json* payload = msg.find("payload");
        const Json* verNode = payload ? payload->find("protocolVersion") : nullptr;
        const int peerVersion = verNode ? verNode->asInt() : 0;
        const bool match = (peerVersion == TN_PROTOCOL_VERSION);

        Json caps = Json::makeArray();
        for (const std::string& c : capabilities()) caps.push(Json(c));

        Json p = Json::makeObject();
        p.set("protocolVersion", Json(static_cast<double>(TN_PROTOCOL_VERSION)));
        p.set("capabilities", std::move(caps));
        p.set("versionMatch", Json(match));
        p.set("actualState", actualState(session));

        if (match) {
            std::printf("[native] 握手成功（协议版本 %d）\n", TN_PROTOCOL_VERSION);
        } else {
            std::printf("[native] 协议版本不一致: 对端 %d / 本机 %d\n",
                        peerVersion, TN_PROTOCOL_VERSION);
        }
        return envelope(TN_N2C_READY, id, std::move(p)).dump();
    }

    // ── native.replay：Go 重连后重放全部期望状态（ADR-012）──
    if (type == TN_C2N_REPLAY) {
        const Json* payload = msg.find("payload");
        const Json* registry = payload ? payload->find("registry") : nullptr;

        Json applied = Json::makeArray();
        Json skipped = Json::makeArray();

        const Json* display = registry ? registry->find("display") : nullptr;
        if (display && display->isObject()) {
            std::string derr;
            if (!applyDisplay(*display, session, derr)) {
                return errorReply("CREATE_FAILED", derr, id).dump();
            }
            applied.push(Json("display"));
        }

        // 热键：**已实现，要真的应用**。
        // （早期版本这里把它列进 skipped——那时它确实没实现。后来实现了热键
        //   却忘了改这一支，导致重连后热键不会被重放。端到端跑一次才发现。）
        const Json* hotkeys = registry ? registry->find("hotkeys") : nullptr;
        if (hotkeys && hotkeys->isArray()) {
            std::string herr;
            session.hotkeys.apply(parseHotkeyList(*hotkeys), herr);
            applied.push(Json("hotkeys"));
            std::printf("[native] 重放热键：%zu 个，其中 %zu 个降级\n",
                        session.hotkeys.size(), session.hotkeys.fallbackIDs().size());
        }

        // 托盘：**已实现，要真的应用**。
        //
        // ⚠️ 这里曾经把它列进 skipped —— 写这段代码时托盘确实还没实现，
        //    但后来实现了却忘了改这一支，重连后托盘就不会被恢复。
        //    热键当初踩过**一模一样**的坑（见上面那段的注释）。
        const Json* trayNode = registry ? registry->find("tray") : nullptr;
        if (trayNode && trayNode->isObject()) {
            TraySpec spec;
            parseTraySpec(*trayNode, spec);
            std::string terr;
            session.tray.apply(spec, terr);
            applied.push(Json("tray"));
        }

        std::printf("[native] 重放：应用 %zu 项，跳过 %zu 项\n",
                    applied.array.size(), skipped.array.size());

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("applied", std::move(applied));
        p.set("skipped", std::move(skipped));
        p.set("actualState", actualState(session));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── display.create：创建/重建悬浮窗 ──
    if (type == TN_C2N_DISPLAY_CREATE) {
        const Json* payload = msg.find("payload");
        if (!payload || !payload->isObject()) {
            return errorReply("BAD_FRAME", "display.create 缺少 payload 对象", id).dump();
        }
        std::string derr;
        if (!applyDisplay(*payload, session, derr)) {
            return errorReply("CREATE_FAILED", derr, id).dump();
        }
        std::printf("[native] 窗口已创建 %dx%d\n",
                    intField(*payload, "width", 620), intField(*payload, "height", 360));

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("display", windowState(session.window));
        p.set("webview", webviewState(session.webview));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── display.visible：显示/隐藏 ──
    // ── display.opacity：运行期改窗口级 alpha，不重建窗口 ──
    //
    // 补这条是因为 `display.create` 只在建窗时读一次 opacity，
    // 运行期改配置只能等下次启动——用户在设置里拖滑块看到的是「毫无反应」，
    // 而窗口其实一直是启动那一刻的值（实测 alpha 与配置项对不上）。
    if (type == TN_C2N_DISPLAY_OPACITY) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const double opacity = payload ? numField(*payload, "opacity", 1.0) : 1.0;
        const bool ok = session.window.setOpacity(opacity);

        Json p = Json::makeObject();
        p.set("ok", Json(ok));
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    if (type == TN_C2N_DISPLAY_VISIBLE) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const bool visible = payload ? boolField(*payload, "visible", true) : true;
        if (visible) {
            session.window.show();
            session.webview.show();
        } else {
            session.window.hide();
            session.webview.hide();
        }

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── display.setClickThrough：鼠标穿透 ──
    if (type == TN_C2N_CLICK_THROUGH) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const bool enabled = payload ? boolField(*payload, "enabled", false) : false;
        session.window.setClickThrough(enabled);

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── window.blur：切换毛玻璃 ──
    if (type == TN_C2N_WINDOW_BLUR) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const std::string mode = payload ? strField(*payload, "mode", "auto") : "auto";
        const bool ok = session.window.applyBlur(mode);

        Json p = Json::makeObject();
        p.set("ok", Json(ok));
        if (!ok) {
            p.set("code", Json(std::string("BLUR_FAILED")));
            p.set("message", Json("毛玻璃模式 " + mode + " 在本机不可用"));
        }
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── theme.set：切换窗口材质的深浅（深色/浅色主题）──
    if (type == TN_C2N_THEME_SET) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const bool dark = payload ? boolField(*payload, "dark", true) : true;
        session.window.setDarkMode(dark);

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── window.beginMoveResize：前端发起的拖动/缩放 ──
    //
    // 前端在最外圈（标题带或四边）监听到 mousedown 就发这条。这里只**记下起点
    // 立刻返回**，真正的位移由主循环每轮调 OverlayWindow::tickMoveResize 推进——
    // 因为主线程一旦被系统的模态移动循环占住，就再也读不到管道了。
    // 详见 window.hpp 里 beginMoveResize 的说明。
    //
    // ⚠️ 这是**瞬时动作**，不是期望状态：绝不能写进 desired，否则重连重放
    //    会把窗口莫名其妙地重新拖一次。
    if (type == TN_C2N_WINDOW_MOVE) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const std::string zone = payload ? strField(*payload, "zone", "") : std::string();
        const HitZone hit = zoneFromName(zone);
        if (hit == HitZone::None) {
            return errorReply("BAD_ZONE", "未知的拖动区域: " + zone, id).dump();
        }
        const bool ok = session.window.beginMoveResize(hit);
        std::printf("[native] window.beginMoveResize zone=%s -> %s\n",
                    zone.c_str(), ok ? "已开始跟踪" : "失败");
        std::fflush(stdout);

        Json p = Json::makeObject();
        p.set("ok", Json(ok));
        p.set("zone", Json(zone));
        p.set("display", windowState(session.window));  // 起点，不是终点
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── tray.set：托盘图标与菜单 ──
    //
    // 分界与原设计一致（矩阵 W7）：native 只把图标和菜单**显示**出来，
    // 点完之后做什么由 Go 决定——所以这里收到的是一份已经算好的规格
    // （含勾选状态），点中的项也只作为一个事件报回去。
    if (type == TN_C2N_TRAY_SET) {
        const Json* payload = msg.find("payload");
        if (!payload || !payload->isObject()) {
            return errorReply("BAD_FRAME", "tray.set 缺少 payload 对象", id).dump();
        }

        TraySpec spec;
        parseTraySpec(*payload, spec);

        std::string terr;
        session.tray.apply(spec, terr);

        std::printf("[native] 托盘: %s，菜单 %zu 项\n",
                    spec.enabled ? "启用" : "停用", spec.menu.size());
        std::fflush(stdout);

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("tray", trayState(session.tray));
        if (!terr.empty()) p.set("warning", Json(terr));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── settings.open：托盘菜单的「Settings 设置」 ──
    //
    // 这条路径上没有 window.open，所以拿不到 NewWindowRequested 事件，
    // 得自己把第二个窗口的 WebView 建起来并导航（见 WebViewHost::openChildWindow）。
    if (type == TN_C2N_SETTINGS_OPEN) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const Json* payload = msg.find("payload");
        const std::string url = payload ? strField(*payload, "url", "") : std::string();
        if (url.empty()) {
            return errorReply("BAD_FRAME", "settings.open 缺少 url", id).dump();
        }

        std::string oerr;
        if (!session.webview.openChildWindow(utf8ToWide(url), oerr)) {
            return errorReply("SETTINGS_OPEN_FAILED", oerr, id).dump();
        }

        // ⚠️ 必须显式提到置顶窗口之上。
        //
        // 这条路径（托盘菜单 / 悬浮窗右键菜单的「设置」）只是**创建**了窗口，
        // 不含任何"把它带到前面"的动作。而游戏窗口是 TOPMOST 的，
        // 于是用户看到的是：点了「设置」什么也没发生（窗口被压在游戏下面）。
        if (session.settings.valid()) {
            raiseAboveTopmost(static_cast<HWND>(session.settings.hwnd()));
        }

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("webview", webviewState(session.webview));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── window.focus：把悬浮窗带到前台（热键呼出输入框）──
    //
    // 热键是全局的：用户可能正在游戏里，悬浮窗既不在前台也可能被隐藏。
    // 不做这一步的话，前端那边的 inputRef.focus() 只会在「窗口已经是前台」
    // 时才有可见效果——也就是热键按下去经常等于没反应。
    if (type == TN_C2N_WINDOW_FOCUS) {
        if (!session.window.valid()) {
            return errorReply("NO_WINDOW", "尚未创建窗口，请先发送 display.create", id).dump();
        }
        const bool ok = session.window.focus();

        // 把鼠标也挪到窗口上。
        //
        // V1 `overlay.py:966-977` 的 `_click_on_widget` 就是这么做的：
        // 把光标 `SetCursorPos` 到输入框中心，再补一次左键按下/抬起。
        // V1 这么做有两个目的——**让用户不用找鼠标**，以及（它的主要动机）
        // 强制 Tk 把焦点给输入框。
        //
        // V2 只需要前一半：前端收到 hotkey 事件后会自己 `focus()` 输入栏
        // （而且带 50/150ms 重试），所以这里**不点**——多点一下会打断
        // 用户可能正在进行的拖拽或选择。
        //
        // 落点选窗口**底部**而不是中心：悬浮窗的输入栏就在底部，
        // 那里正是用户接下来要打字的地方，比窗口中心更贴近意图。
        // 24px 是输入栏的视觉高度量级，够落在栏内而不会压到窗口边缘。
        {
            HWND h = static_cast<HWND>(session.window.hwnd());
            RECT rc{};
            if (GetClientRect(h, &rc)) {
                POINT pt{ (rc.left + rc.right) / 2, rc.bottom - 24 };
                if (ClientToScreen(h, &pt)) {
                    SetCursorPos(pt.x, pt.y);
                }
            }
        }

        Json p = Json::makeObject();
        p.set("ok", Json(ok));
        p.set("display", windowState(session.window));
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── input.send：模拟按键把一条消息发到游戏聊天里 ──
    //
    // 注意这里**会阻塞约 1.5 秒**（三段 500ms 等待）。这不是问题：
    // input.cpp 的等待实现会在期间持续泵窗口消息，所以界面全程可用。
    if (type == TN_C2N_INPUT_SEND) {
        const Json* payload = msg.find("payload");
        if (!payload || !payload->isObject()) {
            return errorReply("BAD_FRAME", "input.send 缺少 payload 对象", id).dump();
        }

        SendRequest req;
        req.text = utf8ToWide(strField(*payload, "text", ""));
        // VK/Mods 由 Go 侧解析（单一解析器，见缺陷 D7），native 不碰热键字符串
        req.hotkeyVK = static_cast<unsigned int>(intField(*payload, "hotkeyVK", 0));
        req.hotkeyMods = static_cast<unsigned int>(intField(*payload, "hotkeyMods", 0));
        req.delayMs = intField(*payload, "delayMs", 500);

        if (req.hotkeyVK == 0) {
            return errorReply("BAD_HOTKEY", "hotkeyVK 为 0，无法模拟按键", id).dump();
        }
        if (req.text.empty()) {
            return errorReply("BAD_FRAME", "text 为空", id).dump();
        }

        const SendResult r = session.input.sendChatMessage(req);

        std::printf("[native] input.send %s\n", r.ok ? "成功" : r.reason.c_str());
        std::fflush(stdout);

        Json p = Json::makeObject();
        p.set("ok", Json(r.ok));
        if (!r.ok) {
            p.set("code", Json(std::string("SEND_FAILED")));
            p.set("message", Json(r.reason));
        }
        return envelope(TN_N2C_INPUT_RESULT, id, std::move(p)).dump();
    }

    // ── input.copy：把文本放进剪贴板 ──
    if (type == TN_C2N_INPUT_COPY) {
        const Json* payload = msg.find("payload");
        const std::string text = payload ? strField(*payload, "text", "") : "";
        const bool ok = session.input.setClipboardText(utf8ToWide(text));

        Json p = Json::makeObject();
        p.set("ok", Json(ok));
        if (!ok) {
            p.set("code", Json(std::string("CLIPBOARD_FAILED")));
            p.set("message", Json("写入剪贴板失败（可能被其他程序占用）"));
        }
        return envelope(TN_N2C_INPUT_RESULT, id, std::move(p)).dump();
    }

    // ── hotkey.set：全量替换全局热键（对应 ADR-012 的期望状态重放）──
    //
    // 载荷：{"list":[{"id":"copy","vk":67,"mods":2,"enabled":true}, ...]}
    // vk/mods 由 Go 侧解析好（缺陷 D7：热键解析只有一处实现）。
    //
    // 注册失败的条目会降级为轮询，并在回复里如实列出——不让 Go 以为
    // 「设置成功了」而实际有热键根本不工作。
    if (type == TN_C2N_HOTKEY_SET) {
        const Json* payload = msg.find("payload");
        const Json* list = payload ? payload->find("list") : nullptr;

        std::vector<HotkeySpec> specs;
        if (list) specs = parseHotkeyList(*list);

        std::string herr;
        session.hotkeys.apply(specs, herr);

        std::printf("[native] 热键已设置：%zu 个，其中 %zu 个走轮询降级\n",
                    session.hotkeys.size(), session.hotkeys.fallbackIDs().size());
        std::fflush(stdout);

        Json fallback = Json::makeArray();
        // 变量名不能叫 id：外层已有请求的 id（double），/W4 下 C4456 遮蔽警告
        for (const std::string& hkID : session.hotkeys.fallbackIDs()) {
            fallback.push(Json(hkID));
        }

        Json p = Json::makeObject();
        p.set("ok", Json(true));
        p.set("registered", Json(static_cast<double>(session.hotkeys.size())));
        p.set("fallback", std::move(fallback));
        if (!herr.empty()) {
            p.set("warning", Json(herr));
        }
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── display.update：不需要实现 ──
    //
    // 消息**不经 native**：前端通过 SSE（`/api/events`）直接从 Go 拿，
    // Go 有消息就推、前端收到就渲染。native 只负责把页面显示在窗口里，
    // 不参与内容分发。这条消息类型保留在协议表里是为了完整性，
    // 收到时明确说明，而不是含糊地报「未实现」。
    if (type == TN_C2N_DISPLAY_UPDATE) {
        return errorReply("NOT_NEEDED",
                          "display.update 不需要：消息经 SSE 直达前端，native 只托管页面", id)
            .dump();
    }

    // ── ping / pong：心跳（Go 侧 1 秒一次，连续 3 次无响应判定失联）──
    if (type == TN_C2N_PING) {
        const Json* payload = msg.find("payload");
        const Json* seqNode = payload ? payload->find("seq") : nullptr;
        Json p = Json::makeObject();
        p.set("seq", Json(seqNode ? seqNode->asNumber() : 0.0));
        return envelope(TN_N2C_PONG, id, std::move(p)).dump();
    }

    // ── shutdown：优雅退出 ──
    if (type == TN_C2N_SHUTDOWN) {
        Json p = Json::makeObject();
        p.set("ok", Json(true));
        stop = true;
        std::printf("[native] 收到 shutdown，准备退出\n");
        return envelope(TN_N2C_STATE_CHANGED, id, std::move(p)).dump();
    }

    // ── 未实现的消息：明确回报，不静默吞掉 ──
    // 静默忽略会让 Go 侧以为命令生效了，那是难查的失效。
    std::printf("[native] 未实现的消息类型: %s\n", type.c_str());
    return errorReply("UNSUPPORTED", "本阶段尚未实现: " + type, id).dump();
}

}  // namespace tn
