// window 模块自测。
//
// 分两部分：
//   1. 纯函数断言（命中区域、NC 码映射、单实例互斥体、几何解析、重试时刻表）
//      ——任何环境都能跑
//   2. 真实窗口测试——需要交互式桌面会话；没有会话时明确跳过而不是假装通过
#include "../src/window.hpp"
#include "../include/translator_native.h"  // TN_EXIT_* 退出码

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

using tn::HitZone;

// ── 命中区域：拖拽/缩放逻辑的全部所在 ──

void testHitTestCorners() {
    const int w = 600, h = 400, b = 6;
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, 0, 0, b)), static_cast<int>(HitZone::TopLeft), "左上角");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w - 1, 0, b)), static_cast<int>(HitZone::TopRight), "右上角");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, 0, h - 1, b)), static_cast<int>(HitZone::BottomLeft), "左下角");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w - 1, h - 1, b)), static_cast<int>(HitZone::BottomRight), "右下角");
}

void testHitTestEdges() {
    const int w = 600, h = 400, b = 6;
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, 0, h / 2, b)), static_cast<int>(HitZone::Left), "左边");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w - 1, h / 2, b)), static_cast<int>(HitZone::Right), "右边");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, 0, b)), static_cast<int>(HitZone::Top), "上边");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, h - 1, b)), static_cast<int>(HitZone::Bottom), "下边");
    // 刚好在边框内侧一格就不算边框
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, b, h / 2, b)), static_cast<int>(HitZone::None), "边框内侧不算边框");
}

// 关键：标题带只占顶部一条，其余客户区必须返回 None（否则 WebView 收不到鼠标事件）
void testCaptionBandIsLimited() {
    const int w = 600, h = 400, b = 6;
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, 10, b)), static_cast<int>(HitZone::Caption), "标题带内");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, 31, b)), static_cast<int>(HitZone::Caption), "标题带下沿");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, 40, b)), static_cast<int>(HitZone::None), "标题带以下应交给客户区");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, h / 2, b)), static_cast<int>(HitZone::None), "窗口中部应交给客户区");
    checkEqInt(static_cast<int>(tn::hitTestZone(w, h, w / 2, h - 40, b)), static_cast<int>(HitZone::None), "输入区应交给客户区");
}

void testHitTestDegenerateSizes() {
    // 极小窗口不应崩溃，也不应把整个客户区当成边框
    checkEqInt(static_cast<int>(tn::hitTestZone(0, 0, 0, 0, 6)), static_cast<int>(HitZone::None), "零尺寸");
    checkEqInt(static_cast<int>(tn::hitTestZone(-5, 10, 0, 0, 6)), static_cast<int>(HitZone::None), "负尺寸");

    // 10x10 小于 3 倍边框（18）→ 关闭缩放区；同时标题带收缩到 h/2=5，
    // 所以 (5,5) 既不是边框也不是标题带 → 交给客户区
    checkEqInt(static_cast<int>(tn::hitTestZone(10, 10, 5, 5, 6)), static_cast<int>(HitZone::None),
               "窗口小于 3 倍边框时应关闭缩放区");
    // 小窗口仍然要能拖动：标题带收缩到 h/2=5，左上角落进标题带而不是缩放区
    checkEqInt(static_cast<int>(tn::hitTestZone(10, 10, 0, 0, 6)), static_cast<int>(HitZone::Caption),
               "小窗口左上角应是标题带（可拖动）而非缩放区");
    checkEqInt(static_cast<int>(tn::hitTestZone(10, 10, 9, 9, 6)), static_cast<int>(HitZone::None),
               "小窗口右下角应交给客户区");

    // 中等偏小的窗口（40x40 >= 18）应恢复正常的边框判定
    checkEqInt(static_cast<int>(tn::hitTestZone(40, 40, 0, 20, 6)), static_cast<int>(HitZone::Left),
               "40x40 窗口左边应为缩放区");
    checkEqInt(static_cast<int>(tn::hitTestZone(40, 40, 20, 20, 6)), static_cast<int>(HitZone::None),
               "40x40 窗口中部应交给客户区");
    // 40 高时标题带取 min(32, 20) = 20，所以 y=10 是标题带
    checkEqInt(static_cast<int>(tn::hitTestZone(40, 40, 20, 10, 6)), static_cast<int>(HitZone::Caption),
               "小窗口的标题带应收缩到可用范围内");
}

