// WebView2 宿主实现。
//
// ⚠️ 本文件只在找到 WebView2 SDK 时参与编译（CMake 的 TN_HAS_WEBVIEW2）。
//    没有 SDK 时下面的接口实现全部走「未编译进显示层」的降级分支。
#include "webview.hpp"

#include <windows.h>

#include <cstdio>
#include <functional>
#include <string>
#include <vector>

#ifdef TN_HAS_WEBVIEW2

#include <objbase.h>
// Toolhelp：退出收尾时要用它数出本进程的 msedgewebview2 子进程（见 shutdownForExit）
#include <tlhelp32.h>
#include <wrl/client.h>  // 只用 ComPtr；回调自己写，见下面的说明

#include <WebView2.h>

namespace tn {

namespace {

using Microsoft::WRL::ComPtr;

std::string hresultText(HRESULT hr) {
    char buf[32];
    std::snprintf(buf, sizeof(buf), "0x%08lX", static_cast<unsigned long>(hr));
    return buf;
}

// 宽字符串 → UTF-8。
//
// ⚠️ 必须转成窄串再 printf，不能直接用 wprintf：C 标准规定每个流有唯一的
//    方向（orientation），首次 printf 之后 stdout 就固定为字节导向，
//    再调 wprintf 会静默失败——日志里表现为「某些行凭空消失」。
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

// ── COM 回调基类 ────────────────────────────────────────────
//
// 为什么手写而不用 Microsoft::WRL::Callback：
//   WRL 的模板确实更省事，但它对参数类型的要求很挑剔，而 WebView2 的
//   `CreateCoreWebView2EnvironmentWithOptions` 在那个模板下经常匹配不上，
//   报错信息又极其难懂。手写这三个回调各约 20 行，换来的是可控与可读。
template <typename TInterface>
class CallbackBase : public TInterface {
public:
    ULONG STDMETHODCALLTYPE AddRef() override {
        return static_cast<ULONG>(InterlockedIncrement(&ref_));
    }

