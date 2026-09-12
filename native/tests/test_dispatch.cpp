// 消息分发自测。
//
// 直接喂 JSON 帧给 tn::handleMessage，断言回复内容**并且**回头核对真实窗口状态。
// 这样即使管道客户端在沙箱里被禁用，分发逻辑依然能被完整验证。
//
// 分两部分：
//   1. 纯协议断言（握手、错误码、心跳、重放）——任何环境都能跑
//   2. 真实窗口断言——需要交互式桌面会话；没有会话时明确跳过而不是假装通过
#include "../src/dispatch.hpp"
#include "../src/json.hpp"

#include <windows.h>

#include <cstdio>
#include <string>

namespace {

int g_failed = 0;
int g_passed = 0;
int g_skipped = 0;

void check(bool cond, const std::string& what) {
    if (cond) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s\n", what.c_str());
}

void skip(const std::string& what) {
    ++g_skipped;
    std::printf("  SKIP: %s\n", what.c_str());
}

void checkEqInt(int got, int want, const std::string& what) {
    if (got == want) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s  got=%d want=%d\n", what.c_str(), got, want);
}

void checkEqStr(const std::string& got, const std::string& want, const std::string& what) {
    if (got == want) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s  got=\"%s\" want=\"%s\"\n", what.c_str(), got.c_str(), want.c_str());
}

// ── 请求/回复小工具 ───────────────────────────────────────────

struct Reply {
    bool parsed = false;
    std::string type;
    tn::Json payload;

    bool has(const std::string& key) const { return payload.find(key) != nullptr; }

    std::string str(const std::string& key, const std::string& def = "") const {
        const tn::Json* v = payload.find(key);
        return (v && v->isString()) ? v->asString() : def;
    }
    bool boolean(const std::string& key, bool def = false) const {
        const tn::Json* v = payload.find(key);
        return (v && v->type == tn::JsonType::Bool) ? v->asBool(def) : def;
    }
    int number(const std::string& key, int def = -1) const {
        const tn::Json* v = payload.find(key);
        return (v && v->isNumber()) ? v->asInt(def) : def;
    }
    const tn::Json* obj(const std::string& key) const {
        const tn::Json* v = payload.find(key);
        return (v && v->isObject()) ? v : nullptr;
    }

    // display 段内的字段：窗口类命令把状态放在 payload.display 里，
    // 从顶层读会静默拿到默认值，断言就永远不会失败
    bool displayBool(const std::string& key, bool def = false) const {
        const tn::Json* d = obj("display");
        if (!d) return def;
        const tn::Json* v = d->find(key);
        return (v && v->type == tn::JsonType::Bool) ? v->asBool(def) : def;
    }
    int displayInt(const std::string& key, int def = -1) const {
        const tn::Json* d = obj("display");
        if (!d) return def;
        const tn::Json* v = d->find(key);
        return (v && v->isNumber()) ? v->asInt(def) : def;
    }
};

// 数组字段里是否含某个字符串
bool arrayHas(const tn::Json& o, const std::string& key, const std::string& want) {
    const tn::Json* v = o.find(key);
    if (!v || !v->isArray()) return false;
    for (const tn::Json& item : v->array) {
        if (item.isString() && item.asString() == want) return true;
    }
    return false;
}

Reply send(tn::Session& s, const std::string& raw, bool& stop) {
    const std::string out = tn::handleMessage(raw, s, stop);

    Reply r;
    tn::Json m;
    std::string err;
    if (!tn::Json::parse(out, m, err)) return r;
    r.parsed = true;

    const tn::Json* t = m.find("type");
    if (t) r.type = t->asString();
    const tn::Json* p = m.find("payload");
    if (p && p->isObject()) r.payload = *p;
    return r;
}

// 构造一条请求帧。id 用 int，避免 std::to_string 产生小数
std::string req(const std::string& type, int id, const std::string& payloadJson) {
    return "{\"type\":\"" + type + "\",\"id\":" + std::to_string(id) +
           ",\"payload\":" + payloadJson + "}";
}