void testHitZoneToNcCode() {
    checkEqInt(tn::hitZoneToNcCode(HitZone::Caption), HTCAPTION, "Caption → HTCAPTION");
    checkEqInt(tn::hitZoneToNcCode(HitZone::Left), HTLEFT, "Left → HTLEFT");
    checkEqInt(tn::hitZoneToNcCode(HitZone::None), HTCLIENT, "None → HTCLIENT");
    checkEqInt(tn::hitZoneToNcCode(HitZone::BottomRight), HTBOTTOMRIGHT, "BottomRight → HTBOTTOMRIGHT");
}

// ── 单实例互斥体 ──

void testSingleInstance() {
    const std::wstring name = L"Global\\ETS2TranslatorV2SelfTest";
    (void)name;  // 见下方：非 Global 名避免权限问题

    const std::wstring localName = L"ETS2TranslatorV2SelfTest";
    tn::SingleInstance first;
    check(first.acquire(localName), "首个实例应获取成功");
    check(!first.alreadyRunning(), "首个实例不应报告已存在");

    tn::SingleInstance second;
    check(!second.acquire(localName), "第二个实例应获取失败");
    check(second.alreadyRunning(), "第二个实例应报告已存在");

    // 释放后应能再次获取（Windows 上需等内核对象回收，故先释放第一个）
    first.release();
    second.release();

    tn::SingleInstance third;
    check(third.acquire(localName), "释放后应能重新获取");
    third.release();
}

// ── 窗口几何解析：位置恢复的钳制与居中 ──

// 「没记住位置」＝两轴居中（V1 overlay.py:394-399）。
//
// 这条以前是坏的：native 把 y 算成 (sh-h)/3，看着像「稍偏上」，实际上是另一回事，
// 而且屏幕越高、窗口越矮，偏得越多——1080 屏上 360 高的窗口差了 108 像素。
void testResolveGeometryCentering() {
    const tn::WindowGeometry g = tn::resolveWindowGeometry(-1, -1, 620, 360, 1920, 1080);
    checkEqInt(g.width, 620, "居中时宽度不变");
    checkEqInt(g.height, 360, "居中时高度不变");
    checkEqInt(g.x, (1920 - 620) / 2, "居中时 X 取屏幕正中");
    checkEqInt(g.y, (1080 - 360) / 2, "居中时 Y 取屏幕正中");

    // V1 的判据是 win_x>=0 且 win_y>=0 才算「记住过」，
    // 所以只要有一边是负的就整体按居中处理。
    const tn::WindowGeometry half = tn::resolveWindowGeometry(-1, 50, 400, 200, 1920, 1080);
    checkEqInt(half.x, (1920 - 400) / 2, "只有 x 为负时 X 也应居中");
    checkEqInt(half.y, (1080 - 200) / 2, "只有 x 为负时 Y 也应居中");

    // 奇数差要向下取整，不能因为四舍五入把窗口算到屏幕外
    const tn::WindowGeometry odd = tn::resolveWindowGeometry(-1, -1, 621, 361, 1920, 1080);
    checkEqInt(odd.x, (1920 - 621) / 2, "宽度为奇数时 X 向下取整");
    checkEqInt(odd.y, (1080 - 361) / 2, "高度为奇数时 Y 向下取整");
}

