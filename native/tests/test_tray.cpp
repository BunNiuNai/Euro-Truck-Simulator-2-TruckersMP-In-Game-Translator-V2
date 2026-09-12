// tray 模块自测。
//
// 重点全在**线程生命周期**上：托盘必须跑在自己的线程里（TrackPopupMenu 是
// 模态的，放主线程会把管道读取一起堵死），而线程对象一旦没收干净，症状都不是
// 异常或报错，而是「进程凭空消失」或「进程再也关不掉」——两种都极难从现象
// 反推原因。所以这里的断言都绕着「还能不能再来一次」展开：
//
//   · 反复 create / destroy —— 旧代码第二次 apply 就会 std::terminate
//     （std::thread 的赋值运算符碰到 joinable 的旧对象直接终止进程）
//   · create 之后**立刻** destroy —— 旧代码会卡在一个永远不会来的消息上
//     （窗口还没发布出来，没人去唤醒 GetMessageW，join 永不返回）
//   · 建窗失败之后析构 —— 旧代码在失败路径上直接 return，留下一个
//     joinable 却没人 join 的线程对象，delete impl_ 时同样是终止进程
//
// 图标能不能真的挂上取决于桌面会话（无会话时 Shell_NotifyIcon 必失败），
// 所以断言的是「要么挂上、要么给出失败原因」这个不变量，而不是具体成败。
#include "../src/tray.hpp"
#include "../src/window.hpp"  // hasDesktopSession：无桌面会话时跳过真实托盘断言

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

void checkEqInt(int got, int want, const std::string& what) {
    if (got == want) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s  got=%d want=%d\n", what.c_str(), got, want);
}

void skip(const std::string& what) {
    ++g_skipped;
    std::printf("  SKIP: %s\n", what.c_str());
}

// 托盘窗口类名。与 tray.cpp 里的 kTrayClass 必须一致——它没有对外暴露，
// 这里只能照着写；对不上时 testDestructAfterFailedCreate 会直接 SKIP 而不是
// 悄悄测了别的类。
constexpr wchar_t kTrayClass[] = L"ETS2TrayIconV2";

tn::TraySpec makeSpec() {
    tn::TraySpec spec;
    spec.enabled = true;
    spec.tip = "ETS2 Translator";

    tn::TrayMenuItem show;
    show.id = "toggle";
    show.label = "Show / Hide 显示/隐藏";
    show.isDefault = true;
    spec.menu.push_back(show);

    tn::TrayMenuItem sep;
    sep.separator = true;
    spec.menu.push_back(sep);

    tn::TrayMenuItem quit;
    quit.id = "quit";
    quit.label = "Exit / 退出";
    spec.menu.push_back(quit);
    return spec;
}

// 等托盘线程把结果落下来：成功（active）或失败（lastError 非空）。
//
// 托盘是**异步**创建的，apply() 返回 true 只代表「已发起」，所以任何断言前
// 都必须先等这一步——否则测的只是「线程还没跑起来时的样子」。
bool waitForOutcome(const tn::TrayIcon& tray, int timeoutMs) {
    for (int waited = 0; waited < timeoutMs; waited += 20) {
        if (tray.active() || !tray.lastError().empty()) return true;
        Sleep(20);
    }
    return false;
}

// ── 生命周期 ────────────────────────────────────────────────

// 反复 create / destroy。
//
// 这条同时钉住两处：① 旧代码把新线程赋给一个仍然 joinable 的旧线程对象，
// std::thread 的 operator= 直接 std::terminate；② destroy() 之后必须真的
// 收干净，不能留下「图标没了但线程还在」的半截状态。
void testRepeatedCreateDestroy() {
    tn::TrayIcon tray;
    const tn::TraySpec spec = makeSpec();

    for (int round = 0; round < 3; ++round) {
        std::string err;
        check(tray.apply(spec, err), "apply 应接受启用规格");

        // 不变量：托盘线程要么把图标挂上，要么给出一条失败原因。
        // 两者都没有 = 线程既没成功也没报错地消失了（正是缺陷的表现）。
        check(waitForOutcome(tray, 3000), "托盘线程应在 3 秒内给出结果（成功或失败原因）");
        if (tray.active()) {
            check(tray.lastError().empty(), "成功挂上图标时不应同时报错");
        } else {
            check(!tray.lastError().empty(), "没挂上图标时必须给出失败原因");
            std::printf("    （第 %d 轮未挂上图标: %s）\n", round + 1, tray.lastError().c_str());
        }

        tray.destroy();
        check(!tray.active(), "destroy 之后不应再报告 active");
        check(tray.takeCommands().empty(), "没有点击时命令队列应为空");
    }
    std::printf("  （重复 create/destroy 3 轮，进程仍然存活）\n");
}

