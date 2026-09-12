// 无边框悬浮窗实现（Windows）。
#include "window.hpp"

#include <windows.h>
#include <dwmapi.h>

#include <cstdio>

#pragma comment(lib, "dwmapi.lib")

namespace tn {

namespace {

// 拖拽用的标题带高度。**只在这条带子里返回 HTCAPTION**——
// 若整个客户区都返回 HTCAPTION，嵌在窗口里的 WebView 就收不到鼠标事件了。
// 这是与 V1（tkinter 时代在消息区按下即可拖动）的有意差异。
constexpr int kCaptionHeight = 32;

// 边缘缩放判定宽度
constexpr int kResizeBorder = 6;

constexpr wchar_t kWindowClass[] = L"ETS2TranslatorOverlayV2";

// 设置窗口用**另一个类名**，虽然共用同一个窗口过程。
// 分开是为了让外部工具（tools/inspect-overlay-window.ps1 之类按类名找窗口）
// 不会因为两个同类窗口而抓到错的那个。
constexpr wchar_t kFramedWindowClass[] = L"ETS2TranslatorSettingsV2";

// Win11 22621+ 才有 DWMWA_SYSTEMBACKDROP_TYPE
constexpr int kWin11Backdrop = 22621;
// Win10 17134+ 才有 ACCENT_ENABLE_ACRYLICBLURBEHIND
constexpr int kWin10Acrylic = 17134;

// 毛玻璃重试定时器 id。用 SetTimer 而不是自己记时间戳轮询：
// 主循环那个 10ms 的心跳节拍不该被「200ms 后重试」这种一次性延迟牵着走，
// 而窗口定时器天然跟着窗口销毁，不需要额外维护生命周期。
constexpr UINT_PTR kBlurRetryTimerId = 1;

constexpr DWORD DWMWA_SYSTEMBACKDROP_TYPE_ = 38;
constexpr DWORD DWMWA_USE_IMMERSIVE_DARK_MODE_ = 20;

constexpr int DWMSBT_NONE = 1;
constexpr int DWMSBT_MAINWINDOW = 2;       // Mica
constexpr int DWMSBT_TRANSIENTWINDOW = 3;  // Acrylic

constexpr int ACCENT_DISABLED = 0;
constexpr int ACCENT_ENABLE_ACRYLICBLURBEHIND = 4;
constexpr int WCA_ACCENT_POLICY = 19;

struct AccentPolicy {
    int accentState;
    int accentFlags;
    DWORD gradientColor;  // 0xAABBGGRR
    int animationId;
};

struct WindowCompositionAttributeData {
    int attribute;
    void* data;
    size_t sizeOfData;
};

using SetWindowCompositionAttributeFn =
    BOOL(WINAPI*)(HWND, WindowCompositionAttributeData*);

HWND asHwnd(void* p) { return reinterpret_cast<HWND>(p); }
void* asVoid(HWND h) { return reinterpret_cast<void*>(h); }

// ── 毛玻璃：两条路径 + 优雅降级（V1 acrylic_helper 的等价实现）──

bool applyDwmBackdrop(HWND hwnd, int backdropType, bool dark) {
    HMODULE dwm = LoadLibraryW(L"dwmapi.dll");
    if (!dwm) return false;

    using DwmSetWindowAttributeFn = HRESULT(WINAPI*)(HWND, DWORD, LPCVOID, DWORD);
    using DwmExtendFrameIntoClientAreaFn = HRESULT(WINAPI*)(HWND, const MARGINS*);

    auto setAttr = reinterpret_cast<DwmSetWindowAttributeFn>(
        GetProcAddress(dwm, "DwmSetWindowAttribute"));
    auto extendFrame = reinterpret_cast<DwmExtendFrameIntoClientAreaFn>(
        GetProcAddress(dwm, "DwmExtendFrameIntoClientArea"));
    if (!setAttr) {
        FreeLibrary(dwm);
        return false;
    }

    const int value = backdropType;
    const HRESULT hr = setAttr(hwnd, DWMWA_SYSTEMBACKDROP_TYPE_,
                               &value, sizeof(value));
    // 把 DWM 边框扩展到整个客户区，否则背景只覆盖非客户区
    if (extendFrame) {
        MARGINS margins{-1, -1, -1, -1};
        extendFrame(hwnd, &margins);
    }
    // 深色模式（属性 20 在 Win10 1809+ 可用；失败不影响主效果）。
    // 必须跟随主题：Mica/Acrylic 的深浅由它决定，设成深色再铺浅色内容
    // 就会出现「浅色界面 + 深色材质」的错配。
    const int darkValue = dark ? 1 : 0;
    setAttr(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE_, &darkValue, sizeof(darkValue));

    FreeLibrary(dwm);
    return SUCCEEDED(hr);
}

bool applyAccentAcrylic(HWND hwnd, int accentState, DWORD gradientColor) {
    HMODULE user32 = LoadLibraryW(L"user32.dll");
    if (!user32) return false;

    auto setComposition = reinterpret_cast<SetWindowCompositionAttributeFn>(
        GetProcAddress(user32, "SetWindowCompositionAttribute"));
    if (!setComposition) {
        FreeLibrary(user32);
        return false;
    }

    AccentPolicy policy{};
    policy.accentState = accentState;
    policy.accentFlags = 2;  // 绘制全部边框
    policy.gradientColor = gradientColor;
    policy.animationId = 0;

    WindowCompositionAttributeData data{};
    data.attribute = WCA_ACCENT_POLICY;
    data.data = &policy;
    data.sizeOfData = sizeof(policy);

    const BOOL ok = setComposition(hwnd, &data);
    FreeLibrary(user32);
    return ok != FALSE;
}

}  // namespace

// ── 纯函数：命中区域 ──

HitZone hitTestZone(int width, int height, int x, int y, int border) {
    if (width <= 0 || height <= 0) return HitZone::None;

    // 窗口很小时收缩边框与标题带，否则整个窗口都会落进缩放区，
    // 内容彻底点不到（用户把窗口拖到很小就会遇到）。
    const int bx = (width >= border * 3) ? border : 0;
    const int by = (height >= border * 3) ? border : 0;
    int captionH = (kCaptionHeight < height / 2) ? kCaptionHeight : height / 2;
    if (captionH < 0) captionH = 0;

    const bool left = bx > 0 && x < bx;
    const bool right = bx > 0 && x >= width - bx;
    const bool top = by > 0 && y < by;
    const bool bottom = by > 0 && y >= height - by;

    if (top && left) return HitZone::TopLeft;
    if (top && right) return HitZone::TopRight;
    if (bottom && left) return HitZone::BottomLeft;
    if (bottom && right) return HitZone::BottomRight;
    if (left) return HitZone::Left;
    if (right) return HitZone::Right;
    if (top) return HitZone::Top;
    if (bottom) return HitZone::Bottom;

    // 标题带之外一律返回 None → 交给客户区（WebView 需要收到鼠标事件）
    if (y < captionH) return HitZone::Caption;
    return HitZone::None;
}

int hitZoneToNcCode(HitZone zone) {
    switch (zone) {
        case HitZone::Caption:     return HTCAPTION;
        case HitZone::Left:        return HTLEFT;
        case HitZone::Right:       return HTRIGHT;
        case HitZone::Top:         return HTTOP;
        case HitZone::Bottom:      return HTBOTTOM;
        case HitZone::TopLeft:     return HTTOPLEFT;
        case HitZone::TopRight:    return HTTOPRIGHT;
        case HitZone::BottomLeft:  return HTBOTTOMLEFT;
        case HitZone::BottomRight: return HTBOTTOMRIGHT;
        case HitZone::None:        break;
    }
    return HTCLIENT;
}

// ── 纯函数：窗口几何解析 ──

WindowGeometry resolveWindowGeometry(int x, int y, int width, int height,
                                     int screenWidth, int screenHeight) {
    WindowGeometry g;
    g.x = x;
    g.y = y;
    g.width = width;
    g.height = height;

    // 拿不到屏幕尺寸时（会话切换、RDP 断开的瞬间会返回 0）**原样返回**。
    // 照常往下算的话，坐标会被按 0×0 的屏幕钳成 (0,0)、尺寸被缩成 1×1，
    // 窗口直接缩成左上角的一个点——比不解析更糟。
    if (screenWidth <= 0 || screenHeight <= 0) return g;

    // 比屏幕还大的窗口先缩到屏幕上（V1 overlay.py:391-392 的 min）。
    //
    // 不缩会同时坏两件事：居中算出来的坐标是负的（标题带被推到屏幕外，
    // 用户只看到下半截窗口），而且钳制逻辑会把这个负坐标压成 0——
    // 于是「居中」变成了「贴左上角」。缩到屏幕尺寸后两种情形都自洽。
    if (g.width > screenWidth) g.width = screenWidth;
    if (g.height > screenHeight) g.height = screenHeight;
    if (g.width < 1) g.width = 1;
    if (g.height < 1) g.height = 1;

    // x 或 y 为负 = 这次没有记住的位置（Go 送 -1，V1 里是 win_x/win_y = -1）
    // → 屏幕正中。V1 overlay.py:394-399 用的是 (sw-w)//2 与 (sh-h)//2，
    // 两轴都真的是居中，不是「上三分之一」那种视觉重心偏移。
    if (x < 0 || y < 0) {
        g.x = (screenWidth - g.width) / 2;
        g.y = (screenHeight - g.height) / 2;
        return g;
    }

    // 有记住的位置：钳进**可见**区域。
    //
    // 为什么留 100 像素而不是把窗口整个塞回屏幕（V1 overlay.py:389-390 的
    // `sw - 100` 就是这个语义）：用户把窗口贴到屏幕右边缘是**故意的**，
    // 把 x 硬压回 sw-w 等于每次都擅自挪动他的摆放。留 100 像素既保证窗口
    // 还看得见、标题带抓得住，又不改用户的意图。
    //
    // 这条钳制实际要解决的问题是「换过显示器 / 改过分辨率」：上次记下的
    // 2000 在当前 1920 的屏上会让窗口整个跑到屏幕外面，用户既看不到它、
    // 也没有任何界面入口能把它挪回来。
    const int maxX = (screenWidth > 100) ? screenWidth - 100 : 0;
    const int maxY = (screenHeight > 100) ? screenHeight - 100 : 0;
    g.x = (x > maxX) ? maxX : x;
    g.y = (y > maxY) ? maxY : y;
    return g;
}

// ── 纯函数：毛玻璃重试时刻表 ──

int blurRetryDelayMs(int attempt) {
    switch (attempt) {
        case 0: return 200;    // V1 overlay.py:98  after(200, _apply_acrylic)
        case 1: return 800;    // V1 overlay.py:99  after(800, ...)
        case 2: return 2000;   // V1 overlay.py:100 after(2000, ...)
        default: break;
    }
    return 0;   // 3 次用尽，不再重试
}

// ── 单实例互斥体 ──

SingleInstance::~SingleInstance() { release(); }

bool SingleInstance::acquire(const std::wstring& name) {
    if (handle_) return false;  // 已持有

    HANDLE h = CreateMutexW(nullptr, TRUE, name.c_str());
    if (!h) return false;

    // 同一个进程内重复创建同名互斥体同样会置 ERROR_ALREADY_EXISTS
    alreadyRunning_ = (GetLastError() == ERROR_ALREADY_EXISTS);
    handle_ = reinterpret_cast<void*>(h);
    return !alreadyRunning_;
}

void SingleInstance::release() {
    if (!handle_) return;
    CloseHandle(reinterpret_cast<HANDLE>(handle_));
    handle_ = nullptr;
    alreadyRunning_ = false;
}

// ── 窗口 ──

namespace {

LRESULT CALLBACK overlayProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp);

}  // namespace