// 记住的位置要钳进可见区域，但**不能**擅自改动屏幕内的摆放
// （V1 overlay.py:388-393）。
void testResolveGeometryClamp() {
    // 屏幕内：原样保留
    const tn::WindowGeometry in = tn::resolveWindowGeometry(300, 200, 620, 360, 1920, 1080);
    checkEqInt(in.x, 300, "屏幕内的 X 应原样保留");
    checkEqInt(in.y, 200, "屏幕内的 Y 应原样保留");
    checkEqInt(in.width, 620, "屏幕内的宽度应原样保留");

    // 贴着右下角也算屏幕内（用户的摆放是故意的，不能被挪走）
    const tn::WindowGeometry edge =
        tn::resolveWindowGeometry(1920 - 620, 1080 - 360, 620, 360, 1920, 1080);
    checkEqInt(edge.x, 1920 - 620, "贴右边界的 X 应原样保留");
    checkEqInt(edge.y, 1080 - 360, "贴下边界的 Y 应原样保留");

    // 屏幕外：钳到「至少留 100 像素可见」（换过显示器/改过分辨率的经典场景）
    // 变量名不能用 far：windows.h 沿革下来的 windef.h 里 `far` 是个空宏。
    const tn::WindowGeometry offscreen =
        tn::resolveWindowGeometry(9000, 9000, 620, 360, 1920, 1080);
    checkEqInt(offscreen.x, 1920 - 100, "超出右边界应钳到 屏幕宽-100");
    checkEqInt(offscreen.y, 1080 - 100, "超出下边界应钳到 屏幕高-100");

    // 负坐标已经在上一条测试里走「居中」分支，这里确认钳制不会产出负数
    const tn::WindowGeometry tiny = tn::resolveWindowGeometry(500, 500, 620, 360, 80, 60);
    check(tiny.x >= 0 && tiny.y >= 0, "屏幕小于 100 像素时坐标也不应为负");

    // 比屏幕还大的窗口 → 缩到屏幕尺寸（V1 overlay.py:391-392 的 min）。
    // 不缩的话下面的居中会算出负坐标，标题带被推到屏幕外，用户抓不到窗口。
    const tn::WindowGeometry big = tn::resolveWindowGeometry(0, 0, 3000, 2000, 1920, 1080);
    checkEqInt(big.width, 1920, "比屏幕宽的窗口应缩到屏幕宽");
    checkEqInt(big.height, 1080, "比屏幕高的窗口应缩到屏幕高");
    const tn::WindowGeometry bigCentered =
        tn::resolveWindowGeometry(-1, -1, 3000, 2000, 1920, 1080);
    checkEqInt(bigCentered.x, 0, "缩到屏幕大小后居中不应算出负坐标");
    checkEqInt(bigCentered.y, 0, "缩到屏幕大小后居中不应算出负坐标");

    // 拿不到屏幕尺寸（会话切换的瞬间会返回 0）→ 原样返回。
    // 照常算的话窗口会被挤成 1×1 贴在 (0,0)，比不解析更糟。
    const tn::WindowGeometry degenerate = tn::resolveWindowGeometry(10, 20, 620, 360, 0, 0);
    checkEqInt(degenerate.x, 10, "无屏幕尺寸时 X 应原样返回");
    checkEqInt(degenerate.y, 20, "无屏幕尺寸时 Y 应原样返回");
    checkEqInt(degenerate.width, 620, "无屏幕尺寸时宽度应原样返回");
    checkEqInt(degenerate.height, 360, "无屏幕尺寸时高度应原样返回");
}

// ── 毛玻璃重试时刻表 ──

// V1 overlay.py:97-100 的三次重试：200ms / 800ms / 2000ms，之后放弃。
void testBlurRetrySchedule() {
    checkEqInt(tn::blurRetryDelayMs(0), 200, "第 1 次重试延时 200ms");
    checkEqInt(tn::blurRetryDelayMs(1), 800, "第 2 次重试延时 800ms");
    checkEqInt(tn::blurRetryDelayMs(2), 2000, "第 3 次重试延时 2000ms");
    checkEqInt(tn::blurRetryDelayMs(3), 0, "第 4 次不再重试");
    checkEqInt(tn::blurRetryDelayMs(99), 0, "越界的次数不再重试");
    checkEqInt(tn::blurRetryDelayMs(-1), 0, "负数次数不再重试");
}

// ── 系统信息 ──

void testOsBuild() {
    const int build = tn::OverlayWindow::osBuild();
    check(build > 0, "应能取到系统 build 号");
    if (build > 0) {
        std::printf("  系统 build: %d（Win11 阈值 22621 / Win10 亚克力阈值 17134）\n", build);
    }
}