// 回复里 display 段合并进 payload 的常用断言
void checkDisplay(const Reply& r, int x, int y, int w, int h, const std::string& what) {
    const tn::Json* d = r.obj("display");
    if (!d) { ++g_failed; std::printf("  FAIL: %s —— 回复缺少 display 段\n", what.c_str()); return; }
    const tn::Json* vx = d->find("x"); const tn::Json* vy = d->find("y");
    const tn::Json* vw = d->find("width"); const tn::Json* vh = d->find("height");
    const int gx = vx ? vx->asInt(-1) : -1, gy = vy ? vy->asInt(-1) : -1;
    const int gw = vw ? vw->asInt(-1) : -1, gh = vh ? vh->asInt(-1) : -1;
    if (gx == x && gy == y && gw == w && gh == h) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s  位置尺寸 got=(%d,%d,%d,%d) want=(%d,%d,%d,%d)\n",
                what.c_str(), gx, gy, gw, gh, x, y, w, h);
}

// ── 1. 纯协议断言 ────────────────────────────────────────────

void testHandshake(tn::Session& s, bool& stop) {
    Reply r = send(s, req("native.hello", 1, "{\"protocolVersion\":1}"), stop);

    check(r.parsed, "握手回复是合法 JSON");
    checkEqStr(r.type, "native.ready", "握手回复类型");
    check(r.boolean("versionMatch"), "协议版本匹配");
    checkEqInt(r.number("protocolVersion"), 1, "回报的协议版本");

    const tn::Json* caps = r.payload.find("capabilities");
    check(caps && caps->isArray() && !caps->array.empty(), "能力清单非空");
    check(arrayHas(r.payload, "capabilities", "window"), "能力清单含 window");

    const tn::Json* st = r.obj("actualState");
    check(st != nullptr, "握手回报实际状态");
    if (st) {
        const tn::Json* d = st->find("display");
        check(d != nullptr && d->isObject(), "实际状态含 display 段");
        if (d) {
            const tn::Json* e = d->find("exists");
            check(e != nullptr && e->asBool(true) == false, "尚未建窗时 exists 应为 false");
        }
    }
}

void testVersionMismatch(tn::Session& s, bool& stop) {
    Reply r = send(s, req("native.hello", 2, "{\"protocolVersion\":99}"), stop);
    checkEqStr(r.type, "native.ready", "版本不符时仍回复 ready");
    check(!r.boolean("versionMatch", true), "版本不符时 versionMatch 应为 false");
}

void testBadFrames(tn::Session& s, bool& stop) {
    Reply r1 = send(s, "{\"type\":\"ping\"", stop);  // 缺右括号
    checkEqStr(r1.type, "event.error", "非法 JSON 回报 event.error");
    checkEqStr(r1.str("code"), "BAD_FRAME", "非法 JSON 的错误码");

    Reply r2 = send(s, "{\"id\":1}", stop);  // 缺 type
    checkEqStr(r2.str("code"), "BAD_FRAME", "缺 type 的错误码");

    Reply r3 = send(s, req("display.create", 3, "null"), stop);
    checkEqStr(r3.str("code"), "BAD_FRAME", "display.create 缺 payload 的错误码");
}

void testPing(tn::Session& s, bool& stop) {
    Reply r = send(s, req("ping", 7, "{\"seq\":7}"), stop);
    checkEqStr(r.type, "pong", "心跳回复类型");
    checkEqInt(r.number("seq"), 7, "心跳序号回带");
}