    ULONG STDMETHODCALLTYPE Release() override {
        const ULONG n = static_cast<ULONG>(InterlockedDecrement(&ref_));
        if (n == 0) delete this;
        return n;
    }

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID riid, void** ppv) override {
        if (ppv == nullptr) return E_POINTER;
        if (riid == IID_IUnknown || riid == __uuidof(TInterface)) {
            *ppv = static_cast<TInterface*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }

protected:
    virtual ~CallbackBase() = default;

private:
    LONG ref_ = 1;
};

}  // namespace

// ── 进程内共享的 WebView2 Environment ───────────────────────
//
// ⚠️ 为什么必须是单例：**同一个 user data folder 在一个进程里只能有一个
//    Environment**，第二次创建会失败。而 ADR-013 的两窗口方案要求悬浮窗与
//    设置窗口共用一个——这同时还是 WebView2 的硬性要求：NewWindowRequested
//    里回填的 WebView 必须与发起方在同一个 Environment 上。
//
// 创建是**异步**的，所以「正在创建」期间到达的请求要排队，不能各建各的。

namespace {

struct EnvSlot {
    ComPtr<ICoreWebView2Environment> env;
    bool creating = false;
    bool comInitialized = false;
    std::vector<std::function<void(ICoreWebView2Environment*, HRESULT)>> waiters;
};

EnvSlot& envSlot() {
    // 函数内静态：延后到首次使用才构造，避免在加载期碰 COM。
    static EnvSlot slot;
    return slot;
}

// userDataFolder 必须显式给：默认位置在 exe 旁边，而 exe 若装在
// Program Files 下就没有写权限。用 LOCALAPPDATA 与 Go 侧的配置目录同级。
std::wstring userDataFolder() {
    wchar_t buf[MAX_PATH]{};
    if (GetEnvironmentVariableW(L"LOCALAPPDATA", buf, MAX_PATH) == 0) {
        return std::wstring();
    }
    return std::wstring(buf) + L"\\ETS2 Translator V2\\WebView2";
}

// 环境创建完成 → 唤醒所有等待者。
class EnvReadyHandler
    : public CallbackBase<ICoreWebView2CreateCoreWebView2EnvironmentCompletedHandler> {
public:
    HRESULT STDMETHODCALLTYPE Invoke(HRESULT result,
                                     ICoreWebView2Environment* env) override {
        EnvSlot& slot = envSlot();
        slot.env = env;
        slot.creating = false;

        // 先把等待者搬出来再回调：回调里很可能立刻发起新一轮请求
        // （比如接着去建 Controller），直接遍历原容器会在中途被改。
        auto waiters = std::move(slot.waiters);
        slot.waiters.clear();
        for (auto& w : waiters) w(env, result);
        return S_OK;
    }
};

// 拿到（必要时先创建）共享 Environment 后回调。
// 回调**在本线程**上：要么立刻执行，要么等创建完成后执行。
void withEnvironment(std::function<void(ICoreWebView2Environment*, HRESULT)> cb) {
    EnvSlot& slot = envSlot();

    if (slot.env) {
        cb(slot.env.Get(), S_OK);
        return;
    }

    slot.waiters.push_back(std::move(cb));
    if (slot.creating) return;

    // WebView2 要求 STA。RPC_E_CHANGED_MODE 表示本线程已被初始化成别的模式，
    // 那种情况下不能反初始化（不是我们初始化的）。
    if (!slot.comInitialized) {
        const HRESULT hrInit = CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
        slot.comInitialized = SUCCEEDED(hrInit);
    }

    slot.creating = true;
    const std::wstring userData = userDataFolder();

    auto* handler = new EnvReadyHandler();
    const HRESULT hr = CreateCoreWebView2EnvironmentWithOptions(
        nullptr, userData.empty() ? nullptr : userData.c_str(), nullptr, handler);
    handler->Release();

    if (FAILED(hr)) {
        // 连发起都失败：不能把等待者永远吊着，立刻以失败唤醒。
        slot.creating = false;
        auto waiters = std::move(slot.waiters);
        slot.waiters.clear();
        for (auto& w : waiters) w(nullptr, hr);
    }
}

// 放掉**进程级共享** Environment 的最后一个引用。
//
// 为什么必须单独有这一步（而不是让它跟着某个 WebViewHost 一起走）：Environment
// 的 ComPtr 一直挂在 envSlot 的函数内静态里，**没有任何一处会顺手把它放掉**——
// 于是它就一直活到进程结束，引用计数从来没归过零。进程退出时这一份引用连同
// 进程一起消失，谁也说不清 WebView2 那边有没有机会正常收场。
//
// ⚠️ 别把因果说过头：实测让浏览器进程收场的是**两个 controller 都被关掉**
//    （探针 -Mode reconnect：只关 controller、不放 Environment，浏览器主进程
//    照样换了一个 PID，说明旧的确实退了）。Environment 这一份引用是 **COM 层面**
//    的收尾——不放就是泄漏，而且进程一旦退出再也没人能放。
//
// ⚠️ 只有**进程退出**路径可以调用它。close() 不能调：断线重连要重建窗口，
//    那时必须接着用同一个 Environment（同一 user data folder 在进程内只能有
//    一个 Environment），提前放掉会让重连后的 attach 走一趟失败重来。
void releaseSharedEnvironment() {
    EnvSlot& slot = envSlot();
    slot.env.Reset();
    // 环境已作废：把「正在创建」与等待者一起清掉，让下一次 attach 从头创建一次，
    // 否则后来的等待者会永远挂在一条再也不会有人唤醒的队列上。
    slot.creating = false;
    slot.waiters.clear();
}

// ── 进程退出收尾用的两个小工具 ──────────────────────────────
//
// ⚠️ 为什么自己去数子进程，而不是等一个「Close 完成」的回调：
//    ICoreWebView2Controller::Close() 是同步返回的，**它本身没有完成回调**
//    （SDK 里 ICoreWebView2Controller 只有 Close/CloseDefaultDownloadDialog，
//    没有任何 Closed 类事件）。WebView2 里唯一的「浏览器进程结束」信号是
//    ICoreWebView2Environment5::BrowserProcessExited，而文档对它的描述是
//    「在所有资源（含 user data folder）都释放之后才触发」——我们要等的正是
//    「释放之后」那一刻，拿它当条件既绕又不确定，还得为此再搭一套 COM 回调。
//    这里改成等一个与 COM 生命周期无关的客观信号，见下。
//
// 等的就是**可等待对象本身**：WebView2 的浏览器主进程（msedgewebview2.exe 里
// 不带 --type= 的那个）是本进程的直接子进程（实测：--webview-exe-name 是宿主
// exe 名、mojo 通道名以宿主 PID 开头、PPID 就是宿主）。进程句柄天然可等，
// 它退出了，就说明整棵浏览器进程树都收到了终止信号——这也正是「子进程有没有
// 回收干净」最直接的判据。

// 打开本进程所有 msedgewebview2 直接子进程的句柄（只要 SYNCHRONIZE，够 Wait 用）。
//
// 句柄必须**在关闭 WebView 之前**取：关闭动作一开始，浏览器进程随时可能消失，
// 那时再去数就什么都找不到，也就无从「等它退出」。
std::vector<HANDLE> openWebViewChildHandles() {
    std::vector<HANDLE> handles;

    const HANDLE snap = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
    if (snap == INVALID_HANDLE_VALUE) return handles;

    const DWORD self = GetCurrentProcessId();
    PROCESSENTRY32W entry{};
    entry.dwSize = sizeof(entry);

    if (Process32FirstW(snap, &entry)) {
        do {
            if (entry.th32ParentProcessID != self) continue;
            // 单 Environment 只有一个浏览器主进程；crashpad-handler 等如果也是
            // 直接子进程就一并等——它们同样该在收尾时消失，多等一个无害。
            if (lstrcmpiW(entry.szExeFile, L"msedgewebview2.exe") != 0) continue;

            const HANDLE h = OpenProcess(SYNCHRONIZE, FALSE, entry.th32ProcessID);
            if (h != nullptr) handles.push_back(h);
            // 上限保护：MsgWaitForMultipleObjects 一次最多等 MAXIMUM_WAIT_OBJECTS 个
            if (handles.size() >= 16) break;
        } while (Process32NextW(snap, &entry));
    }
    CloseHandle(snap);
    return handles;
}

void closeHandles(std::vector<HANDLE>& handles) {
    for (HANDLE h : handles) CloseHandle(h);
    handles.clear();
}

// 等这批子进程全部退出，最多 timeoutMs 毫秒；返回 false 表示超时。
//
// ⚠️ 用 MsgWaitForMultipleObjects 而不是 WaitForSingleObject：WebView2 的收尾
//    有一部分（销毁 WebView 自己的子窗口、处理窗口销毁通知）要宿主线程继续泵
//    消息，只等不泵有可能永远等不到。超时是硬性的——退出路径绝不允许卡死。
bool waitForChildrenExit(const std::vector<HANDLE>& handles, unsigned timeoutMs) {
    if (handles.empty()) return true;   // 本来就没有子进程（没建过 WebView）

    const DWORD start = GetTickCount();
    for (;;) {
        // 先探一次：已经全部退出就立刻返回，不白等一轮
        bool allExited = true;
        for (HANDLE h : handles) {
            if (WaitForSingleObject(h, 0) == WAIT_TIMEOUT) {
                allExited = false;
                break;
            }
        }
        if (allExited) return true;

        const DWORD used = GetTickCount() - start;
        if (used >= timeoutMs) return false;

        const DWORD w = MsgWaitForMultipleObjects(static_cast<DWORD>(handles.size()),
                                                  handles.data(), TRUE,
                                                  timeoutMs - used, QS_ALLINPUT);
        if (w == WAIT_TIMEOUT) return false;
        // 句柄出了问题（极少见）就别赖在退出路径上，按「等不到」返回
        if (w == WAIT_FAILED) return false;

        MSG msg;
        while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
            TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }
    }
}


}  // namespace