OverlayWindow::~OverlayWindow() { destroy(); }

int OverlayWindow::osBuild() {
    // 用 RtlGetVersion 而不是 GetVersionEx：后者会被应用清单影响（与 V1 同一做法）。
    // 这里自带结构体定义，避免依赖 winternl.h / ntddk.h 的可用性。
    struct OsVersionInfo {
        unsigned long dwOSVersionInfoSize;
        unsigned long dwMajorVersion;
        unsigned long dwMinorVersion;
        unsigned long dwBuildNumber;
        unsigned long dwPlatformId;
        wchar_t szCSDVersion[128];
    };

    HMODULE ntdll = GetModuleHandleW(L"ntdll.dll");
    if (!ntdll) return 0;
    using RtlGetVersionFn = LONG(WINAPI*)(OsVersionInfo*);
    auto fn = reinterpret_cast<RtlGetVersionFn>(
        GetProcAddress(ntdll, "RtlGetVersion"));
    if (!fn) return 0;

    OsVersionInfo info{};
    info.dwOSVersionInfoSize = sizeof(info);
    if (fn(&info) != 0) return 0;
    return static_cast<int>(info.dwBuildNumber);
}

bool OverlayWindow::hasDesktopSession() {
    // 没有交互式桌面时会话不可用，建窗口必然失败；测试据此跳过
    HWINSTA station = GetProcessWindowStation();
    if (!station) return false;
    HDESK desk = GetThreadDesktop(GetCurrentThreadId());
    if (!desk) return false;
    return GetSystemMetrics(SM_CMONITORS) > 0;
}