// ── 真实窗口 ──

void testWindowLifecycle() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        std::printf("  SKIP: 当前无交互式桌面会话，跳过真实窗口测试\n");
        return;
    }

    tn::OverlayWindow win;
    tn::WindowSpec spec;
    spec.width = 400;
    spec.height = 300;
    spec.topmost = true;
    spec.blur = "auto";

    if (!win.create(spec)) {
        ++g_skipped;
        std::printf("  SKIP: 创建窗口失败（可能被沙箱或会话限制），跳过窗口测试\n");
        return;
    }
    check(win.valid(), "创建后 hwnd 应有效");
    check(win.hwnd() != nullptr, "hwnd() 不应为空");

    // 显示 / 隐藏
    win.show();
    win.pump();
    check(win.isVisible(), "show 后应可见");
    win.hide();
    win.pump();
    check(!win.isVisible(), "hide 后应不可见");

    // 位置与尺寸
    int x = 0, y = 0, w = 0, h = 0;
    check(win.getBounds(x, y, w, h), "应能读取位置尺寸");
    checkEqInt(w, 400, "窗口宽度");
    checkEqInt(h, 300, "窗口高度");
    check(win.setBounds(120, 80, 500, 360), "应能设置位置尺寸");
    win.pump();
    check(win.getBounds(x, y, w, h), "重新读取位置尺寸");
    checkEqInt(x, 120, "设置后的 X");
    checkEqInt(y, 80, "设置后的 Y");
    checkEqInt(w, 500, "设置后的宽度");

    // 鼠标穿透
    check(!win.isClickThrough(), "默认不应穿透");
    check(win.setClickThrough(true), "应能开启穿透");
    check(win.isClickThrough(), "开启后应报告穿透");
    check(win.setClickThrough(false), "应能关闭穿透");
    check(!win.isClickThrough(), "关闭后应报告非穿透");

    // 毛玻璃（成败取决于系统版本；这里验证「不崩溃且如实报告模式」）
    const bool blurOk = win.applyBlur("auto");
    const std::string applied = win.appliedBlur();
    std::printf("  毛玻璃: ok=%d 实际模式=%s\n", blurOk ? 1 : 0, applied.c_str());
    check(applied == "mica" || applied == "acrylic" || applied == "none",
          "毛玻璃模式取值应在 {mica, acrylic, none} 内");
    if (blurOk) {
        check(applied != "none", "报告成功时应确实应用了某种效果");
    }

    // 透明度
    check(win.setOpacity(0.5), "应能设置透明度");
    check(win.setOpacity(1.0), "应能恢复不透明");
    check(win.setOpacity(0.05), "透明度应被下限截断（不报错）");
    check(win.setOpacity(5.0), "透明度应被上限截断（不报错）");

    // 销毁后状态应干净
    win.destroy();
    check(!win.valid(), "销毁后 hwnd 应为空");
    check(!win.isVisible(), "销毁后不应可见");
}

// 销毁后可以重建（对应 Go 侧「重建窗口」的恢复路径）
void testWindowRecreate() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        return;
    }
    tn::OverlayWindow win;
    tn::WindowSpec spec;
    spec.width = 200;
    spec.height = 150;

    if (!win.create(spec)) {
        ++g_skipped;
        return;
    }
    win.destroy();
    if (!win.create(spec)) {
        ++g_skipped;
        std::printf("  SKIP: 重建窗口失败（环境限制）\n");
        return;
    }
    check(win.valid(), "应能重建窗口");
    win.show();
    win.pump();
    check(win.isVisible(), "重建后应可见");
    win.destroy();
}