// ── 实现体 ──────────────────────────────────────────────────

struct WebViewHost::Impl {
    // 这里曾经有个 kResizeInset（把 WebView 四周内缩 6px，把边缘让给宿主窗口
    // 做鼠标缩放）。实测**行不通**，已彻底移除，留此记录免得有人再试一遍：
    //
    //   WebView2 的子窗口 HWND 始终铺满整个客户区，put_Bounds 只裁剪**绘制**、
    //   不裁剪**命中测试**。把光标停在窗口四边，WindowFromPoint 依然返回
    //   Chrome_RenderWidgetHostHWND，指针也始终是 IDC_ARROW。
    //   那圈内缩只换来一圈难看的背景色，一点缩放能力都没换到。
    //
    // 缩放与拖动因此改由前端监听最外圈 mousedown、经 IPC 交给
    // OverlayWindow::beginMoveResize 处理（见 window.hpp 的说明）。
    // WebView 铺满整个窗口，与 V1 的观感一致。

    void* hwnd = nullptr;              // HWND（父窗口）
    ComPtr<ICoreWebView2Environment> env;   // 指向**进程内共享**的那个（见 envSlot）
    ComPtr<ICoreWebView2Controller> controller;
    ComPtr<ICoreWebView2> webview;

    // ── 第二个窗口（设置窗口）的 WebView ──
    //
    // 由 NewWindowRequested 触发创建。用的是**同一个 Environment**——
    // 这是 WebView2 的硬性要求，也是 ADR-013 两窗口方案的依据。
    ComPtr<ICoreWebView2Controller> childController;
    ComPtr<ICoreWebView2> childWebview;
    ChildWindowProvider childProvider;
    int childWidth = 0;
    int childHeight = 0;

    std::wstring url;
    std::string lastError;
    bool attaching = false;
    bool comInitialized = false;
    int width = 0;
    int height = 0;

    void setError(const std::string& what, HRESULT hr) {
        lastError = what + "（" + hresultText(hr) + "）";
        std::printf("[webview] %s\n", lastError.c_str());
        std::fflush(stdout);
    }

    void syncBounds() {
        if (!controller || width <= 0 || height <= 0) return;
        // 满幅（不留内缩，见本结构体开头的说明）
        const RECT r{0, 0, width, height};
        controller->put_Bounds(r);
    }

    void syncChildBounds() {
        if (!childController || childWidth <= 0 || childHeight <= 0) return;
        const RECT r{0, 0, childWidth, childHeight};
        childController->put_Bounds(r);
    }

    // 共享 Environment 就绪（或失败）后的续作：在宿主窗口上建 controller。
    // 定义放在后面——它要用到 ControllerCompletedHandler，而那是个 COM 回调类。
    void onEnvironmentReady(ICoreWebView2Environment* envIn, HRESULT hr);
};