// create 之后**立刻** destroy：刻意撞进「窗口还没建出来」的那个窗口期。
//
// 这是缺陷 b 的回归测试。旧代码只在 hwnd 已经发布时才 PostMessage(WM_CLOSE)，
// 而托盘线程这时可能还停在 CreateWindowExW 之前——没有任何句柄可发，
// 线程随后一头扎进 GetMessageW 等一条永远不会来的消息，join() 永不返回。
// 现象是整个进程卡死在托盘销毁上（关不掉、没日志，只能杀进程）。
// 这条测试如果卡住，就是缺陷复现了。
void testDestroyBeforeWindowPublished() {
    tn::TrayIcon tray;
    const tn::TraySpec spec = makeSpec();

    const int rounds = 20;
    for (int i = 0; i < rounds; ++i) {
        std::string err;
        check(tray.apply(spec, err), "反复 apply 应都返回 true");
        tray.destroy();   // 卡在这里 = 缺陷 b 复现
    }
    check(!tray.active(), "反复销毁后不应报告 active");
    std::printf("  （%d 轮「apply 后立刻 destroy」全部返回，没有卡住）\n", rounds);
}

// destroy 之后还能再创建：证明没有留下悬空的线程对象。
//
// 这是缺陷 c 在正常环境下**能观察到**的那一半。旧代码在 run() 的失败分支上
// 直接 return，线程对象仍然 joinable 却又没人 join——于是下一次 apply 赋新值时
// std::terminate()，进程猝死（0xC0000409），日志里什么都没有。
void testRestartAfterDestroy() {
    tn::TrayIcon tray;
    const tn::TraySpec spec = makeSpec();

    std::string err;
    check(tray.apply(spec, err), "第一次 apply");
    check(waitForOutcome(tray, 3000), "第一次创建应给出结果");
    tray.destroy();

    check(tray.apply(spec, err), "销毁之后应还能再创建（第二次 apply）");
    check(waitForOutcome(tray, 3000), "第二次创建应给出结果");
    tray.destroy();
    check(!tray.active(), "第二次销毁后不应报告 active");
}

// 建窗必然失败的窗口过程：WM_CREATE 返回 -1 就会拒绝创建，
// CreateWindowExW 随之返回 NULL（这是「在创建阶段拒绝创建」的标准做法；
// 注意 WM_NCCREATE 的语义不同，它要返回 FALSE 才拒绝，返回 -1 反而算通过）。
LRESULT CALLBACK rejectCreateProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp) {
    if (msg == WM_CREATE) return -1;
    return DefWindowProcW(hwnd, msg, wp, lp);
}

// 允许建窗、但**吞掉** WM_CLOSE 的窗口过程：窗口不会因此被销毁，
// PostQuitMessage 也永远没人调——消息循环只会在 GetMessageW 上一直等下去。
LRESULT CALLBACK deafCloseProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp) {
    if (msg == WM_CLOSE) return 0;
    return DefWindowProcW(hwnd, msg, wp, lp);
}

// 把托盘窗口类名劫持成自己的窗口过程。返回 true 表示劫持成功；
// 调用方负责用 unhijackTrayClass 还回去。
bool hijackTrayClass(WNDPROC proc) {
    HMODULE own = GetModuleHandleW(nullptr);
    if (own == nullptr) return false;

    // ⚠️ 先把类名从自己名下**腾出来**：前面的托盘测试已经注册过它，
    //    不腾的话下面这次注册只会拿到 ALREADY_EXISTS，劫持不到。
    //    此刻进程里没有活着的托盘窗口，注销是安全的。
    UnregisterClassW(kTrayClass, own);

    WNDCLASSEXW wc{};
    wc.cbSize = sizeof(wc);
    wc.lpfnWndProc = proc;
    wc.hInstance = own;
    wc.lpszClassName = kTrayClass;
    return RegisterClassExW(&wc) != 0;
}