// tray.set：托盘现在**已实现**。
//
// ⚠️ 这个用例原来是断言 tray.set 报 UNSUPPORTED 的——那是托盘还没做的年代
//    写的。实现之后不改它，就会把「已经做好」当成失败。热键的 replay 分支
//    当初就埋过同一颗雷（实现了却忘了改 replay，重连后热键不恢复）。
//
// ⚠️ 这里只发 enabled=false：enabled=true 会真的往用户托盘里挂一个图标，
//    和 input 测试污染用户剪贴板是同一类问题——测试不该有外部可见副作用。
void testTraySet(tn::Session& s, bool& stop) {
    Reply r = send(s, req("tray.set", 31,
                          "{\"enabled\":false,\"tip\":\"测试\",\"menu\":[]}"), stop);
    checkEqStr(r.type, "event.stateChanged", "tray.set 应正常受理");
    check(r.boolean("ok"), "tray.set 应回报 ok");

    const tn::Json* trayObj = r.obj("tray");
    check(trayObj != nullptr, "tray.set 应带上托盘实际状态");
    if (trayObj != nullptr) {
        const tn::Json* active = trayObj->find("active");
        const bool isActive = (active && active->type == tn::JsonType::Bool)
                                  ? active->asBool(false) : true;
        check(!isActive, "停用状态下托盘不应处于 active");
    }
}

void testUnimplemented(tn::Session& s, bool& stop) {
    // 未实现的能力必须明确回报错误，绝不能静默假装成功。
    //
    // 注意：协议里剩下的命令现在**都已实现**（托盘是最后一块），所以
    // 只有「类型未知」这一条还能触发 UNSUPPORTED。display.update 是
    // 另一回事——它不是未实现，而是**不需要**（见下）。
    Reply r2 = send(s, req("display.update", 12, "{\"messages\":[]}"), stop);
    // display.update 不是「未实现」，而是**不需要**：消息经 SSE 直达前端，
    // native 只托管页面。两种情况的错误码不同，别混为一谈。
    checkEqStr(r2.str("code"), "NOT_NEEDED", "display.update 应说明「不需要」而非「未实现」");

    Reply r3 = send(s, req("totally.unknown", 13, "{}"), stop);
    checkEqStr(r3.str("code"), "UNSUPPORTED", "未知消息类型的错误码");
    check(r3.str("message").find("totally.unknown") != std::string::npos,
          "错误信息里应带上原始类型名");
}

// hotkey.set：全量替换、跳过禁用项、能清空。
void testHotkeySet(tn::Session& s, bool& stop) {
    Reply r = send(s, req("hotkey.set", 71,
                          "{\"list\":[{\"id\":\"copy\",\"vk\":67,\"mods\":2,\"enabled\":true}]}"),
                   stop);
    checkEqStr(r.type, "event.stateChanged", "hotkey.set 回复类型");
    check(r.boolean("ok"), "hotkey.set 回报 ok");
    checkEqInt(r.number("registered"), 1, "应注册 1 个热键");
    checkEqInt(static_cast<int>(s.hotkeys.size()), 1, "会话里的热键数");

    // 全量替换：禁用的条目不该被注册
    Reply r2 = send(s, req("hotkey.set", 72,
                           "{\"list\":[{\"id\":\"a\",\"vk\":65,\"mods\":0,\"enabled\":true},"
                           "{\"id\":\"b\",\"vk\":66,\"mods\":0,\"enabled\":false}]}"),
                    stop);
    check(r2.boolean("ok"), "第二次 hotkey.set 回报 ok");
    checkEqInt(r2.number("registered"), 1, "禁用项不应被注册");

    // 空列表 = 清空（热键是进程级资源，测试结束必须还回去）
    Reply r3 = send(s, req("hotkey.set", 73, "{\"list\":[]}"), stop);
    checkEqInt(r3.number("registered"), 0, "空列表应清空热键");
    checkEqInt(static_cast<int>(s.hotkeys.size()), 0, "清空后会话里的热键数应为 0");
}