namespace {

// 注册 window.open 的处理器。定义放在本文件靠后的位置——它要用到
// NewWindowHandler，而那个类定义在后面。
void registerNewWindowHandler(WebViewHost::Impl* owner);

// 环境就绪 → 在宿主窗口上创建 controller。
//
// 这里**不再**负责创建 Environment：Environment 现在是进程内共享的
// （见上面的 envSlot），创建与排队统一由 withEnvironment 处理。
class ControllerCompletedHandler
    : public CallbackBase<ICoreWebView2CreateCoreWebView2ControllerCompletedHandler> {
public:
    explicit ControllerCompletedHandler(WebViewHost::Impl* owner) : owner_(owner) {}

    HRESULT STDMETHODCALLTYPE Invoke(HRESULT result,
                                     ICoreWebView2Controller* controller) override {
            if (FAILED(result) || controller == nullptr) {
                owner_->setError("创建 WebView2 控制器失败", result);
                owner_->attaching = false;
                return S_OK;
            }

            owner_->controller = controller;
            if (FAILED(controller->get_CoreWebView2(&owner_->webview)) || !owner_->webview) {
                owner_->setError("获取 CoreWebView2 失败", E_FAIL);
                owner_->attaching = false;
                return S_OK;
            }

            // 透明背景：Mica/Acrylic 材质在窗口层，WebView 若铺不透明底色
            // 就会把它整个盖住——和之前 CSS 用纯黑底是同一个问题，只是这次
            // 发生在 WebView 这一层。
            ComPtr<ICoreWebView2Controller2> ctrl2;
            if (SUCCEEDED(controller->QueryInterface(IID_PPV_ARGS(&ctrl2))) && ctrl2) {
                COREWEBVIEW2_COLOR transparent{0, 0, 0, 0};
                ctrl2->put_DefaultBackgroundColor(transparent);
            }

            // 默认关掉浏览器自带的那套辅助（右键菜单、缩放、状态栏）。
            // 悬浮窗的右键菜单是我们自己画的（与 V1 一致，只有 Settings/Exit）。
            ComPtr<ICoreWebView2Settings> settings;
            if (SUCCEEDED(owner_->webview->get_Settings(&settings)) && settings) {
                settings->put_AreDefaultContextMenusEnabled(FALSE);
                settings->put_IsStatusBarEnabled(FALSE);
                settings->put_AreDevToolsEnabled(FALSE);
                settings->put_IsZoomControlEnabled(FALSE);

                // ── 「非客户区支持」（app-region: drag）——试过，本机不生效，已关闭 ──
                //
                // 这条路本来是首选：官方文档说开启后网页里标了
                // `app-region: drag` 的区域会被当成窗口标题栏，能拖动窗口、
                // 右键弹系统菜单。实测在本机（运行时 152.0.4191.66）：
                //   put hr=0x00000000  回读 hr=0x00000000 当前值=TRUE
                //   设置确实生效、CSS 也确实在产物里，但真实鼠标拖动
                //   窗口位移恒为 (0,0)——不生效。社区反馈里还要额外的
                //   `--enable-features=msWebView2EnableDraggableRegions`。
                //
                // 结论：不再依赖这个特性。拖动与缩放统一走
                // 前端 mousedown → IPC → OverlayWindow::beginMoveResize
                // （见 window.hpp）。显式关掉它而不是留着，是为了**行为确定**：
                // 一个「有时生效」的机制会让我们自己的实现和它打架，
                // 那种 bug 极难查。
                ComPtr<ICoreWebView2Settings9> settings9;
                if (SUCCEEDED(settings->QueryInterface(IID_PPV_ARGS(&settings9))) && settings9) {
                    const HRESULT hrPut = settings9->put_IsNonClientRegionSupportEnabled(FALSE);
                    BOOL nowOn = TRUE;
                    const HRESULT hrGet = settings9->get_IsNonClientRegionSupportEnabled(&nowOn);
                    std::printf("[webview] 非客户区支持(app-region): 已显式关闭 put hr=%s 回读 hr=%s 当前值=%s\n",
                                hresultText(hrPut).c_str(), hresultText(hrGet).c_str(),
                                nowOn ? "TRUE" : "FALSE");
                    std::fflush(stdout);
                } else {
                    std::printf("[webview] 当前 WebView2 运行时没有 Settings9 接口（不影响使用）\n");
                    std::fflush(stdout);
                }
            }

            // 新窗口请求：前端用 window.open('?page=settings') 开设置窗口。
            // WebView2 默认会弹一个自己的窗口，那不受我们控制；这里挂上处理器，
            // 由它用**共享的 Environment** 创建第二个窗口并回填 NewWindow（ADR-013）。
            //
            // 注册动作抽成自由函数是因为 NewWindowHandler 定义在下面
            // （它要用到 ChildControllerHandler），这里放不下。
            registerNewWindowHandler(owner_);

            owner_->syncBounds();
            owner_->controller->put_IsVisible(TRUE);

            const HRESULT navHr = owner_->webview->Navigate(owner_->url.c_str());
            if (FAILED(navHr)) {
                owner_->setError("导航失败", navHr);
            } else {
                std::printf("[webview] 已导航: %s\n", wideToUtf8(owner_->url).c_str());
                std::fflush(stdout);
            }

            owner_->attaching = false;
            return S_OK;
        }

    private:
        WebViewHost::Impl* owner_;
    };

// 设置窗口的 controller 创建完成 → 回填 NewWindow。
//
// ⚠️ 文档里的三条硬约束（CoreWebView2NewWindowRequestedEventArgs.NewWindow）：
//    1. NewWindow 里给的 WebView **必须与发起方在同一个 Environment 上**；
//    2. 它 **不能自己去 Navigate**——WebView2 会把它导航到请求的 URI；
//    3. 所有 settings 必须在 put_NewWindow **之前**设完，否则不会对新
//       WebView 生效。
//    三条都已按此实现，改这个类之前请先回看文档。
class ChildControllerHandler
    : public CallbackBase<ICoreWebView2CreateCoreWebView2ControllerCompletedHandler> {
public:
    ChildControllerHandler(WebViewHost::Impl* owner,
                           ICoreWebView2NewWindowRequestedEventArgs* args,
                           ICoreWebView2Deferral* deferral,
                           void* hwnd,
                           const std::wstring& navigateTo)
        : owner_(owner), args_(args), deferral_(deferral), hwnd_(hwnd),
          navigateTo_(navigateTo) {}

    HRESULT STDMETHODCALLTYPE Invoke(HRESULT result,
                                     ICoreWebView2Controller* controller) override {
        if (FAILED(result) || controller == nullptr) {
            fail("创建设置窗口的 WebView2 控制器失败", result);
            return S_OK;
        }

        owner_->childController = controller;
        if (FAILED(controller->get_CoreWebView2(&owner_->childWebview)) ||
            !owner_->childWebview) {
            owner_->childController.Reset();
            fail("设置窗口获取 CoreWebView2 失败", E_FAIL);
            return S_OK;
        }

        // 设置窗口是**普通窗口**，不是浮层：不透明底色。
        ComPtr<ICoreWebView2Controller2> ctrl2;
        if (SUCCEEDED(controller->QueryInterface(IID_PPV_ARGS(&ctrl2))) && ctrl2) {
            const COREWEBVIEW2_COLOR opaque{0xFF, 0xFF, 0xFF, 0xFF};
            ctrl2->put_DefaultBackgroundColor(opaque);
        }

        // 设置窗口**保留**浏览器默认右键菜单：那里面有输入框，用户要能
        // 右键复制/粘贴。悬浮窗那边才需要把它关掉（自绘菜单）。
        ComPtr<ICoreWebView2Settings> settings;
        if (SUCCEEDED(owner_->childWebview->get_Settings(&settings)) && settings) {
            settings->put_AreDefaultContextMenusEnabled(TRUE);
            settings->put_IsStatusBarEnabled(FALSE);
            settings->put_AreDevToolsEnabled(FALSE);
        }

        // 摆正 bounds：窗口尺寸在宿主建窗时就定了，按客户区实际大小来。
        RECT rc{};
        if (GetClientRect(static_cast<HWND>(hwnd_), &rc)) {
            const RECT bounds{0, 0, rc.right - rc.left, rc.bottom - rc.top};
            controller->put_Bounds(bounds);
            owner_->childWidth = rc.right - rc.left;
            owner_->childHeight = rc.bottom - rc.top;
        }
        controller->put_IsVisible(TRUE);

        // ⚠️ 这里**不要** Navigate（见类头注释第 2 条）。
        if (args_) {
            args_->put_NewWindow(owner_->childWebview.Get());
            args_->put_Handled(TRUE);
        } else if (!navigateTo_.empty()) {
            // 走 window.open 以外的路径时（托盘菜单的「设置」），没有人替我们
            // 导航——WebView2 只在 put_NewWindow 那条路上代劳。
            owner_->childWebview->Navigate(navigateTo_.c_str());
        }
        if (deferral_) deferral_->Complete();

        std::printf("[webview] 设置窗口已就绪（与悬浮窗共享同一个 Environment）\n");
        std::fflush(stdout);
        return S_OK;
    }

private:
    void fail(const std::string& what, HRESULT hr) {
        owner_->setError(what, hr);
        if (args_) args_->put_Handled(TRUE);   // 不让 WebView2 弹它自己的窗口
        if (deferral_) deferral_->Complete();
    }

    WebViewHost::Impl* owner_;
    ComPtr<ICoreWebView2NewWindowRequestedEventArgs> args_;
    ComPtr<ICoreWebView2Deferral> deferral_;
    void* hwnd_ = nullptr;   // HWND
    std::wstring navigateTo_;  // 非空表示「这条路径没人代我们导航」
};

// window.open 的处理：**真正**创建第二个窗口。
//
// 以前这里只是 put_Handled(TRUE) 就返回，什么都不做——结果是网页里
// window.open 拿到 null，前端只能弹一句「浏览器拦截了新窗口」的误导提示，
// 设置入口彻底断掉。现在改成：向宿主索取承载窗口 → 用共享 Environment 建
// Controller → 回填 NewWindow。
class NewWindowHandler : public CallbackBase<ICoreWebView2NewWindowRequestedEventHandler> {
public:
    explicit NewWindowHandler(WebViewHost::Impl* owner) : owner_(owner) {}