bool unhijackTrayClass() {
    return UnregisterClassW(kTrayClass, GetModuleHandleW(nullptr)) != FALSE;
}

// 创建失败之后析构不能崩（缺陷 c 的核心）。
//
// 怎么**人为**制造建窗失败：抢先把 `ETS2TrayIconV2` 注册成自己的类，窗口过程
// 在 WM_CREATE 里返回 -1。托盘线程再注册同名类只会拿到 ERROR_CLASS_ALREADY_EXISTS
// （它是容忍的）→ 于是创建时用的正是我们那个过程 → CreateWindowExW 必然返回
// NULL，attach() 走到失败分支。这就是旧代码留下 joinable 线程对象、
// 随后在析构里 std::terminate 的那条路。
//
// 不用「把类名挂到别的模块名下」那种写法：RegisterClassExW 会校验 hInstance，
// 传别人的模块句柄直接 ERROR_INVALID_PARAMETER（err 87），根本注册不上。
void testDestructAfterFailedCreate() {
    if (!hijackTrayClass(rejectCreateProc)) {
        skip("无法劫持托盘窗口类名，跳过建窗失败路径");
        return;
    }

    bool attachFailed = false;
    {
        tn::TrayIcon tray;
        std::string err;
        tray.apply(makeSpec(), err);

        // 先断言这条路径**确实**走到了失败分支：否则测试看着通过，
        // 实际测的是正常路径，等于什么都没测。
        attachFailed = waitForOutcome(tray, 3000) && !tray.active();
        check(attachFailed, "劫持类名之后建窗应当失败（否则没测到目标路径）");
        if (attachFailed) {
            check(!tray.lastError().empty(), "建窗失败必须留下失败原因（不能静默）");
            std::printf("    （建窗确实失败: %s）\n", tray.lastError().c_str());
        }

        // 关键：这里 destroy() 之后 tray 析构。
        // 旧代码在这条路上会 std::terminate——进程当场消失，连 FAIL 都打不出来。
        tray.destroy();
    }
    check(true, "建窗失败后 destroy/析构都不应终止进程");

    check(unhijackTrayClass(), "劫持用的窗口类应能注销掉（否则会影响后续测试）");
}

// destroy() 必须**无条件**返回，哪怕窗口过程完全不配合。
//
// 这条是缺陷 b 的加强版：常规路径下 WM_CLOSE 就足以唤醒托盘线程的消息循环，
// 但那要依赖「窗口过程是我们的、收到 WM_CLOSE 会 DestroyWindow → PostQuitMessage」。
// 把窗口过程换成一个吞掉 WM_CLOSE 的过程之后，那条路就断了——此时唯一的
// 唤醒手段是投到**线程**消息队列的停止消息。
//
// 为什么值得这么较真：destroy() 走在退出路径上，在这里挂住的表现是
// 「程序关不掉、日志也没有」，用户只能杀进程，而现场什么线索都不剩。
void testDestroyWithForeignWindowProc() {
    if (!hijackTrayClass(deafCloseProc)) {
        skip("无法劫持托盘窗口类名，跳过窗口过程不配合的销毁路径");
        return;
    }

    {
        tn::TrayIcon tray;
        std::string err;
        tray.apply(makeSpec(), err);

        // 等到图标真的挂上，说明托盘线程已经把窗口建出来并**进了消息循环**
        // （只有进了循环才谈得上「唤醒它」）。再等一会儿确保它确实睡在
        // GetMessageW 里面，而不是还在循环前面。
        const bool up = waitForOutcome(tray, 3000);
        if (!up) {
            skip("托盘线程没有在 3 秒内给出结果，跳过这条断言");
        } else {
            Sleep(200);
            tray.destroy();   // 卡在这里 = 「窗口过程不配合时 destroy 挂死」
            check(!tray.active(), "destroy 之后不应再报告 active");
            check(true, "窗口过程吞掉 WM_CLOSE 时 destroy 仍然返回了");
        }
        tray.destroy();   // 可重复
    }

    check(unhijackTrayClass(), "劫持用的窗口类应能注销掉（否则会影响后续测试）");
}