void testNoWindowErrors(tn::Session& s, bool& stop) {
    // 还没建窗就下发窗口命令，必须报 NO_WINDOW 而不是默默成功
    Reply r1 = send(s, req("display.visible", 21, "{\"visible\":true}"), stop);
    checkEqStr(r1.str("code"), "NO_WINDOW", "未建窗时 display.visible 的错误码");

    Reply r2 = send(s, req("display.setClickThrough", 22, "{\"enabled\":true}"), stop);
    checkEqStr(r2.str("code"), "NO_WINDOW", "未建窗时点击穿透的错误码");

    Reply r3 = send(s, req("window.blur", 23, "{\"mode\":\"mica\"}"), stop);
    checkEqStr(r3.str("code"), "NO_WINDOW", "未建窗时切换毛玻璃的错误码");
}

void testShutdownLast(tn::Session& s, bool& stop) {
    Reply r = send(s, req("shutdown", 99, "null"), stop);
    checkEqStr(r.type, "event.stateChanged", "shutdown 回复类型");
    check(r.boolean("ok"), "shutdown 回报 ok");
    check(stop, "shutdown 应置 stop 标志");
}

// ── 2. 真实窗口断言 ──────────────────────────────────────────

void testThemeSet(tn::Session& s, bool& stop) {
    // 主题必须在建窗之后才能设
    s.window.destroy();
    Reply noWin = send(s, req("theme.set", 60, "{\"dark\":false}"), stop);
    checkEqStr(noWin.str("code"), "NO_WINDOW", "未建窗时 theme.set 的错误码");

    // 建窗时就能带 dark —— 材质深浅是在应用材质那一刻决定的
    Reply created = send(s, req("display.create", 61,
                                "{\"x\":80,\"y\":80,\"width\":400,\"height\":260,"
                                "\"blur\":\"auto\",\"dark\":false,\"visible\":true}"),
                         stop);
    check(created.boolean("ok"), "带 dark=false 建窗应成功");
    check(!s.window.isDark(), "建窗后应为浅色模式");
    const tn::Json* d = created.obj("display");
    if (d) {
        const tn::Json* dv = d->find("dark");
        check(dv != nullptr && dv->asBool(true) == false, "实际状态应回报 dark=false");
    }

    // 运行时切换
    Reply toDark = send(s, req("theme.set", 62, "{\"dark\":true}"), stop);
    check(toDark.boolean("ok"), "theme.set 回报 ok");
    check(s.window.isDark(), "切换后应为深色模式");

    Reply toLight = send(s, req("theme.set", 63, "{\"dark\":false}"), stop);
    check(toLight.boolean("ok"), "切回浅色回报 ok");
    check(!s.window.isDark(), "切回后应为浅色模式");

    // 缺字段时应保持深色默认（老调用方不带这个字段）
    Reply dflt = send(s, req("display.create", 64,
                             "{\"x\":0,\"y\":0,\"width\":300,\"height\":200,\"visible\":true}"),
                      stop);
    check(dflt.boolean("ok"), "不带 dark 建窗应成功");
    check(s.window.isDark(), "不带 dark 时应默认为深色");
}

// WebView2 显示层：能力清单必须与**编译期事实**一致。
//
// 这条断言的价值：显示层是编译期可选的（没 SDK 就不接）。若能力清单写死，
// 就会出现「Go 以为页面能显示，而窗口其实是一块空玻璃」这种最难查的问题。
void testWebViewCapability(tn::Session& s, bool& stop) {
    Reply hello = send(s, req("native.hello", 81, "{\"protocolVersion\":1}"), stop);
    const bool advertised = arrayHas(hello.payload, "capabilities", "webview");
    const bool compiledIn = tn::WebViewHost::compiled();
    checkEqInt(advertised ? 1 : 0, compiledIn ? 1 : 0,
               "能力清单里的 webview 应与编译期事实一致");
    std::printf("    （本次构建%s显示层）\n", compiledIn ? "包含" : "不包含");

    // 建窗（**不带 url**）时也要回报 webview 状态——
    // 不带 url 就不会创建 WebView，因此这个用例不会真的拉起浏览器进程。
    Reply c = send(s, req("display.create", 82,
                          "{\"x\":0,\"y\":0,\"width\":300,\"height\":200,\"visible\":true}"),
                   stop);
    check(c.boolean("ok"), "不带 url 建窗应成功");
    const tn::Json* wv = c.obj("webview");
    check(wv != nullptr, "display.create 回复应含 webview 段");
    if (wv) {
        const tn::Json* comp = wv->find("compiled");
        check(comp != nullptr, "webview 段应报告 compiled（编译期事实）");
        const tn::Json* ready = wv->find("ready");
        check(ready != nullptr, "webview 段应报告 ready（运行期状态）");
        // 没给 url 就不该建 WebView
        check(ready != nullptr && ready->asBool(true) == false,
              "没给 url 时不该创建 WebView");
    }
}