bool OverlayWindow::create(const WindowSpec& spec) {
    if (hwnd_) return true;

    HINSTANCE inst = GetModuleHandleW(nullptr);

    if (!classRegistered_) {
        WNDCLASSEXW wc{};
        wc.cbSize = sizeof(wc);
        wc.style = CS_HREDRAW | CS_VREDRAW;
        wc.lpfnWndProc = overlayProc;
        wc.hInstance = inst;
        wc.hCursor = LoadCursorW(nullptr, IDC_ARROW);
        // 悬浮窗不画背景，留空好让毛玻璃透出来；带边框窗口由系统绘制。
        wc.hbrBackground = spec.framed ? reinterpret_cast<HBRUSH>(COLOR_WINDOW + 1) : nullptr;
        wc.lpszClassName = spec.framed ? kFramedWindowClass : kWindowClass;
        if (!RegisterClassExW(&wc)) {
            if (GetLastError() != ERROR_CLASS_ALREADY_EXISTS) return false;
        }
        classRegistered_ = true;
    }

    // 位置与尺寸：记住的位置要钳进可见屏幕；没记住的（-1）两轴都取正中。
    // 规则与边界全部在 resolveWindowGeometry 里，那里也解释了为什么。
    //
    // ⚠️ 屏幕尺寸用 GetSystemMetrics（主屏）而不是 GetMonitorInfo：
    //    本进程是 DPI-unaware 的，CreateWindowExW 收到的坐标同样经过 DPI
    //    虚拟化，两者必须落在**同一个坐标系**里算；混用会让 125%/150% 缩放下
    //    算出来的居中位置明显偏到一边。多显示器下这里只看主屏，
    //    与 V1 一致（tkinter 的 winfo_screenwidth/height 也只给主屏）。
    const WindowGeometry geo =
        resolveWindowGeometry(spec.x, spec.y, spec.width, spec.height,
                              GetSystemMetrics(SM_CXSCREEN),
                              GetSystemMetrics(SM_CYSCREEN));

    // 设置窗口是普通窗口：进任务栏（WS_EX_APPWINDOW），不置顶、不透明、不穿透。
    // 悬浮窗相反——不进任务栏、置顶、可分层、可穿透。
    DWORD exStyle = spec.framed ? WS_EX_APPWINDOW : WS_EX_TOOLWINDOW;
    if (!spec.framed) {
        if (spec.topmost) exStyle |= WS_EX_TOPMOST;
        if (spec.opacity < 1.0) exStyle |= WS_EX_LAYERED;  // 降级路径才用分层
    }

    // 悬浮窗的样式。
    //
    // ⚠️ WS_THICKFRAME 的来历，以及为什么现在**保留**它。
    //
    // 最初的目的是让 WINDOWS 帮我们缩放：Windows 只在窗口带这个样式时
    // 才处理 SC_SIZE，没有它的话 WM_NCHITTEST 返回 HTBOTTOMRIGHT 也会被
    // 直接忽略。
    //
    // 但那条路最后走不通——WebView2 的子窗口铺满客户区，宿主窗口在四边
    // 根本收不到 WM_NCHITTEST。缩放改由前端 mousedown → IPC →
    // SetWindowPos 实现（见 beginMoveResize），而 SetWindowPos 并不要求
    // 这个样式。
    //
    // 仍然保留它，是因为去掉会改变 DWM 对这个窗口的对待方式（阴影与圆角），
    // 而现在的外观是验证过的。带着它也不会多出边框：窗口过程里的
    // WM_NCCALCSIZE 返回 0，客户区依旧铺满整个窗口矩形。
    const DWORD overlayStyle = WS_POPUP | WS_THICKFRAME;

    HWND hwnd = CreateWindowExW(
        exStyle,
        spec.framed ? kFramedWindowClass : kWindowClass,
        spec.title.c_str(),
        // 设置窗口直接用系统的标准带边框样式：标题栏、最小化/最大化/关闭、
        // 边缘缩放全部交给系统——它本来就该是这么一扇窗口。
        spec.framed ? static_cast<DWORD>(WS_OVERLAPPEDWINDOW) : overlayStyle,
        geo.x, geo.y, geo.width, geo.height,
        nullptr, nullptr, inst, this);

    if (!hwnd) {
        std::printf("[window] CreateWindowExW 失败，错误码 %lu\n", GetLastError());
        return false;
    }

    hwnd_ = asVoid(hwnd);
    created_ = true;  // 记住「曾经创建过」，供主循环区分「还没建」与「建了又销毁」
    framed_ = spec.framed;
    opacity_ = spec.opacity;
    dark_ = spec.dark;  // 必须在 applyBlur 之前：材质深浅由它决定

    if (spec.framed) {
        // 设置窗口只需要跟随主题的深色标题栏，不要毛玻璃、不要置顶、
        // 不要透明度、不要穿透——它是一扇普通窗口。
        if (spec.dark) {
            BOOL on = TRUE;
            DwmSetWindowAttribute(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE_, &on, sizeof(on));
        }
        return true;
    }

    if (spec.topmost) {
        SetWindowPos(hwnd, HWND_TOPMOST, 0, 0, 0, 0,
                     SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE);
    }
    if (spec.opacity < 1.0) setOpacity(spec.opacity);
    if (spec.clickThrough) setClickThrough(true);
    applyBlur(spec.blur);

    return true;
}

