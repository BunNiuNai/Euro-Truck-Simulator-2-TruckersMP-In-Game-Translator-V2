// 系统托盘图标实现。设计说明见 tray.hpp。
#include "tray.hpp"

#include "window.hpp"  // 复用 hasDesktopSession

#include <windows.h>
#include <shellapi.h>

#include <atomic>
#include <cstdio>
#include <thread>

namespace tn {

namespace {

// 托盘回调消息。与 V1 的 `WM_TRAYICON = WM_USER + 1` 对应。
constexpr UINT kTrayCallbackMsg = WM_APP + 1;

// 「期望状态变了，刷新一下提示文字」——由主线程 PostMessage 给托盘线程。
constexpr UINT kTrayRefreshMsg = WM_APP + 2;

// 「立刻收摊」——由主线程 **PostThreadMessage** 给托盘线程（发到线程消息队列，
// 不经过任何窗口）。存在的理由：WM_CLOSE 要靠窗口过程配合才能让消息循环退出，
// 而窗口过程万一不是我们的（同名窗口类被别人抢先注册——纯属防御性考虑），
// WM_CLOSE 会石沉大海、PostQuitMessage 也不会有人调，join() 就永远不返回。
// 线程消息只要求「消息循环还在跑」，与窗口过程无关。
constexpr UINT kTrayStopMsg = WM_APP + 3;

constexpr UINT kTrayIconUID = 1;
constexpr wchar_t kTrayClass[] = L"ETS2TrayIconV2";

// 菜单项 id 从 1 开始：TrackPopupMenu 返回 0 表示「没选中任何项」，
// 用 0 当第一个 id 就没法区分「选了第一项」和「取消了」。
constexpr UINT kFirstMenuId = 1;

std::wstring utf8ToWide(const std::string& s) {
    if (s.empty()) return std::wstring();
    const int need = MultiByteToWideChar(CP_UTF8, 0, s.c_str(), static_cast<int>(s.size()),
                                         nullptr, 0);
    if (need <= 0) return std::wstring();
    std::wstring out(static_cast<size_t>(need), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, s.c_str(), static_cast<int>(s.size()), &out[0], need);
    return out;
}

// 生成 32×32 的托盘图标，与 V1 `tray_icon.py:_create_icon_simple` 同一外观：
// 深色底 + 主色方块 + 白色 "T"。
//
// 为什么不加载 .ico 文件：项目里没有随附图标资源，V1 也是现画的。
HICON createTrayIcon() {
    constexpr int kSize = 32;

    HDC screen = GetDC(nullptr);
    if (screen == nullptr) return nullptr;

    HDC mem = CreateCompatibleDC(screen);
    HBITMAP color = CreateCompatibleBitmap(screen, kSize, kSize);
    HBITMAP oldColor = static_cast<HBITMAP>(SelectObject(mem, color));

    const RECT full{0, 0, kSize, kSize};

    HBRUSH bg = CreateSolidBrush(RGB(0x1e, 0x1e, 0x1e));
    FillRect(mem, &full, bg);
    DeleteObject(bg);

    const RECT inner{4, 4, 28, 28};
    HBRUSH accent = CreateSolidBrush(RGB(0x56, 0x9c, 0xd6));
    FillRect(mem, &inner, accent);
    DeleteObject(accent);

    SetBkMode(mem, TRANSPARENT);
    SetTextColor(mem, RGB(0xff, 0xff, 0xff));
    HFONT font = CreateFontW(18, 0, 0, 0, FW_BOLD, 0, 0, 0, DEFAULT_CHARSET, 0, 0, 0, 0,
                             L"Microsoft YaHei");
    HFONT oldFont = static_cast<HFONT>(SelectObject(mem, font));
    RECT textRect{0, 4, kSize, kSize};
    // V1 只给了 DT_CENTER|DT_VCENTER，而 DT_VCENTER 不带 DT_SINGLELINE 是不生效的
    // （垂直居中会被忽略）。这里补上——纯外观修正，不影响任何功能。
    DrawTextW(mem, L"T", 1, &textRect, DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    SelectObject(mem, oldFont);
    DeleteObject(font);

    // 掩码位图必须是 1bpp，且全 1（全部像素可见）。
    HDC mask = CreateCompatibleDC(screen);
    HBITMAP maskBmp = CreateBitmap(kSize, kSize, 1, 1, nullptr);
    HBITMAP oldMask = static_cast<HBITMAP>(SelectObject(mask, maskBmp));
    HBRUSH white = CreateSolidBrush(RGB(0xff, 0xff, 0xff));
    FillRect(mask, &full, white);
    DeleteObject(white);

    ICONINFO ii{};
    ii.fIcon = TRUE;
    ii.hbmColor = color;
    ii.hbmMask = maskBmp;
    HICON icon = CreateIconIndirect(&ii);

    SelectObject(mask, oldMask);
    DeleteObject(maskBmp);
    DeleteDC(mask);
    SelectObject(mem, oldColor);
    DeleteObject(color);
    DeleteDC(mem);
    ReleaseDC(nullptr, screen);
    return icon;
}

}  // namespace

// ── 实现体 ──────────────────────────────────────────────────

struct TrayIcon::Impl {
    std::thread thread;
    std::atomic<bool> running{false};