void testWebViewExitTeardown(tn::Session& s, bool& stop) {
    (void)s;
    (void)stop;

    // 退出收尾的两条硬性不变量（不依赖真的建过 WebView，任何环境都能跑）：
    //   1) 没建过 WebView 时必须**立刻**成功返回，不能白等一个超时；
    //   2) 可以重复调用——退出路径完全可能被走到两次。
    // 这两条要是破了，表现是「程序退出时莫名卡几秒」或者「退出路径二次进入崩」，
    // 都属于最难查的那一类：没有任何报错，只是慢或者偶尔不出日志。
    tn::WebViewHost host;

    const DWORD t0 = GetTickCount();
    const bool first = host.shutdownForExit(2000);
    const DWORD used = GetTickCount() - t0;
    check(first, "未建 WebView 时 shutdownForExit 应直接成功（没有子进程要等）");
    check(used < 1500, "未建 WebView 时 shutdownForExit 不该等到超时（实测 " +
                           std::to_string(static_cast<unsigned long>(used)) + " ms）");
    check(host.shutdownForExit(2000), "shutdownForExit 应可重复调用");
    check(!host.ready(), "shutdownForExit 之后 ready() 应为假");
    check(!host.childReady(), "shutdownForExit 之后 childReady() 应为假");
}

void testDisplayLifecycle(tn::Session& s, bool& stop) {
    // 建窗：位置尺寸必须与请求一致
    Reply c = send(s, req("display.create", 31,
                          "{\"x\":100,\"y\":120,\"width\":500,\"height\":300,"
                          "\"topmost\":true,\"opacity\":0.85,\"blur\":\"auto\","
                          "\"title\":\"自测窗口\",\"visible\":true}"),
                   stop);
    checkEqStr(c.type, "event.stateChanged", "建窗回复类型");
    check(c.boolean("ok"), "建窗回报 ok");
    checkDisplay(c, 100, 120, 500, 300, "建窗后回报的位置尺寸");

    // 回报之外，再核对一次真实 HWND 的边界——回包可能是自说自话
    int x = 0, y = 0, w = 0, h = 0;
    if (s.window.valid() && s.window.getBounds(x, y, w, h)) {
        checkEqInt(w, 500, "真实窗口宽度");
        checkEqInt(h, 300, "真实窗口高度");
        checkEqInt(x, 100, "真实窗口 X");
        checkEqInt(y, 120, "真实窗口 Y");
    } else {
        check(false, "建窗后应能从 HWND 读到边界");
    }
    check(s.window.valid(), "建窗后窗口句柄有效");

    // 重建：期望状态变了必须真的生效，不能被 create() 的早退吞掉
    Reply c2 = send(s, req("display.create", 32,
                           "{\"x\":200,\"y\":240,\"width\":700,\"height\":400,"
                           "\"blur\":\"none\",\"opacity\":1.0,\"visible\":true}"),
                    stop);
    check(c2.boolean("ok"), "重建窗口回报 ok");
    checkDisplay(c2, 200, 240, 700, 400, "重建后回报的位置尺寸");
    if (s.window.getBounds(x, y, w, h)) {
        checkEqInt(w, 700, "重建后真实窗口宽度");
        checkEqInt(h, 400, "重建后真实窗口高度");
    }

    // 隐藏 / 显示
    Reply hid = send(s, req("display.visible", 33, "{\"visible\":false}"), stop);
    check(!hid.displayBool("visible", true), "隐藏后回报 visible=false");
    check(!s.window.isVisible(), "隐藏后真实窗口不可见");

    Reply shw = send(s, req("display.visible", 34, "{\"visible\":true}"), stop);
    check(shw.displayBool("visible"), "显示后回报 visible=true");
    check(s.window.isVisible(), "显示后真实窗口可见");

    // 点击穿透
    Reply on = send(s, req("display.setClickThrough", 35, "{\"enabled\":true}"), stop);
    check(on.displayBool("clickThrough"), "开启穿透后回报 clickThrough=true");
    check(s.window.isClickThrough(), "开启穿透后真实状态为开启");

    Reply off = send(s, req("display.setClickThrough", 36, "{\"enabled\":false}"), stop);
    check(!off.displayBool("clickThrough", true), "关闭穿透后回报 clickThrough=false");
    check(!s.window.isClickThrough(), "关闭穿透后真实状态为关闭");

    // 毛玻璃：只要求调用成功且报回一个已知模式，不假设本机支持哪种
    Reply blur = send(s, req("window.blur", 37, "{\"mode\":\"auto\"}"), stop);
    check(blur.boolean("ok"), "auto 毛玻璃应报成功");
    const std::string mode = blur.obj("display") && blur.obj("display")->find("blur")
                                 ? blur.obj("display")->find("blur")->asString()
                                 : std::string();
    check(mode == "mica" || mode == "acrylic" || mode == "none",
          "毛玻璃模式应在已知取值内，实际=" + mode);

    Reply none = send(s, req("window.blur", 38, "{\"mode\":\"none\"}"), stop);
    check(none.boolean("ok"), "关闭毛玻璃应报成功");
    if (none.obj("display")) {
        const tn::Json* b = none.obj("display")->find("blur");
        checkEqStr(b ? b->asString() : std::string(), "none", "关闭后实际模式为 none");
    }
}