void OverlayWindow::onDestroyed() {
    // 清几何相关的状态，并**把 hwnd_ 置空**——这是这个函数存在的理由：
    // 用户点标题栏的 × 时是系统直接销毁窗口，不会走 OverlayWindow::destroy()，
    // 不在这里清就会留下一个指向已销毁窗口的句柄，valid() 永远为真。
    hwnd_ = nullptr;
    appliedBlur_ = "none";
    resizeHandler_ = nullptr;
    dragging_ = false;
    dragZone_ = HitZone::None;

    // 重试计数与「隐藏过」的标记也要清：窗口都没了，还留着它们会让下一个
    // 重建出来的窗口从「已经重试过 3 次」开始，一次毛玻璃都不重试。
    // 定时器不用管：SetTimer 绑在窗口上，窗口销毁时系统会一起收掉。
    blurRetry_ = 0;
    needsBlurReapply_ = false;
    hiddenByUser_ = false;
}

bool OverlayWindow::destroy() {
    if (!hwnd_) return true;
    DestroyWindow(asHwnd(hwnd_));
    // DestroyWindow 会同步派发 WM_DESTROY，onDestroyed() 已经把状态清干净了。
    // 这里兜一道底，防止窗口过程因为某种原因没被调到。
    if (hwnd_) onDestroyed();
    return true;
}

void OverlayWindow::show() {
    if (!hwnd_) return;
    ShowWindow(asHwnd(hwnd_), SW_SHOWNOACTIVATE);
}

void OverlayWindow::hide() {
    if (!hwnd_) return;
    ShowWindow(asHwnd(hwnd_), SW_HIDE);
}

bool OverlayWindow::focus() {
    if (!hwnd_) return false;
    HWND h = asHwnd(hwnd_);

    // 隐藏着就先显示出来（V1 的 `deiconify`）：热键呼出时窗口不该还藏着。
    if (!IsWindowVisible(h)) {
        ShowWindow(h, SW_SHOWNOACTIVATE);
    }

    // ⚠️ 单纯 SetForegroundWindow 在后台进程里**经常直接失败**。
    //
    // Windows 的「前台锁」只允许两类进程抢前台：当前前台窗口所属的进程，
    // 或刚收到用户输入的进程。热键触发时两条都不满足，调用会被静默忽略
    // （返回 FALSE，没有异常）——用户看到的现象就是「按了呼出热键没反应」。
    //
    // V1 overlay.py:809-849 的做法是把本线程**挂到**前台线程上（AttachThreadInput），
    // 借用它的前台权限完成抢占，完了再解挂。这里照做。
    const HWND fg = GetForegroundWindow();
    const DWORD fgThread = (fg != nullptr) ? GetWindowThreadProcessId(fg, nullptr) : 0;
    const DWORD ownThread = GetCurrentThreadId();

    bool attached = false;
    if (fgThread != 0 && fgThread != ownThread) {
        attached = AttachThreadInput(ownThread, fgThread, TRUE) != FALSE;
    }

    const BOOL ok = SetForegroundWindow(h);
    SetActiveWindow(h);
    SetFocus(h);

    if (attached) {
        AttachThreadInput(ownThread, fgThread, FALSE);
    }

    // V1 还额外合成了一次鼠标点击（tkinter 的 Entry 要靠点击才拿键盘焦点），
    // 并做了 50/150ms 重试。这两样在 WebView2 里都不需要：窗口成为前台之后，
    // 前端对输入框调 focus() 就能拿到键盘焦点。
    return ok != FALSE;
}

bool OverlayWindow::isVisible() const {
    if (!hwnd_) return false;
    return IsWindowVisible(asHwnd(hwnd_)) != FALSE;
}

bool OverlayWindow::setClickThrough(bool enable) {
    if (!hwnd_) return false;
    LONG_PTR ex = GetWindowLongPtrW(asHwnd(hwnd_), GWL_EXSTYLE);
    if (enable) {
        ex |= WS_EX_TRANSPARENT;
    } else {
        ex &= ~static_cast<LONG_PTR>(WS_EX_TRANSPARENT);
    }
    SetWindowLongPtrW(asHwnd(hwnd_), GWL_EXSTYLE, ex);
    return true;
}

bool OverlayWindow::isClickThrough() const {
    if (!hwnd_) return false;
    LONG_PTR ex = GetWindowLongPtrW(asHwnd(hwnd_), GWL_EXSTYLE);
    return (ex & WS_EX_TRANSPARENT) != 0;
}