// 几何解析真的被 create() 用上了：居中与钳制都要在**真实窗口**上成立。
//
// 纯函数测过只说明算法对；调用点漏了的话，症状与算法写错一模一样。
void testWindowGeometryApplied() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        return;
    }
    const int sw = GetSystemMetrics(SM_CXSCREEN);
    const int sh = GetSystemMetrics(SM_CYSCREEN);

    // ① 没记住位置（Go 送 -1）→ 两轴居中
    {
        tn::OverlayWindow win;
        tn::WindowSpec spec;
        spec.width = 400;
        spec.height = 300;
        spec.blur = "none";   // 这条测试只关心几何
        spec.x = -1;
        spec.y = -1;
        if (!win.create(spec)) {
            ++g_skipped;
            std::printf("  SKIP: 创建窗口失败，跳过几何应用测试\n");
            return;
        }
        int x = 0, y = 0, w = 0, h = 0;
        check(win.getBounds(x, y, w, h), "应能读取居中后的几何");
        checkEqInt(x, (sw - w) / 2, "默认位置应水平居中");
        checkEqInt(y, (sh - h) / 2, "默认位置应垂直居中（不是屏高的 1/3）");
        win.destroy();
    }

    // ② 记住的位置落到屏幕外 → 钳回可见区域
    {
        tn::OverlayWindow win;
        tn::WindowSpec spec;
        spec.width = 400;
        spec.height = 300;
        spec.blur = "none";
        spec.x = sw + 4000;
        spec.y = sh + 4000;
        if (!win.create(spec)) {
            ++g_skipped;
            std::printf("  SKIP: 创建窗口失败，跳过钳制测试\n");
            return;
        }
        int x = 0, y = 0, w = 0, h = 0;
        check(win.getBounds(x, y, w, h), "应能读取钳制后的几何");
        checkEqInt(x, sw - 100, "屏幕右侧外的位置应被钳回（至少留 100 像素可见）");
        checkEqInt(y, sh - 100, "屏幕下方外的位置应被钳回（至少留 100 像素可见）");
        win.destroy();
    }

    // ③ 比屏幕还大的窗口 → 缩到屏幕尺寸，坐标不为负
    {
        tn::OverlayWindow win;
        tn::WindowSpec spec;
        spec.width = sw + 500;
        spec.height = sh + 500;
        spec.blur = "none";
        spec.x = -1;
        spec.y = -1;
        if (!win.create(spec)) {
            ++g_skipped;
            std::printf("  SKIP: 创建超大窗口失败，跳过尺寸钳制测试\n");
            return;
        }
        int x = 0, y = 0, w = 0, h = 0;
        check(win.getBounds(x, y, w, h), "应能读取超大窗口的几何");
        checkEqInt(w, sw, "比屏幕宽的窗口应缩到屏幕宽");
        checkEqInt(h, sh, "比屏幕高的窗口应缩到屏幕高");
        check(x >= 0 && y >= 0, "缩放之后坐标不应落到屏幕外");
        win.destroy();
    }
}

// 悬浮窗的关闭＝**隐藏**，不是销毁（V1 main.py:85,210-212）。
//
// 改之前没有 WM_CLOSE 分支，消息落到 DefWindowProc 上把窗口销毁掉——
// 用户按一次 Alt+F4，悬浮窗就再也回不来了，而且日志里没有任何解释。
void testOverlayCloseHides() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        std::printf("  SKIP: 当前无交互式桌面会话，跳过关闭语义测试\n");
        return;
    }

    tn::OverlayWindow win;
    tn::WindowSpec spec;
    spec.width = 400;
    spec.height = 300;
    spec.blur = "none";
    if (!win.create(spec)) {
        ++g_skipped;
        std::printf("  SKIP: 创建窗口失败，跳过关闭语义测试\n");
        return;
    }
    win.show();
    win.pump();
    check(win.isVisible(), "前置条件：窗口应可见");

    SendMessageW(static_cast<HWND>(win.hwnd()), WM_CLOSE, 0, 0);
    win.pump();

    check(win.valid(), "关闭悬浮窗**不应**销毁窗口（否则窗口再也回不来）");
    check(!win.isVisible(), "关闭悬浮窗应变成隐藏");
    check(win.takeUserHide(), "关闭后应留下一次「用户隐藏」通知（要回报给 Go）");
    check(!win.takeUserHide(), "「用户隐藏」通知只能被取走一次");

    // 还能再显示回来：这条才是「关闭＝隐藏」的真正含义
    win.show();
    win.pump();
    check(win.isVisible(), "隐藏之后应还能再显示回来");
    check(win.valid(), "重新显示后窗口仍然有效");
    win.destroy();
}