    HRESULT STDMETHODCALLTYPE Invoke(ICoreWebView2* sender,
                                     ICoreWebView2NewWindowRequestedEventArgs* args) override {
        (void)sender;
        if (args == nullptr) return S_OK;

        if (!owner_->childProvider) {
            // 宿主没提供承载窗口的能力。与其弹一个不受控的浏览器窗口，
            // 不如如实拒绝并在日志里说明。
            args->put_Handled(TRUE);
            std::printf("[webview] 收到新窗口请求，但宿主未提供承载窗口，已取消\n");
            std::fflush(stdout);
            return S_OK;
        }

        std::wstring uri;
        {
            LPWSTR raw = nullptr;
            if (SUCCEEDED(args->get_Uri(&raw)) && raw != nullptr) {
                uri.assign(raw);
                CoTaskMemFree(raw);
            }
        }

        // 设置窗口已经开着：**复用**它的 WebView，不再建第二个。
        // 不复用的话，第二次 window.open 会走到下面「已有 childWebview」
        // 的分支之外而拿不到 NewWindow，前端又只能报错。
        if (owner_->childWebview) {
            if (owner_->childController) owner_->childController->put_IsVisible(TRUE);

            // ⚠️ 必须**同时让宿主把窗口抬到前面**——这一步以前漏掉了。
            //
            // 这条早退分支原先只设了个 IsVisible 就返回，**从不调用 childProvider**；
            // 而「抬到所有置顶窗口之上」正是写在 provideSettingsWindow 的
            // 「窗口已存在」分支里。于是出现一个很怪的现象：
            //   第一次点「设置」→ 走创建路径 → 窗口正常出现
            //   之后再点「设置」→ 走这条复用分支 → 窗口纹丝不动地待在游戏后面
            // 用户看到的就是「点设置没反应」。实测报障步骤完全吻合：
            // 先开设置 → 点游戏让它到最前 → 再点设置 → 浮不上来。
            //
            // 返回值不用接：窗口和 WebView 都已经有了，这里只需要那一下抬升。
            owner_->childProvider(uri);

            args->put_NewWindow(owner_->childWebview.Get());
            args->put_Handled(TRUE);
            std::printf("[webview] 设置窗口已存在，复用现有 WebView 并抬到前面\n");
            std::fflush(stdout);
            return S_OK;
        }

        // 向宿主索取承载用的 HWND。这一步是同步的：宿主负责建窗口并显示。
        void* childHwnd = owner_->childProvider(uri);
        if (childHwnd == nullptr) {
            args->put_Handled(TRUE);
            std::printf("[webview] 宿主拒绝创建新窗口，已取消\n");
            std::fflush(stdout);
            return S_OK;
        }

        // ⚠️ 必须取 deferral：建 Controller 是异步的，而 put_NewWindow 必须在
        //    本事件处理返回之前完成。deferral 就是把处理「延长」到异步结束。
        ComPtr<ICoreWebView2Deferral> deferral;
        args->GetDeferral(&deferral);

        auto* handler = new ChildControllerHandler(owner_, args, deferral.Get(), childHwnd,
                                                   std::wstring());
        const HRESULT hr = owner_->env->CreateCoreWebView2Controller(
            static_cast<HWND>(childHwnd), handler);
        handler->Release();

        if (FAILED(hr)) {
            owner_->setError("发起设置窗口的 WebView2 控制器创建失败", hr);
            args->put_Handled(TRUE);
            if (deferral) deferral->Complete();
        }
        return S_OK;
    }

private:
    WebViewHost::Impl* owner_;
};

// 注册 window.open 的处理器。
void registerNewWindowHandler(WebViewHost::Impl* owner) {
    auto* handler = new NewWindowHandler(owner);
    EventRegistrationToken token{};
    owner->webview->add_NewWindowRequested(handler, &token);
    handler->Release();
}

}  // namespace