bool OverlayWindow::applyBlur(const std::string& mode) {
    if (!hwnd_) return false;

    // 记住**期望**模式。重试与「显示后重应用」都要拿它重来一次，
    // 只记结果（appliedBlur_）是不够的——失败时结果会退化成 "none"，
    // 拿它去重试等于什么都不做。
    blurMode_ = mode;

    // 每次显式调用都重新给足 3 次机会：上一次已经把次数用尽时，
    // 这里的重试预算要归零，否则会立刻打出「3 次重试均失败」——
    // 一次都没试就宣告失败，日志会把排障的人带偏。
    blurRetry_ = 0;

    if (applyBlurNow(mode)) {
        cancelBlurRetry();
        return true;
    }

    // 失败**不是**「一次定终身」：调用这一刻窗口往往刚建出来、还没画过，
    // DWM 会拒绝设置材质；慢一点的机器上甚至要等到一两秒后才认。
    // V1 为此重试 3 次（overlay.py:97-100），这里照做。
    armBlurRetry();
    return false;
}

bool OverlayWindow::applyBlurNow(const std::string& mode) {
    if (!hwnd_) return false;

    HWND hwnd = asHwnd(hwnd_);
    const int build = osBuild();

    if (mode == "none") {
        if (build >= kWin11Backdrop) {
            applyDwmBackdrop(hwnd, DWMSBT_NONE, dark_);
        } else if (build >= kWin10Acrylic) {
            applyAccentAcrylic(hwnd, ACCENT_DISABLED, 0);
        }
        appliedBlur_ = "none";
        return true;
    }

    // Path 1：Win11 22621+ —— DWM 系统背景（Mica / Acrylic）
    if (build >= kWin11Backdrop) {
        int backdrop = DWMSBT_MAINWINDOW;
        if (mode == "acrylic") backdrop = DWMSBT_TRANSIENTWINDOW;
        if (applyDwmBackdrop(hwnd, backdrop, dark_)) {
            appliedBlur_ = (mode == "acrylic") ? "acrylic" : "mica";
            return true;
        }
        // 失败则继续往下试 Win10 路径
    }

    // Path 2：Win10 1803+ —— 未公开的 SetWindowCompositionAttribute 亚克力
    if (build >= kWin10Acrylic) {
        // 0xAABBGGRR：深色用深蓝灰，浅色用浅灰，同样跟随主题
        const DWORD tint = dark_ ? 0x701A1A2E : 0x70F2F2F2;
        if (applyAccentAcrylic(hwnd, ACCENT_ENABLE_ACRYLICBLURBEHIND, tint)) {
            appliedBlur_ = "acrylic";
            return true;
        }
    }

    // Path 3：都不行 → 调用方应退回纯色/半透明（与 V1 一致）
    appliedBlur_ = "none";
    std::printf("[window] 毛玻璃不可用（build=%d，模式=%s），本轮回退纯色\n",
                build, mode.c_str());
    std::fflush(stdout);
    return false;
}

void OverlayWindow::armBlurRetry() {
    if (!hwnd_) return;

    const int delay = blurRetryDelayMs(blurRetry_);
    if (delay <= 0) {
        // 3 次重试用尽 → 明确放弃，并**留下日志**。
        // 静默降级是最难查的一种：用户只看到「说好的毛玻璃没了」，
        // 日志里却没有任何一行能解释为什么。
        std::printf("[window] 毛玻璃 3 次重试均失败（模式=%s），已退回纯色\n",
                    blurMode_.c_str());
        std::fflush(stdout);
        return;
    }

    SetTimer(asHwnd(hwnd_), kBlurRetryTimerId, static_cast<UINT>(delay), nullptr);
    ++blurRetry_;
}

void OverlayWindow::cancelBlurRetry() {
    if (hwnd_) KillTimer(asHwnd(hwnd_), kBlurRetryTimerId);
    blurRetry_ = 0;
}

void OverlayWindow::onBlurTimer() {
    if (!hwnd_) return;

    if (applyBlurNow(blurMode_)) {
        KillTimer(asHwnd(hwnd_), kBlurRetryTimerId);
        std::printf("[window] 毛玻璃第 %d 次重试成功: %s\n",
                    blurRetry_, appliedBlur_.c_str());
        std::fflush(stdout);
        blurRetry_ = 0;
        return;
    }

    armBlurRetry();   // 还有次数就安排下一次，用尽则留下降级日志
}

void OverlayWindow::onWindowHidden() {
    // 只是打个标记，真正的重应用等窗口再显示出来的时候做：
    // 隐藏状态下设置 DWM 材质没有任何意义，反而多一次无用的系统调用。
    needsBlurReapply_ = true;
}

void OverlayWindow::onWindowShown() {
    if (!hwnd_) return;
    if (!needsBlurReapply_) return;   // 第一次显示：create() 那轮已经应用过了
    needsBlurReapply_ = false;

    if (blurMode_ == "none") return;  // 调用方明确要求不要材质

    // 为什么非重应用不可：窗口隐藏期间 DWM 会把它从合成树里摘掉，材质随之
    // 失效，再显示出来就是一块纯色（或全透明），而 appliedBlur_ 还记着
    // "mica"——上层完全看不出出了问题。托盘的「显示/隐藏」是常规操作，
    // 一次隐藏就会让毛玻璃永久消失。
    // V1 overlay.py:101-102 用 tkinter 的 `<Map>` 事件做同一件事。
    blurRetry_ = 0;   // 每次重新显示都重新给足 3 次机会
    if (applyBlurNow(blurMode_)) {
        std::printf("[window] 窗口重新显示，毛玻璃已重新应用: %s\n",
                    appliedBlur_.c_str());
        std::fflush(stdout);
        return;
    }
    armBlurRetry();
}