    // 「别再等了，收摊」——destroy() 置位，托盘线程在进消息循环之前读它。
    //
    // ⚠️ 为什么不能只靠「给托盘窗口发 WM_CLOSE」：窗口是托盘线程**异步**建的，
    //    destroy() 完全可能赶在窗口发布出来之前就跑完，那一刻手上没有任何
    //    句柄可发；而托盘线程马上会一头扎进 GetMessageW 等一条永远不会来的
    //    消息——join() 于是永不返回，整个进程卡死在托盘销毁上（关不掉、
    //    连日志都出不来，只能杀进程）。
    //    有了这个标志，线程不需要任何人给它发消息也能退出来。
    //
    // 顺序上有讲究：destroy() 必须先置位**再**读 hwnd。反过来的话，「读到空
    // hwnd」与「线程刚发布 hwnd 并进入等待」这两件事可以交错，唤醒照样丢。
    std::atomic<bool> stopRequested{false};

    std::atomic<bool> active{false};      // 图标是否已挂上
    std::atomic<HWND> hwnd{nullptr};      // 托盘窗口（由托盘线程创建/销毁）

    // 托盘线程 id。destroy() 用它 PostThreadMessage 唤醒消息循环——
    // 那条路不经过窗口过程，所以「窗口过程不是我们的」也不影响唤醒（见 kTrayStopMsg）。
    std::atomic<DWORD> threadId{0};

    // 下三项受 mu 保护：主线程写 spec、读 commands/error；托盘线程反过来。
    std::mutex mu;
    TraySpec spec;
    std::vector<std::string> commands;
    std::string error;

    void setError(const std::string& what) {
        std::lock_guard<std::mutex> lock(mu);
        error = what;
    }

    void push(const std::string& id) {
        if (id.empty()) return;
        std::lock_guard<std::mutex> lock(mu);
        commands.push_back(id);
    }

    // 托盘线程里要收尾的那几样东西。
    //
    // 打包成结构体是为了把「初始化」与「收尾」彻底分开：初始化失败时只置
    // 错误信息返回 false，收尾由 run() 末尾**唯一**的那一处完成。
    // 否则每个失败分支都得自己 return，而任何一次提前 return 都会留下一个
    // joinable 的 std::thread——它既不会被 destroy() 收掉（那时 running 已经
    // 是 false），也不会被析构收掉，于是 `delete impl_` 时 std::thread 的析构
    // 直接 std::terminate()：进程凭空消失，问题现场一点都没留下。
    struct Resources {
        HWND hwnd = nullptr;
        HICON icon = nullptr;
        bool iconAdded = false;
        NOTIFYICONDATAW nid{};
    };