// Impl::onEnvironmentReady 的类外定义。
//
// ⚠️ 必须写在匿名命名空间**外面**：它是 tn::WebViewHost::Impl 的成员函数，
//    而匿名命名空间不是 tn::WebViewHost 的外围命名空间，写在里面 MSVC 会直接
//    报 C2888。放在这里是因为要用到 ControllerCompletedHandler。
void WebViewHost::Impl::onEnvironmentReady(ICoreWebView2Environment* envIn, HRESULT hr) {
    if (FAILED(hr) || envIn == nullptr) {
        setError("创建 WebView2 环境失败（WebView2 运行时可能未安装）", hr);
        attaching = false;
        return;
    }
    env = envIn;   // 缓存一份指向共享 Environment 的引用

    auto* ctrlHandler = new ControllerCompletedHandler(this);
    const HRESULT chr = envIn->CreateCoreWebView2Controller(
        static_cast<HWND>(hwnd), ctrlHandler);
    if (FAILED(chr)) {
        setError("创建 WebView2 控制器失败", chr);
        attaching = false;
    }
    ctrlHandler->Release();
}

// ── 对外接口 ────────────────────────────────────────────────

WebViewHost::WebViewHost() : impl_(new Impl()) {}

WebViewHost::~WebViewHost() {
    close();
    delete impl_;
}