bool OverlayWindow::takeUserHide() {
    if (!hiddenByUser_) return false;
    hiddenByUser_ = false;
    return true;
}

bool OverlayWindow::setDarkMode(bool dark) {
    dark_ = dark;
    // 材质已经生效的话要重新应用一次——DWM 只在设置时读取这个属性
    if (hwnd_ && appliedBlur_ != "none") {
        return applyBlur(appliedBlur_);
    }
    if (hwnd_) {
        // 即使当前没启用材质，也要把属性设上，供之后启用时使用
        applyDwmBackdrop(asHwnd(hwnd_), DWMSBT_NONE, dark_);
    }
    return true;
}

bool OverlayWindow::setOpacity(double opacity) {
    if (!hwnd_) return false;
    if (opacity < 0.10) opacity = 0.10;
    if (opacity > 1.00) opacity = 1.00;
    opacity_ = opacity;

    HWND hwnd = asHwnd(hwnd_);
    LONG_PTR ex = GetWindowLongPtrW(hwnd, GWL_EXSTYLE);
    if (opacity >= 1.0) {
        ex &= ~static_cast<LONG_PTR>(WS_EX_LAYERED);
        SetWindowLongPtrW(hwnd, GWL_EXSTYLE, ex);
        return true;
    }
    ex |= WS_EX_LAYERED;
    SetWindowLongPtrW(hwnd, GWL_EXSTYLE, ex);
    const BYTE alpha = static_cast<BYTE>(opacity * 255.0 + 0.5);
    return SetLayeredWindowAttributes(hwnd, 0, alpha, LWA_ALPHA) != FALSE;
}

bool OverlayWindow::pump() {
    MSG msg;
    while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
        if (msg.message == WM_QUIT) {
            hwnd_ = nullptr;
            return false;
        }
        TranslateMessage(&msg);
        DispatchMessageW(&msg);
    }
    return hwnd_ != nullptr;
}

bool OverlayWindow::getBounds(int& x, int& y, int& width, int& height) const {
    if (!hwnd_) return false;
    RECT rc{};
    if (!GetWindowRect(asHwnd(hwnd_), &rc)) return false;
    x = rc.left;
    y = rc.top;
    width = rc.right - rc.left;
    height = rc.bottom - rc.top;
    return true;
}

bool OverlayWindow::setBounds(int x, int y, int width, int height) {
    if (!hwnd_) return false;
    return SetWindowPos(asHwnd(hwnd_), nullptr, x, y, width, height,
                        SWP_NOZORDER | SWP_NOACTIVATE) != FALSE;
}

void OverlayWindow::setResizeHandler(std::function<void(int, int)> handler) {
    resizeHandler_ = std::move(handler);
}

void OverlayWindow::notifySizeChanged(int width, int height) {
    if (resizeHandler_) resizeHandler_(width, height);
}

bool OverlayWindow::takeUserGeometryChange(int& x, int& y, int& width, int& height) {
    if (!geometryDirty_) return false;
    geometryDirty_ = false;
    return getBounds(x, y, width, height);
}

namespace {

// 最小窗口尺寸 —— 与 V1 overlay.py:432 的 `self.root.minsize(280, 250)` 一致。
constexpr int kMinWidth = 280;
constexpr int kMinHeight = 250;

// 区域 → 系统指针。同时用于拖动中的反馈与前端 CSS 的 cursor 取值依据。
// 中心的 "fleur"（四向移动）在 V1 里对应 IDC_SIZEALL。
LPCWSTR cursorForZone(HitZone zone) {
    switch (zone) {
        case HitZone::Caption:     return IDC_SIZEALL;
        case HitZone::Left:
        case HitZone::Right:       return IDC_SIZEWE;
        case HitZone::Top:
        case HitZone::Bottom:      return IDC_SIZENS;
        case HitZone::TopLeft:
        case HitZone::BottomRight: return IDC_SIZENWSE;
        case HitZone::TopRight:
        case HitZone::BottomLeft:  return IDC_SIZENESW;
        case HitZone::None:        break;
    }
    return IDC_ARROW;
}

}  // namespace

bool OverlayWindow::beginMoveResize(HitZone zone) {
    if (!hwnd_) return false;
    if (zone == HitZone::None) return false;

    POINT pt{};
    if (!GetCursorPos(&pt)) return false;
    if (!getBounds(dragStartX_, dragStartY_, dragStartW_, dragStartH_)) return false;

    dragZone_ = zone;
    dragging_ = true;
    dragOriginX_ = pt.x;
    dragOriginY_ = pt.y;
    dragLastX_ = pt.x;
    dragLastY_ = pt.y;

    SetCursor(LoadCursorW(nullptr, cursorForZone(zone)));
    std::printf("[window] 开始拖动 zone=%d 窗口 %d,%d %dx%d 光标 %d,%d\n",
                static_cast<int>(zone), dragStartX_, dragStartY_, dragStartW_,
                dragStartH_, pt.x, pt.y);
    std::fflush(stdout);
    return true;
}