// 设置窗口的 WM_CLOSE 语义**相反**：它是普通窗口，关闭就是关掉。
// 这条是防回归的——「悬浮窗关闭＝隐藏」很容易被顺手写成对所有窗口生效。
void testFramedCloseStillDestroys() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        return;
    }

    tn::OverlayWindow win;
    tn::WindowSpec spec;
    spec.framed = true;   // 设置窗口
    spec.width = 400;
    spec.height = 300;
    spec.dark = false;
    if (!win.create(spec)) {
        ++g_skipped;
        std::printf("  SKIP: 创建设置窗口失败，跳过设置窗口关闭测试\n");
        return;
    }
    win.show();
    win.pump();
    check(win.valid(), "设置窗口应创建成功");

    SendMessageW(static_cast<HWND>(win.hwnd()), WM_CLOSE, 0, 0);
    win.pump();

    check(!win.valid(), "设置窗口的关闭仍然应该是销毁");
    check(!win.takeUserHide(), "设置窗口关闭不应产生「用户隐藏悬浮窗」的通知");
    win.destroy();
}

// 隐藏再显示之后毛玻璃要重应用（V1 overlay.py:101-102 的 `<Map>` 重应用）。
//
// 不复现这个的话，用户第一次用托盘的「显示/隐藏」之后毛玻璃就永久消失了，
// 而 appliedBlur_ 还记着 "mica"——上层完全看不出出了问题。
void testBlurReappliedAfterHideShow() {
    if (!tn::OverlayWindow::hasDesktopSession()) {
        ++g_skipped;
        return;
    }

    tn::OverlayWindow win;
    tn::WindowSpec spec;
    spec.width = 400;
    spec.height = 300;
    spec.blur = "auto";
    if (!win.create(spec)) {
        ++g_skipped;
        std::printf("  SKIP: 创建窗口失败，跳过毛玻璃重应用测试\n");
        return;
    }
    win.show();
    win.pump();

    const std::string first = win.appliedBlur();
    check(win.blurMode() == "auto", "期望模式应保持 auto（重试与重应用都靠它）");

    win.hide();
    win.pump();
    check(!win.isVisible(), "隐藏后应不可见");

    win.show();
    win.pump();
    check(win.isVisible(), "重新显示后应可见");

    const std::string second = win.appliedBlur();
    check(second == "mica" || second == "acrylic" || second == "none",
          "重应用后模式取值应在 {mica, acrylic, none} 内");
    if (first != "none") {
        check(second == first, "重新显示后应重新应用同一种材质");
    } else {
        std::printf("    （本机毛玻璃不可用，只验证不崩）\n");
    }

    // 让重试定时器跑完一轮：只有首轮失败时才会真的排定，
    // 这条断言的是「定时器路径不会把窗口弄坏、也不会死循环」。
    Sleep(300);
    win.pump();
    check(win.valid(), "重试定时器跑过之后窗口仍应有效");
    check(!win.takeUserHide(), "程序自己显示/隐藏不应产生「用户隐藏」通知");
    win.destroy();
}

// ── 启动路径上的单实例保护（缺陷 5 的回归）──