bool WebViewHost::compiled() { return true; }

bool WebViewHost::attach(void* hwnd, const std::wstring& url, std::string& err) {
    if (impl_->controller) {
        // 已经建好：换个地址重新导航即可，不必重建整个 WebView
        if (impl_->webview) {
            impl_->url = url;
            impl_->webview->Navigate(url.c_str());
        }
        return true;
    }

    impl_->hwnd = hwnd;
    impl_->url = url;
    impl_->lastError.clear();
    impl_->attaching = true;

    // Environment 是**进程内共享**的：设置窗口的 WebView 必须和悬浮窗在同一个
    // Environment 上（WebView2 的硬性要求），而且同一个 user data folder 在一个
    // 进程里也只能有一个 Environment。所以这里不自己建，统一交给 withEnvironment。
    //
    // ⚠️ 回调里只捕获 impl_（稳定指针），**不能**捕获 attach 的局部变量：
    //    环境还在创建时回调会被排队，等到之后的消息循环才执行，那时局部变量
    //    早就没了。曾经差点这么写，是个货真价实的悬垂引用。
    withEnvironment([impl = impl_](ICoreWebView2Environment* env, HRESULT hr) {
        impl->onEnvironmentReady(env, hr);
    });

    // 排队等待时 attaching 仍为 true（= 已发起，稍后完成）；
    // 同步失败时 onEnvironmentReady 已经把它置回 false。
    // controller 未建好是正常的（controller 创建本身总是异步的）。
    if (!impl_->attaching && impl_->controller == nullptr) {
        err = impl_->lastError;
        return false;
    }
    return true;
}

void WebViewHost::setBounds(int width, int height) {
    // 尺寸没变就不动：主循环每轮都会调一次，而 put_Bounds 是跨进程的 COM 调用，
    // 无脑调用纯属浪费。加了这层判断后调用方就不必自己记上次的尺寸。
    if (width == impl_->width && height == impl_->height) return;
    impl_->width = width;
    impl_->height = height;
    impl_->syncBounds();
}

void WebViewHost::show() {
    if (impl_->controller) impl_->controller->put_IsVisible(TRUE);
}

void WebViewHost::hide() {
    if (impl_->controller) impl_->controller->put_IsVisible(FALSE);
}

void WebViewHost::close() {
    closeChild();
    if (impl_->controller) {
        impl_->controller->Close();
        impl_->controller.Reset();
    }
    impl_->webview.Reset();
    impl_->env.Reset();
    impl_->hwnd = nullptr;
    impl_->attaching = false;
    // 注意：**不**在这里 CoUninitialize。COM 的初始化现在归 envSlot 所有
    // （Environment 是进程内共享的，生命周期不跟着某一个 WebViewHost 走），
    // 在这里反初始化会把共享 Environment 一起弄坏。
}

// 进程退出前的显式收尾。四步的**次序是有含义的**，逐条理由见 webview.hpp
// 的声明处，这里只强调踩过的那一点：
//
// ⚠️ 两个窗口共享同一个 Environment（ADR-013）。所以：
//    · 关其中一个 controller 时**不能**顺手把 Environment 也放掉——另一个窗口
//      还在用它，那会让它变成一个再也用不了的壳；
//    · 次序只有一条：**先把所有 controller 关掉，最后才释放 Environment**。
bool WebViewHost::shutdownForExit(unsigned timeoutMs) {
    // ① 先取子进程句柄——必须在关闭之前（理由见 openWebViewChildHandles）。
    std::vector<HANDLE> children = openWebViewChildHandles();

    // ② 关掉全部 controller：先是设置窗口的，再是悬浮窗的。
    closeChild();                        // 设置窗口的 controller
    if (impl_->controller) {             // 悬浮窗的 controller
        impl_->controller->Close();
        impl_->controller.Reset();
    }
    impl_->webview.Reset();
    impl_->hwnd = nullptr;
    impl_->attaching = false;

    // ③ 最后释放**进程级共享** Environment 的引用。
    //    impl_->env 是本对象缓存的那一份，envSlot 里那一份才是「进程还活着就
    //    一直有效」的那份，两份都要放掉，引用计数才会归零。
    impl_->env.Reset();
    releaseSharedEnvironment();

    // ④ 显式等它退出。这里**不是**用固定 Sleep 硬等：超时由 waitForChildrenExit
    //    控制，且等待期间照常泵消息；超时了也照样往下走，只是把事实记进日志。
    const DWORD start = GetTickCount();
    const bool exited = waitForChildrenExit(children, timeoutMs);
    const DWORD used = GetTickCount() - start;

    if (children.empty()) {
        std::printf("[webview] 退出收尾：没有 WebView2 子进程需要等待\n");
    } else if (exited) {
        std::printf("[webview] 退出收尾：%zu 个 WebView2 子进程已退出（用时 %lu ms）\n",
                    children.size(), static_cast<unsigned long>(used));
    } else {
        // 超时不能把退出卡死，但必须如实说出来：残留的那几个只能等浏览器进程
        // 自己发现宿主消失才收场——那正是本次修复要避免依赖的那条路。
        std::printf("[webview] 退出收尾：等待 WebView2 子进程退出超时"
                    "（已等 %lu ms，%zu 个仍在），按超时继续退出\n",
                    static_cast<unsigned long>(used), children.size());
    }
    std::fflush(stdout);

    closeHandles(children);
    return exited;
}