bool OverlayWindow::tickMoveResize() {
    if (!dragging_) return false;

    // 左键松开就结束。
    //
    // 用 GetAsyncKeyState 轮询按键状态，而不是等前端发 mouseup：
    // 拖动中途鼠标很可能移出窗口、或被别的窗口抢走消息，那时前端再也收不到
    // mouseup——窗口就会一直粘在鼠标上。轮询按键没有这个失败模式。
    if ((GetAsyncKeyState(VK_LBUTTON) & 0x8000) == 0) {
        dragging_ = false;
        dragZone_ = HitZone::None;
        // 用户确实改过几何了，等主循环取走去上报（不要在这里直接发事件：
        // 这里在消息泵里，管道写入归主循环管）。
        geometryDirty_ = true;
        int fx = 0, fy = 0, fw = 0, fh = 0;
        if (getBounds(fx, fy, fw, fh)) {
            std::printf("[window] 拖动结束: %d,%d %dx%d\n", fx, fy, fw, fh);
            std::fflush(stdout);
        }
        return false;
    }

    POINT pt{};
    if (!GetCursorPos(&pt)) return true;

    int x = 0, y = 0, w = 0, h = 0;

    if (dragZone_ == HitZone::Caption) {
        // 移动：以按下那一刻的窗口位置 + **绝对**位移。
        // 用绝对量而非增量，是为了避免每轮的小误差累积成漂移。
        x = dragStartX_ + (pt.x - dragOriginX_);
        y = dragStartY_ + (pt.y - dragOriginY_);
        w = dragStartW_;
        h = dragStartH_;
    } else {
        // 缩放：读**当前**几何再叠加增量，语义与 V1 overlay.py:508-528 一致。
        //
        // 每轮重读当前尺寸（而不是一直用按下时的尺寸）有个好处：
        // 系统若对尺寸做了修正，下一轮会自动以修正后的值为基准，不会累积偏差。
        if (!getBounds(x, y, w, h)) return true;

        const int dx = pt.x - dragLastX_;
        const int dy = pt.y - dragLastY_;

        // 从右边/下边拉：直接加。
        if (dragZone_ == HitZone::Right || dragZone_ == HitZone::TopRight ||
            dragZone_ == HitZone::BottomRight) {
            w = (w + dx > kMinWidth) ? w + dx : kMinWidth;
        }
        if (dragZone_ == HitZone::Bottom || dragZone_ == HitZone::BottomLeft ||
            dragZone_ == HitZone::BottomRight) {
            h = (h + dy > kMinHeight) ? h + dy : kMinHeight;
        }
        // 从左边/上边拉：尺寸变多少，原点就补多少，保证对边钉住不动。
        if (dragZone_ == HitZone::Left || dragZone_ == HitZone::TopLeft ||
            dragZone_ == HitZone::BottomLeft) {
            const int nw = (w - dx > kMinWidth) ? w - dx : kMinWidth;
            x += w - nw;
            w = nw;
        }
        if (dragZone_ == HitZone::Top || dragZone_ == HitZone::TopLeft ||
            dragZone_ == HitZone::TopRight) {
            const int nh = (h - dy > kMinHeight) ? h - dy : kMinHeight;
            y += h - nh;
            h = nh;
        }
    }

    dragLastX_ = pt.x;
    dragLastY_ = pt.y;

    SetWindowPos(asHwnd(hwnd_), nullptr, x, y, w, h,
                 SWP_NOZORDER | SWP_NOACTIVATE);

    // 拖到中途光标往往已经离开抓到的那几像素，系统会把指针改回默认箭头。
    // 每轮重新设一次，视觉上才连贯。
    SetCursor(LoadCursorW(nullptr, cursorForZone(dragZone_)));
    return true;
}