// 劫持收尾之后，正常路径必须还能走通：证明前面两次失败没有污染进程状态。
void testNormalPathAfterHijack() {
    tn::TrayIcon tray;
    std::string err;
    check(tray.apply(makeSpec(), err), "劫持测试之后应还能创建托盘");
    check(waitForOutcome(tray, 3000), "应能给出结果");
    check(tray.active(), "图标应能重新挂上");
    tray.destroy();
    check(!tray.active(), "销毁后状态应清干净");
}

// ── 停用与空对象 ────────────────────────────────────────────

void testDisabledAndEmpty() {
    // 从未 apply 过：所有查询都必须安全（impl_ 为空）
    tn::TrayIcon never;
    check(!never.active(), "未 apply 时不应 active");
    check(never.lastError().empty(), "未 apply 时不应有错误");
    check(never.takeCommands().empty(), "未 apply 时命令队列应为空");
    never.destroy();
    check(true, "未 apply 时 destroy 不应崩");

    // 停用规格：不建托盘，且可以反复 destroy
    tn::TrayIcon tray;
    tn::TraySpec off;   // enabled 默认 false
    std::string err;
    check(tray.apply(off, err), "停用规格应返回 true");
    check(!tray.active(), "停用后不应 active");
    tray.destroy();
    tray.destroy();
    check(true, "重复 destroy 不应崩");

    // 启用 → 停用：destroy() 由 apply 内部调，这条走的是「已在运行又收到停用」
    tn::TrayIcon toggled;
    std::string terr;
    check(toggled.apply(makeSpec(), terr), "启用");
    check(waitForOutcome(toggled, 3000), "启用后应给出结果");
    check(toggled.apply(off, err), "再停用");
    check(!toggled.active(), "停用后不应再 active");
}

// 提示文字的刷新走的是「已在运行 → PostMessage」这条路：
// 它不应改变正在跑的状态，也不应让 destroy() 之后收不干净。
void testReapplyWhileRunning() {
    tn::TrayIcon tray;
    tn::TraySpec spec = makeSpec();
    std::string err;
    if (!tray.apply(spec, err)) {
        check(false, "首次 apply 应成功");
        return;
    }
    if (!waitForOutcome(tray, 3000)) {
        skip("托盘没有在 3 秒内给出结果，跳过重复 apply 断言");
        return;
    }
    const bool wasActive = tray.active();

    spec.tip = "ETS2 Translator（已更新）";
    for (int i = 0; i < 5; ++i) {
        check(tray.apply(spec, err), "运行中重复 apply 应始终返回 true");
    }
    checkEqInt(tray.active() ? 1 : 0, wasActive ? 1 : 0, "重复 apply 不应改变 active 状态");
    tray.destroy();
    check(!tray.active(), "destroy 之后状态应清干净");
}

}  // namespace

int main() {
    // ⚠️ 关掉 stdout 缓冲。这一套里有用例**故意**去踩「卡住」和「进程被终止」
    //    那两条路：默认的缓冲会让崩溃或挂起时已打印的内容全部丢掉，
    //    排查时看到的是一个空文件——那正是最需要线索的时候。
    std::setvbuf(stdout, nullptr, _IONBF, 0);

    std::printf("=== translator_native tray 自测 ===\n");
    std::printf("  桌面会话: %s\n",
                tn::TrayIcon::hasDesktopSession() ? "可用" : "不可用（托盘断言将放宽）");

    // 无桌面会话时也能跑：那正是「建窗失败」的真实环境，检查点反而更有意义。
    testDisabledAndEmpty();
    testRepeatedCreateDestroy();
    testDestroyBeforeWindowPublished();
    testRestartAfterDestroy();
    testReapplyWhileRunning();

    // ⚠️ 下面三条必须放在最后：它们会临时把托盘窗口类名劫持成自己的窗口过程，
    //    虽然每条用完都立刻注销，但放在最后可以确保它们不会影响上面任何一条。
    testDestructAfterFailedCreate();
    testNormalPathAfterHijack();
    testDestroyWithForeignWindowProc();
    testNormalPathAfterHijack();

    std::printf("通过 %d 项，失败 %d 项，跳过 %d 项\n", g_passed, g_failed, g_skipped);
    if (g_failed == 0) {
        std::printf("=== 全部通过 ===\n");
        return 0;
    }
    std::printf("=== 存在失败 ===\n");
    return 1;
}