void WebViewHost::setChildWindowProvider(ChildWindowProvider provider) {
    impl_->childProvider = std::move(provider);
}
bool WebViewHost::childReady() const { return impl_->childWebview != nullptr; }

void WebViewHost::setChildBounds(int width, int height) {
    // 与 setBounds 同理：尺寸没变就不动，put_Bounds 是跨进程的 COM 调用。
    if (width == impl_->childWidth && height == impl_->childHeight) return;
    impl_->childWidth = width;
    impl_->childHeight = height;
    impl_->syncChildBounds();
}

void WebViewHost::closeChild() {
    if (impl_->childController) {
        impl_->childController->Close();
        impl_->childController.Reset();
    }
    impl_->childWebview.Reset();
    impl_->childWidth = 0;
    impl_->childHeight = 0;
}

bool WebViewHost::openChildWindow(const std::wstring& url, std::string& err) {
    err.clear();

    if (!impl_->childProvider) {
        err = "宿主未提供承载窗口";
        return false;
    }

    // 已经开着：provider 里会把它提到前面，不重建 WebView
    void* host = impl_->childProvider(url);
    if (host == nullptr) {
        err = "宿主拒绝创建窗口";
        return false;
    }
    if (impl_->childWebview) return true;

    if (!impl_->env) {
        err = "WebView2 环境尚未就绪";
        return false;
    }

    // args 与 deferral 传空：这条路径上没有 window.open 事件要回填，
    // 也没有 deferral 要完成。导航改由 navigateTo 参数负责。
    auto* handler = new ChildControllerHandler(impl_, nullptr, nullptr, host, url);
    const HRESULT hr = impl_->env->CreateCoreWebView2Controller(
        static_cast<HWND>(host), handler);
    handler->Release();

    if (FAILED(hr)) {
        err = "发起设置窗口的 WebView2 控制器创建失败";
        return false;
    }
    return true;
}

bool WebViewHost::ready() const { return impl_->controller != nullptr; }
bool WebViewHost::attaching() const { return impl_->attaching; }
const std::string& WebViewHost::lastError() const { return impl_->lastError; }
std::wstring WebViewHost::url() const { return impl_->url; }

}  // namespace tn

#else  // !TN_HAS_WEBVIEW2
// ── 未编译进显示层 ──────────────────────────────────────────
//
// 这些空实现存在的意义：让调用方（dispatch / main）不必到处写 #ifdef。
// 能力清单由 compiled() 决定，所以不会出现「报了 webview 但用不了」的情况。

namespace tn {

struct WebViewHost::Impl {};

WebViewHost::WebViewHost() : impl_(nullptr) {}
WebViewHost::~WebViewHost() = default;

bool WebViewHost::compiled() { return false; }

bool WebViewHost::attach(void*, const std::wstring&, std::string& err) {
    err = "本次构建未包含 WebView2 支持（未找到 SDK）";
    return false;
}

void WebViewHost::setBounds(int, int) {}
void WebViewHost::show() {}
void WebViewHost::hide() {}
void WebViewHost::close() {}

// 没有显示层就没有 WebView2 子进程要等：直接报「已经收干净了」。
// 不能返回 false——调用方会把 false 记成「等待超时」，那是假告警。
bool WebViewHost::shutdownForExit(unsigned) { return true; }

void WebViewHost::setChildWindowProvider(ChildWindowProvider) {}
bool WebViewHost::childReady() const { return false; }
void WebViewHost::setChildBounds(int, int) {}
void WebViewHost::closeChild() {}

bool WebViewHost::openChildWindow(const std::wstring&, std::string& err) {
    err = "本次构建未包含 WebView2 支持（未找到 SDK）";
    return false;
}

bool WebViewHost::ready() const { return false; }
bool WebViewHost::attaching() const { return false; }
const std::string& WebViewHost::lastError() const {
    static const std::string kNotCompiled = "未编译进显示层";
    return kNotCompiled;
}
std::wstring WebViewHost::url() const { return std::wstring(); }

}  // namespace tn

#endif  // TN_HAS_WEBVIEW2