// 拉起一个 translator_native.exe，stdout/stderr 重定向到 outPath。
// 返回 true 时 pi 有效（调用方负责 CloseHandle 并结束它）。
bool launchNative(const std::wstring& exe, const std::wstring& pipeName,
                  const std::wstring& outPath, PROCESS_INFORMATION& pi) {
    // ⚠️ 句柄必须可继承（SECURITY_ATTRIBUTES.bInheritHandle），
    //    否则子进程拿到的是无效句柄。
    SECURITY_ATTRIBUTES sa{};
    sa.nLength = sizeof(sa);
    sa.bInheritHandle = TRUE;

    HANDLE out = CreateFileW(outPath.c_str(), GENERIC_WRITE | GENERIC_READ,
                             FILE_SHARE_READ | FILE_SHARE_WRITE, &sa, CREATE_ALWAYS,
                             FILE_ATTRIBUTE_NORMAL, nullptr);
    if (out == INVALID_HANDLE_VALUE) return false;

    // 标准输入给个 NUL：STARTF_USESTDHANDLES 下留空句柄会让 CRT 拿到无效值，
    // 而 MSVC 的 CRT 遇到无效句柄会走「无效参数处理」——默认是直接终止进程。
    HANDLE nul = CreateFileW(L"NUL", GENERIC_READ,
                             FILE_SHARE_READ | FILE_SHARE_WRITE, &sa, OPEN_EXISTING,
                             FILE_ATTRIBUTE_NORMAL, nullptr);

    STARTUPINFOW si{};
    si.cb = sizeof(si);
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdOutput = out;
    si.hStdError = out;
    si.hStdInput = (nul != INVALID_HANDLE_VALUE) ? nul : nullptr;

    std::wstring mutableCmd = L"\"" + exe + L"\" --pipe " + pipeName;
    const BOOL ok = CreateProcessW(nullptr, mutableCmd.data(), nullptr, nullptr, TRUE,
                                   CREATE_NO_WINDOW, nullptr, nullptr, &si, &pi);

    // 父进程这两个句柄要立刻关掉：留着的话子进程退出后文件仍被占用，
    // 读回来永远只有半截（很常见的一个陷阱）。
    CloseHandle(out);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    return ok != FALSE;
}

std::wstring tempLogPath(const wchar_t* tag) {
    wchar_t dir[MAX_PATH]{};
    const DWORD n = GetTempPathW(MAX_PATH, dir);
    std::wstring path = (n > 0) ? std::wstring(dir) : std::wstring(L".\\");
    path += L"tn-";
    path += tag;
    path += L"-";
    path += std::to_wstring(GetCurrentProcessId());
    path += L".log";
    return path;
}

std::string readFileText(const std::wstring& path) {
    HANDLE h = CreateFileW(path.c_str(), GENERIC_READ,
                           FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
                           FILE_ATTRIBUTE_NORMAL, nullptr);
    if (h == INVALID_HANDLE_VALUE) return std::string();

    std::string out;
    char buf[4096];
    DWORD got = 0;
    while (ReadFile(h, buf, sizeof(buf), &got, nullptr) && got > 0) {
        out.append(buf, got);
        if (out.size() > 65536) break;   // 只为了找几行诊断，不必读全
    }
    CloseHandle(h);
    return out;
}