void testReplay(tn::Session& s, bool& stop) {
    // 销毁再重放：模拟 Go 重连后重建期望状态
    s.window.destroy();

    Reply r = send(s, req("native.replay", 41,
                          "{\"registry\":{\"display\":{\"x\":60,\"y\":60,"
                          "\"width\":640,\"height\":320,\"blur\":\"auto\",\"visible\":true},"
                          // 热键用的是**解析后**的 vk/mods —— native 不认热键字符串
                          "\"hotkeys\":[{\"id\":\"toggle\",\"vk\":84,\"mods\":3,\"enabled\":true}]}}"),
                   stop);
    check(r.boolean("ok"), "重放回报 ok");
    check(arrayHas(r.payload, "applied", "display"), "重放应把 display 列入 applied");
    // 热键现在已经实现：重放时应当**被应用**，而不是像早期那样列进 skipped。
    // 这条断言是端到端跑出来的——当时 replay 把 hotkeys 报成「跳过」，
    // 于是重连后热键根本不会被恢复。
    check(arrayHas(r.payload, "applied", "hotkeys"), "重放应把 hotkeys 列入 applied");
    check(!arrayHas(r.payload, "skipped", "hotkeys"), "已实现的热键不该列入 skipped");
    checkEqInt(static_cast<int>(s.hotkeys.size()), 1, "重放后热键应已注册");

    const tn::Json* st = r.obj("actualState");
    if (st) {
        const tn::Json* d = st->find("display");
        const tn::Json* w = d ? d->find("width") : nullptr;
        checkEqInt(w ? w->asInt(-1) : -1, 640, "重放后实际宽度");
    } else {
        check(false, "重放应回报实际状态");
    }

    int x = 0, y = 0, w = 0, h = 0;
    if (s.window.getBounds(x, y, w, h)) {
        checkEqInt(w, 640, "重放后真实窗口宽度");
        checkEqInt(h, 320, "重放后真实窗口高度");
    }

    // 空的热键列表不该被报成 skipped——那会让 Go 以为有东西没生效
    Reply empty = send(s, req("native.replay", 42,
                              "{\"registry\":{\"hotkeys\":[]}}"),
                       stop);
    check(empty.boolean("ok"), "空重放回报 ok");
    check(!arrayHas(empty.payload, "skipped", "hotkeys"), "空热键列表不应报 skipped");
    check(!arrayHas(empty.payload, "applied", "display"), "无 display 段时不应报 applied");
}