namespace {

LRESULT CALLBACK overlayProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp) {
    if (msg == WM_NCCREATE) {
        auto* cs = reinterpret_cast<CREATESTRUCTW*>(lp);
        SetWindowLongPtrW(hwnd, GWLP_USERDATA,
                          reinterpret_cast<LONG_PTR>(cs->lpCreateParams));
    }

    // 下面几处分支都只在**无边框悬浮窗**上生效；带边框的设置窗口一律交回
    // DefWindowProc，让系统按标准窗口处理（它有真正的标题栏和边框）。
    const bool framed = (GetWindowLongPtrW(hwnd, GWL_STYLE) & WS_POPUP) == 0;

    switch (msg) {
        // ⚠️ 必须返回 0：让客户区等于整个窗口矩形。
        //
        // 悬浮窗带了 WS_THICKFRAME（见 CreateWindowExW 处的说明），它本来会
        // 占掉一圈非客户区并画出真正的边框。返回 0 之后边框既不占位也不绘制，
        // 仍然是彻底的无边框窗口。
        //
        // wParam == FALSE 时表示「正在为客户区算 WM_SIZE」，那种情况必须
        // 交回 DefWindowProc，不能也返回 0。
        case WM_NCCALCSIZE:
            if (!framed && wp == TRUE) return 0;
            break;

        case WM_NCHITTEST: {
            if (framed) break;  // 标准窗口的命中测试交给系统
            RECT rc{};
            GetWindowRect(hwnd, &rc);
            const int w = rc.right - rc.left;
            const int h = rc.bottom - rc.top;
            const int x = static_cast<int>(LOWORD(lp)) - rc.left;
            const int y = static_cast<int>(HIWORD(lp)) - rc.top;
            HitZone zone = hitTestZone(w, h, x, y, kResizeBorder);
            return hitZoneToNcCode(zone);
        }

        // 边缘的缩放指针要自己设。
        //
        // 悬浮窗没有 WS_CAPTION，DefWindowProc 对非客户区光标的处理依赖
        // 窗口样式，不能指望它给出正确的缩放箭头——那样用户看不到任何提示，
        // 只能靠猜边缘在哪。带边框窗口有标准边框，系统自己就会给出。
        case WM_SETCURSOR: {
            if (framed) break;
            LPCWSTR id = nullptr;
            switch (LOWORD(lp)) {
                case HTLEFT:
                case HTRIGHT:      id = IDC_SIZEWE;   break;
                case HTTOP:
                case HTBOTTOM:     id = IDC_SIZENS;   break;
                case HTTOPLEFT:
                case HTBOTTOMRIGHT: id = IDC_SIZENWSE; break;
                case HTTOPRIGHT:
                case HTBOTTOMLEFT: id = IDC_SIZENESW; break;
                default: break;
            }
            if (id != nullptr) {
                SetCursor(LoadCursorW(nullptr, id));
                return TRUE;
            }
            break;
        }

        case WM_SIZE: {
            // 原生缩放循环期间，主循环不会跑，所以 WebView 的尺寸必须
            // 从这里同步——否则拖完边框才「跳」一下，中间一直是旧尺寸。
            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (wp != SIZE_MINIMIZED) {
                if (self != nullptr) {
                    self->notifySizeChanged(static_cast<int>(LOWORD(lp)),
                                            static_cast<int>(HIWORD(lp)));
                }
            }
            // 带边框窗口（设置窗口）的位置尺寸是**用户**用系统标题栏改的，
            // 主循环无从得知，只能靠这里打标记让它去上报（见
            // takeUserGeometryChange 的说明）。
            //
            // 最小化/最大化时**不记**：那不是用户想要的「上次位置」，
            // 记下来会让下次打开设置窗口直接最大化。
            if (self != nullptr && self->framed() && wp == SIZE_RESTORED &&
                !IsZoomed(hwnd) && !IsIconic(hwnd)) {
                self->markUserGeometryChanged();
            }
            break;
        }

        case WM_MOVE: {
            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (self != nullptr && self->framed() && IsWindowVisible(hwnd) &&
                !IsZoomed(hwnd) && !IsIconic(hwnd)) {
                self->markUserGeometryChanged();
            }
            break;
        }

        case WM_ERASEBKGND:
            // 悬浮窗不擦背景：让 DWM 的毛玻璃透出来（否则会闪白）。
            // 带边框窗口按常规擦，否则缩放时会拖出残影。
            if (!framed) return 1;
            break;

        // ⚠️ 这里**故意没有** WM_ACTIVATE → 撤掉置顶的处理。
        //
        // 曾经加过一版「失去激活就 HWND_NOTOPMOST」，结果是设置窗口再也浮不上来：
        // 打开时我们会把它抬到 HWND_TOPMOST（游戏窗口本身是置顶的，不抬就被压住），
        // 但 `SetForegroundWindow` 在**后台进程**里经常直接失败——窗口被抬成置顶
        // 却从未被激活，于是 WM_ACTIVATE(WA_INACTIVE) 立刻到达、置顶马上被撤，
        // 用户看到的现象和修之前一模一样。
        //
        // 现在的语义：**设置窗口开着就一直置顶**。它是用户主动打开、主动关闭的
        // 配置面板，期间压在游戏上面正是他要的；关掉窗口置顶自然消失
        // （framed 窗口走 DefWindowProc 销毁）。悬浮窗不受影响——它本来就一直置顶。

        // 悬浮窗的「关闭」＝**隐藏**，不是销毁。
        //
        // V1 就是这么定义的：`main.py:85` 把 WM_DELETE_WINDOW 绑到 hide，
        // `main.py:210-212` 的 _on_close 也只调 overlay.hide()——进程和托盘
        // 照常跑，用户随时能把窗口叫回来。
        //
        // ⚠️ 不能交回 DefWindowProc（改之前就是这样）：它会**销毁**窗口，
        //    悬浮窗从此消失，而且没有任何办法把它找回来——Go 期望状态里窗口
        //    还是「可见」的，托盘「显示/隐藏」取反之后仍然是隐藏，
        //    用户看到的现象就是「按了下 Alt+F4，悬浮窗再也回不来」。
        //    隐藏则是可逆的：主循环把这个事实回报 Go，新消息一到窗口就回来。
        //
        // ⚠️ 带边框的**设置窗口**语义相反：它就是普通窗口，关闭＝关掉，
        //    所以 framed 一律 break 交回 DefWindowProc，这里绝不能一起改。
        case WM_CLOSE: {
            if (framed) break;

            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (self != nullptr) {
                // 先打标记再隐藏：这个标记是主循环用来回报 Go 的唯一依据，
                // 顺序反了（或者漏掉）就等于 Go 永远以为窗口还开着。
                self->markHiddenByUser();
                self->hide();
                std::printf("[window] 用户关闭悬浮窗 → 隐藏（进程继续运行）\n");
                std::fflush(stdout);
            } else {
                ShowWindow(hwnd, SW_HIDE);   // 兜底：万一 lpCreateParams 没接上
            }
            return 0;
        }

        // 毛玻璃重试（V1 overlay.py:97-100 的 200/800/2000ms 三次）
        case WM_TIMER: {
            if (wp != kBlurRetryTimerId) break;
            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (self != nullptr) self->onBlurTimer();
            return 0;
        }

        // 窗口被显示/隐藏。隐藏会让 DWM 摘掉材质，重新显示时必须重应用一次
        // （V1 overlay.py:101-102 用 tkinter 的 `<Map>` 事件做同一件事）。
        case WM_SHOWWINDOW: {
            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (self != nullptr) {
                if (wp != FALSE) {
                    self->onWindowShown();
                } else {
                    self->onWindowHidden();
                }
            }
            break;   // 交回 DefWindowProc：这条消息系统自己也要用
        }

        case WM_DESTROY: {
            // ⚠️ 必须在这里把窗口对象的状态清掉。用户点标题栏的 × 时是系统
            //    直接销毁窗口，不会经过 OverlayWindow::destroy()——不清的话
            //    hwnd_ 会一直指向已销毁的窗口，上层完全察觉不到窗口没了。
            auto* self = reinterpret_cast<OverlayWindow*>(
                GetWindowLongPtrW(hwnd, GWLP_USERDATA));
            if (self != nullptr) self->onDestroyed();
            SetWindowLongPtrW(hwnd, GWLP_USERDATA, 0);
            return 0;
        }

        default:
            break;
    }
    return DefWindowProcW(hwnd, msg, wp, lp);
}

}  // namespace

}  // namespace tn