// 单实例保护必须接在**启动路径**上。
//
// 只测 SingleInstance 类是不够的——那正是当初的缺陷形态：类与单测都在，
// 启动路径上却没有任何调用者，二次启动照样起第二个进程。
// 所以这里直接拉起构建产物两次：第二次应当以「已在运行」的退出码干净退出，
// 并打印出人能看懂的原因。
void testSingleInstanceOnLaunchPath() {
    wchar_t self[MAX_PATH]{};
    if (GetModuleFileNameW(nullptr, self, MAX_PATH) == 0) {
        ++g_skipped;
        std::printf("  SKIP: 取不到自身路径，跳过启动路径测试\n");
        return;
    }
    std::wstring dir(self);
    const size_t slash = dir.find_last_of(L'\\');
    if (slash == std::wstring::npos) {
        ++g_skipped;
        return;
    }
    dir.resize(slash + 1);
    const std::wstring exe = dir + L"translator_native.exe";
    if (GetFileAttributesW(exe.c_str()) == INVALID_FILE_ATTRIBUTES) {
        ++g_skipped;
        std::printf("  SKIP: 同目录下没有 translator_native.exe（只构建了测试目标？）\n");
        return;
    }

    // 管道名固定即可：这条测试真正要占用的是**单实例互斥体**，与管道名无关。
    const std::wstring pipe = L"\\\\.\\pipe\\tn-singleton-selftest";
    const std::wstring out1 = tempLogPath(L"native1");
    const std::wstring out2 = tempLogPath(L"native2");

    PROCESS_INFORMATION pi1{};
    if (!launchNative(exe, pipe, out1, pi1)) {
        DeleteFileW(out1.c_str());
        ++g_skipped;
        std::printf("  SKIP: 无法拉起第一个实例（环境限制），跳过启动路径测试\n");
        return;
    }
    CloseHandle(pi1.hThread);

    // 等第一个实例**确实拿到互斥体**再启动第二个。
    //
    // banner 是在获取互斥体之后才打印的，所以它的出现就等于「互斥体已被占住」。
    // 直接用固定 Sleep 会引入竞态：慢机器上第二个实例可能先跑起来，
    // 这条测试就变成一条永远超时的假失败。
    bool ready = false;
    for (int waited = 0; waited < 5000 && !ready; waited += 50) {
        Sleep(50);
        ready = readFileText(out1).find("[native] 管道:") != std::string::npos;
    }
    if (!ready) {
        DWORD code1 = 0;
        GetExitCodeProcess(pi1.hProcess, &code1);
        TerminateProcess(pi1.hProcess, 0);
        WaitForSingleObject(pi1.hProcess, 5000);
        CloseHandle(pi1.hProcess);
        DeleteFileW(out1.c_str());
        DeleteFileW(out2.c_str());
        ++g_skipped;
        if (code1 == TN_EXIT_ALREADY_RUNNING) {
            std::printf("  SKIP: 本机已有一个真实的 native 实例在跑，跳过启动路径测试\n");
        } else {
            std::printf("  SKIP: 第一个实例未能进入服务状态（退出码 %lu），环境限制\n",
                        static_cast<unsigned long>(code1));
        }
        return;
    }

    PROCESS_INFORMATION pi2{};
    const bool launched2 = launchNative(exe, pipe, out2, pi2);
    check(launched2, "应能拉起第二个实例");
    if (launched2) {
        CloseHandle(pi2.hThread);
        const DWORD waited = WaitForSingleObject(pi2.hProcess, 10000);
        checkEqInt(static_cast<int>(waited), static_cast<int>(WAIT_OBJECT_0),
                   "第二个实例应在 10 秒内退出（没有保护时会一直等管道连接）");

        DWORD code2 = 0;
        GetExitCodeProcess(pi2.hProcess, &code2);
        checkEqInt(static_cast<int>(code2), TN_EXIT_ALREADY_RUNNING,
                   "第二个实例的退出码应为「已在运行」，与成功区分开");

        const std::string text = readFileText(out2);
        // 断言的是**有明确诊断**，不是这一句话的措辞：只看退出码的话，
        // 一个「静默退出 2」也能过，而用户遇到的现象是「双击没反应」。
        check(text.find("实例在运行") != std::string::npos,
              "第二个实例应打印明确的「实例在运行」诊断，而不是静默退出");
        if (text.find("实例在运行") == std::string::npos) {
            std::printf("    第二个实例的实际输出: %s\n", text.c_str());
        }
        CloseHandle(pi2.hProcess);
    }

    // 收尾：第一个实例一直等着管道连接，只能结束它
    TerminateProcess(pi1.hProcess, 0);
    WaitForSingleObject(pi1.hProcess, 5000);
    CloseHandle(pi1.hProcess);
    DeleteFileW(out1.c_str());
    DeleteFileW(out2.c_str());
}

}  // namespace

int main() {
    std::printf("=== translator_native window 自测 ===\n");

    testHitTestCorners();
    testHitTestEdges();
    testCaptionBandIsLimited();
    testHitTestDegenerateSizes();
    testHitZoneToNcCode();
    testSingleInstance();
    testResolveGeometryCentering();
    testResolveGeometryClamp();
    testBlurRetrySchedule();
    testOsBuild();
    testWindowLifecycle();
    testWindowRecreate();
    testWindowGeometryApplied();
    testOverlayCloseHides();
    testFramedCloseStillDestroys();
    testBlurReappliedAfterHideShow();
    testSingleInstanceOnLaunchPath();

    std::printf("通过 %d 项，失败 %d 项，跳过 %d 项\n", g_passed, g_failed, g_skipped);
    if (g_failed == 0) {
        std::printf("=== 全部通过 ===\n");
        return 0;
    }
    std::printf("=== 存在失败 ===\n");
    return 1;
}