void testEdgeCases(tn::Session& s, bool& stop) {
    // 极小窗口：不设人为下限（保持与 V1 一致），但必须建得出来且不崩
    Reply tiny = send(s, req("display.create", 51,
                             "{\"x\":0,\"y\":0,\"width\":20,\"height\":20,\"visible\":true}"),
                      stop);
    check(tiny.boolean("ok"), "极小窗口应能创建");

    // 负坐标与超大尺寸：解析不能出错（-1 表示自动居中）
    Reply neg = send(s, req("display.create", 52,
                            "{\"x\":-1,\"y\":-1,\"width\":400,\"height\":200,\"visible\":true}"),
                     stop);
    check(neg.boolean("ok"), "负坐标（自动居中）应能创建");
    const tn::Json* d = neg.obj("display");
    const tn::Json* vx = d ? d->find("x") : nullptr;
    check(vx && vx->asInt(-1) >= 0, "自动居中后 X 应为非负实际值");

    // 字段类型不符：应回落到默认值而不是解析失败。
    // 没给 x/y 就是 -1 → 自动居中，所以位置是由屏幕尺寸算出来的，只能断言非负。
    Reply wrong = send(s, req("display.create", 53,
                              "{\"width\":\"很宽\",\"topmost\":\"yes\",\"opacity\":\"半透明\"}"),
                       stop);
    check(wrong.boolean("ok"), "字段类型不符时应回落默认值并建窗成功");
    checkEqInt(wrong.displayInt("width"), 620, "类型不符时宽度回落 620");
    checkEqInt(wrong.displayInt("height"), 360, "类型不符时高度回落 360");
    check(wrong.displayInt("x", -1) >= 0, "未给坐标时应自动居中（X 非负）");
    check(wrong.displayInt("y", -1) >= 0, "未给坐标时应自动居中（Y 非负）");
}

void testCleanup(tn::Session& s) {
    check(s.window.destroy(), "销毁窗口");
    check(!s.window.valid(), "销毁后句柄失效");
    // 热键是进程级资源（占着系统热键表），测试结束必须还给系统，
    // 否则同一台机器上后续测试会莫名其妙地「注册失败」。
    s.hotkeys.release();
    checkEqInt(static_cast<int>(s.hotkeys.size()), 0, "热键应已全部注销");
}

}  // namespace

int main() {
    std::printf("=== translator_native 分发自测 ===\n");

    tn::Session session;
    bool stop = false;

    // 纯协议部分：不依赖窗口
    testHandshake(session, stop);
    testVersionMismatch(session, stop);
    testBadFrames(session, stop);
    testPing(session, stop);
    testTraySet(session, stop);
    testUnimplemented(session, stop);
    testNoWindowErrors(session, stop);
    testWebViewExitTeardown(session, stop);

    // 窗口部分：需要交互式桌面会话
    if (tn::OverlayWindow::hasDesktopSession()) {
        testWebViewCapability(session, stop);
        testHotkeySet(session, stop);
        testDisplayLifecycle(session, stop);
        testThemeSet(session, stop);
        testReplay(session, stop);
        testEdgeCases(session, stop);
        testCleanup(session);
    } else {
        skip("无交互式桌面会话，跳过全部窗口相关断言");
    }

    testShutdownLast(session, stop);

    std::printf("通过 %d 项，失败 %d 项，跳过 %d 项\n", g_passed, g_failed, g_skipped);
    if (g_failed > 0) {
        std::printf("=== 存在失败 ===\n");
        return 1;
    }
    std::printf("=== 全部通过 ===\n");
    return 0;
}