    // 建窗口 + 挂图标。失败只置 error 并返回 false，**不做任何收尾**。
    bool attach(Resources& res);
    // 撤图标 → 销毁窗口 → 释放图标（顺序不能反）。
    void detach(Resources& res);

    // 托盘线程的主函数：attach → 消息循环 → detach。**不设提前 return**。
    void run();
    void showMenu(HWND h);
    void refreshTip(HWND h);
};

namespace {

// 托盘窗口过程。需要拿到所属的 Impl，所以用 GWLP_USERDATA 存指针。
LRESULT CALLBACK trayProc(HWND hwnd, UINT msg, WPARAM wp, LPARAM lp) {
    if (msg == WM_NCCREATE) {
        auto* cs = reinterpret_cast<CREATESTRUCTW*>(lp);
        SetWindowLongPtrW(hwnd, GWLP_USERDATA,
                          reinterpret_cast<LONG_PTR>(cs->lpCreateParams));
    }

    auto* self = reinterpret_cast<TrayIcon::Impl*>(GetWindowLongPtrW(hwnd, GWLP_USERDATA));
    if (self == nullptr) return DefWindowProcW(hwnd, msg, wp, lp);

    if (msg == kTrayCallbackMsg) {
        // ⚠️ 这里用 lParam 判断鼠标事件，意味着**不设置 NOTIFYICON_VERSION_4**。
        //    version 4 会把 lParam 的语义换成「LOWORD=事件、HIWORD=图标 id」，
        //    两者混用是托盘代码最常见的 bug 来源。V1 也没设版本，这里保持一致。
        if (lp == WM_RBUTTONUP) {
            self->showMenu(hwnd);
        } else if (lp == WM_LBUTTONUP || lp == WM_LBUTTONDBLCLK) {
            // 左键 = 默认项（V1 的 default_cb，挂的是「显示/隐藏」）。
            //
            // ⚠️ 必须**先出锁再 push**：push 自己也要拿同一把锁，
            //    而 std::mutex 不可重入，持锁调用会直接死锁。
            std::string defaultId;
            {
                std::lock_guard<std::mutex> lock(self->mu);
                for (const TrayMenuItem& item : self->spec.menu) {
                    if (item.isDefault && !item.separator) {
                        defaultId = item.id;
                        break;
                    }
                }
            }
            self->push(defaultId);
        }
        return 0;
    }

    if (msg == kTrayRefreshMsg) {
        self->refreshTip(hwnd);
        return 0;
    }

    if (msg == WM_CLOSE) {
        DestroyWindow(hwnd);
        return 0;
    }

    if (msg == WM_DESTROY) {
        PostQuitMessage(0);
        return 0;
    }

    return DefWindowProcW(hwnd, msg, wp, lp);
}

}  // namespace

void TrayIcon::Impl::refreshTip(HWND h) {
    std::string tip;
    {
        std::lock_guard<std::mutex> lock(mu);
        tip = spec.tip;
    }
    const std::wstring wide = utf8ToWide(tip);

    NOTIFYICONDATAW nid{};
    nid.cbSize = sizeof(nid);
    nid.hWnd = h;
    nid.uID = kTrayIconUID;
    nid.uFlags = NIF_TIP;
    wcsncpy_s(nid.szTip, _countof(nid.szTip), wide.c_str(), _TRUNCATE);
    Shell_NotifyIconW(NIM_MODIFY, &nid);
}

void TrayIcon::Impl::showMenu(HWND h) {
    // 先按当前期望状态快照出一份菜单（勾选状态由 Go 决定，这里只是照搬）。
    std::vector<TrayMenuItem> items;
    {
        std::lock_guard<std::mutex> lock(mu);
        items = spec.menu;
    }
    if (items.empty()) return;

    HMENU menu = CreatePopupMenu();
    if (menu == nullptr) return;

    std::vector<std::string> idOf;   // 菜单 id（从 1 开始）→ 命令 id
    idOf.push_back(std::string());   // 占位：id 0 恒为「未选中」

    for (const TrayMenuItem& item : items) {
        if (item.separator) {
            AppendMenuW(menu, MF_SEPARATOR, 0, nullptr);
            continue;
        }
        UINT flags = MF_STRING;
        if (item.checked) flags |= MF_CHECKED;
        if (item.isDefault) flags |= MF_DEFAULT;
        const UINT id = kFirstMenuId + static_cast<UINT>(idOf.size()) - 1;
        AppendMenuW(menu, flags, id, utf8ToWide(item.label).c_str());
        idOf.push_back(item.id);
    }

    POINT pt{};
    GetCursorPos(&pt);

    // ⚠️ 必须先 SetForegroundWindow，否则弹出的菜单收不到点击
    //    （点菜单外面不会关、点菜单项也没反应）——这是 TrackPopupMenu 的经典坑。
    SetForegroundWindow(h);

    const UINT picked = static_cast<UINT>(
        TrackPopupMenu(menu, TPM_RIGHTBUTTON | TPM_RETURNCMD, pt.x, pt.y, 0, h, nullptr));

    // TrackPopupMenu 返回后补一条 WM_NULL，让菜单正常消失（KB135788）。
    // V1 没有这一步，菜单偶尔会残留在屏幕上。
    PostMessageW(h, WM_NULL, 0, 0);

    DestroyMenu(menu);

    if (picked > 0 && picked < idOf.size()) push(idOf[picked]);
}

bool TrayIcon::Impl::attach(Resources& res) {
    HINSTANCE inst = GetModuleHandleW(nullptr);

    // 先把自己的线程 id 发布出去：destroy() 拿它才能把「收摊」消息投到本线程的
    // 消息队列里（那条路不依赖窗口，见 kTrayStopMsg）。
    threadId = GetCurrentThreadId();

    WNDCLASSEXW wc{};
    wc.cbSize = sizeof(wc);
    wc.lpfnWndProc = trayProc;
    wc.hInstance = inst;
    wc.lpszClassName = kTrayClass;
    if (RegisterClassExW(&wc) == 0 && GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
        setError("注册托盘窗口类失败，错误码 " + std::to_string(GetLastError()));
        return false;
    }

    // 必须是**普通顶层窗口**而不是 HWND_MESSAGE 消息窗口：
    // TrackPopupMenu 要求窗口能成为前台窗口，而消息窗口做不到。
    // 它不可见（不调用 ShowWindow）就够了——V1 也是这么做的。
    HWND h = CreateWindowExW(0, kTrayClass, L"ETS2Tray", WS_OVERLAPPED,
                             0, 0, 0, 0, nullptr, nullptr, inst, this);
    if (h == nullptr) {
        setError("创建托盘窗口失败，错误码 " + std::to_string(GetLastError()));
        return false;
    }

    // ⚠️ 窗口句柄要**立刻发布**（res 与 hwnd 都写）。
    //    destroy() 只有看到非空的 hwnd 才会去 PostMessage(WM_CLOSE)；
    //    晚一步发布不会有问题（那种情况由 stopRequested 兜住），但已发布之后
    //    销毁路径就能走「发消息唤醒消息循环」这条更快的路。
    res.hwnd = h;
    hwnd = h;

    res.icon = createTrayIcon();

    std::string tip;
    {
        std::lock_guard<std::mutex> lock(mu);
        tip = spec.tip;
    }
    const std::wstring wideTip = utf8ToWide(tip);

    res.nid.cbSize = sizeof(res.nid);
    res.nid.hWnd = h;
    res.nid.uID = kTrayIconUID;
    res.nid.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP;
    res.nid.uCallbackMessage = kTrayCallbackMsg;
    res.nid.hIcon = (res.icon != nullptr) ? res.icon : LoadIconW(nullptr, IDI_APPLICATION);
    wcsncpy_s(res.nid.szTip, _countof(res.nid.szTip), wideTip.c_str(), _TRUNCATE);

    if (Shell_NotifyIconW(NIM_ADD, &res.nid) == FALSE) {
        // 挂图标失败**不算致命**：窗口已经建好，菜单与命令队列照常可用，
        // 上层从 active()/lastError() 就能看到失败。这里保持原来的行为
        // （照旧进消息循环），只是把原因如实记下来，不再让它变成一条
        // 「直接 return、线程没人收」的路径。
        setError("Shell_NotifyIcon 添加图标失败，错误码 " + std::to_string(GetLastError()));
        return true;
    }

    res.iconAdded = true;
    active = true;
    std::printf("[tray] 托盘图标已添加\n");
    std::fflush(stdout);
    return true;
}

void TrayIcon::Impl::detach(Resources& res) {
    // 顺序不能反：图标还挂在托盘里时就 DestroyIcon，
    // 托盘里会留下一个点不动的空白占位。
    if (res.iconAdded) Shell_NotifyIconW(NIM_DELETE, &res.nid);
    if (res.icon != nullptr) DestroyIcon(res.icon);
    if (res.hwnd != nullptr && IsWindow(res.hwnd)) DestroyWindow(res.hwnd);

    // 对外可见的状态要先清干净：destroy() 在 join 之后还会兜一遍，
    // 但线程自己也得收尾，不能留一个「图标还在、窗口还在」的假象给 active()。
    active = false;
    hwnd = nullptr;
    res.hwnd = nullptr;
    res.icon = nullptr;
    res.iconAdded = false;
}

void TrayIcon::Impl::run() {
    Resources res;

    // ⚠️ 这里**不允许**任何提前 return。
    //
    // 旧写法在建类失败 / 建窗失败时直接 return，线程对象于是永远停在 joinable
    // 状态，却再也没有人会 join 它（destroy() 看到 running==false 就不 join 了），
    // 等到 delete impl_ 时 std::thread 的析构函数直接 std::terminate()。
    // 现象是「托盘创建失败之后，下一次重启托盘、或者退出程序时进程凭空消失」，
    // 而且死在析构里，连一句错误日志都留不下。
    //
    // 所以初始化失败只是**跳过消息循环**，收尾一律走下面这唯一一处 detach()。
    //
    // stopRequested 与 attach 的结果是两件事：前者是「destroy() 已经等在外面，
    // 别进循环了」，后者是「窗口没建起来」。两者都只影响是否跑消息循环。
    if (attach(res) && !stopRequested) {
        MSG msg;
        // 循环条件里带 stopRequested：线程消息（hwnd 为空的 kTrayStopMsg）
        // 被 DispatchMessageW 丢弃后循环会重新判断，这样才能靠它退出来。
        while (!stopRequested && GetMessageW(&msg, nullptr, 0, 0) > 0) {
            TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }
    }

    detach(res);

    // 必须是最后一步：置 false 之后 destroy()/apply() 才认为这个线程已经结束。
    running = false;
}

// ── 对外接口 ────────────────────────────────────────────────

TrayIcon::~TrayIcon() {
    destroy();
    delete impl_;
}

bool TrayIcon::apply(const TraySpec& specIn, std::string& err) {
    err.clear();
    if (impl_ == nullptr) impl_ = new Impl();

    {
        std::lock_guard<std::mutex> lock(impl_->mu);
        impl_->spec = specIn;
        if (specIn.enabled) impl_->error.clear();
    }

    if (!specIn.enabled) {
        destroy();
        return true;
    }

    if (!impl_->running.load()) {
        // ⚠️ 赋新线程之前必须先收掉旧线程对象。
        //
        // std::thread 的 operator= 对 **joinable 的目标对象**会直接
        // std::terminate()（这是标准规定的行为，不是实现细节），而
        // 「托盘创建失败 → 上层再 apply 一次」这条最常见的重启路径恰好会留下
        // 一个 joinable 的线程对象——那时 run() 已经返回、running 已经是 false，
        // 谁都以为它没了，只有 std::thread 自己知道它还挂着一个线程。
        // 于是第二次 apply 就变成进程猝死（0xC0000409），而且日志里什么都看不到。
        //
        // 这里 join 不会卡：running==false 只可能由 run() 的**最后一行**置上，
        // 那时线程除了返回已经没有别的事要做。
        if (impl_->thread.joinable()) {
            impl_->stopRequested = true;
            impl_->thread.join();
        }
        impl_->stopRequested = false;   // 新线程从头开始，别一启动就看见旧的停止请求

        // 托盘线程负责建窗口与挂图标，所以这里只能确认「已发起」。
        // 是否真的挂上、有没有失败，看随后的 active() / lastError()。
        impl_->running = true;
        impl_->thread = std::thread([this] { impl_->run(); });
        return true;
    }

    // 已在运行：只要刷新提示文字即可。菜单内容是每次弹出时现取的，
    // 所以改了菜单不用通知——下一次右键自然就是新的。
    if (HWND h = impl_->hwnd.load()) {
        PostMessageW(h, kTrayRefreshMsg, 0, 0);
    }
    return true;
}

void TrayIcon::destroy() {
    if (impl_ == nullptr) return;

    // ⚠️ 顺序是「先立停止信号，再看 hwnd」，不能反。
    //
    // 反过来的话有一个必然出现的竞态：托盘线程可能正好停在「窗口还没建出来」
    // 的那一小段里，此时没有任何句柄可发消息，而线程紧接着就会进
    // GetMessageW 等一条永远不会来的消息——join() 于是永远不返回，
    // 托盘销毁把整个进程钉死在退出路径上（关不掉、没日志，只能杀进程）。
    //
    // 先置位就消除了它：线程在 attach() 里发布 hwnd **之后**、进消息循环
    // **之前**会读到这个标志，不需要任何人给它发消息就能走到收尾。
    impl_->stopRequested = true;

    if (HWND h = impl_->hwnd.load()) {
        PostMessageW(h, WM_CLOSE, 0, 0);
    }

    // 再补一条**线程消息**：它直接投到托盘线程的消息队列，不经过窗口过程。
    // 常规路径上 WM_CLOSE 就够了；这一条是给「窗口过程不是我们的」那种情况下
    // 兜底的（同名窗口类被别人抢先注册过），否则消息循环醒不过来，
    // 下面的 join() 会一直等下去——而 destroy() 的契约是无条件返回。
    // 队列还没建起来时这次投递会失败，那也没关系：那时线程还没进循环，
    // 它会自己看到上面置的 stopRequested。
    if (DWORD tid = impl_->threadId.load()) {
        PostThreadMessageW(tid, kTrayStopMsg, 0, 0);
    }

    // running 不参与判断：线程完全可能已经失败返回（run() 里 attach 失败那条
    // 路），那时它仍然是 joinable 的——漏掉这次 join，delete impl_ 时就是
    // std::terminate()。所以只要 joinable 就一定 join。
    if (impl_->thread.joinable()) impl_->thread.join();

    impl_->running = false;
    impl_->active = false;
    impl_->hwnd = nullptr;
    impl_->threadId = 0;
}

bool TrayIcon::active() const {
    return impl_ != nullptr && impl_->active.load();
}

std::string TrayIcon::lastError() const {
    if (impl_ == nullptr) return std::string();
    std::lock_guard<std::mutex> lock(impl_->mu);
    return impl_->error;
}

std::vector<std::string> TrayIcon::takeCommands() {
    if (impl_ == nullptr) return {};
    std::lock_guard<std::mutex> lock(impl_->mu);
    std::vector<std::string> out;
    out.swap(impl_->commands);
    return out;
}

bool TrayIcon::hasDesktopSession() {
    return OverlayWindow::hasDesktopSession();
}

}  // namespace tn
