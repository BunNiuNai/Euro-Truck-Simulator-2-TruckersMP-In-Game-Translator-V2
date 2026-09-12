// WebView2 宿主 —— 显示层的实现（ADR-013）。
//
// 职责边界：
//   · native 拥有窗口与 WebView，前端退化为纯 SPA（没有 Wails/Tauri 的私有绑定）
//   · 前端资源由 Go 经本机 HTTP 提供，native 只负责「把这个 URL 显示在这个窗口里」
//   · 两个窗口（悬浮窗 / 设置）共享**同一个 WebView2 Environment**，
//     避免开出两套浏览器进程树
//
// ⚠️ WebView2 的创建是**异步**的：
//   attach() 返回 true 只表示「已发起」。真正的完成后 ready() 才变 true，
//   而那一刻发生在消息泵处理 COM 回调的时候。所以调用方不能在这里等，
//   也不能在返回后立刻假设 WebView 可用——必须先查 ready()。
//
// ⚠️ SDK 不是构建硬依赖（ADR-013 补充）：没有 SDK 时本模块整个不参与编译，
//   compiled() 返回 false，能力清单里也不会出现 "webview"。
//   native 进程照常工作，只是没有显示层。
#ifndef TN_WEBVIEW_HPP
#define TN_WEBVIEW_HPP

#include <functional>
#include <string>

namespace tn {

class WebViewHost {
public:
    // 实现体的前置声明。**必须是 public**：COM 回调类在类外，
    // 要持有 Impl* 才能把异步结果写回去；放 private 它们就引用不到这个名字。
    // 内容仍然只在 .cpp 里定义，外部看不到任何 WebView2 的类型。
    struct Impl;

    WebViewHost();
    ~WebViewHost();
    WebViewHost(const WebViewHost&) = delete;
    WebViewHost& operator=(const WebViewHost&) = delete;

    // 在 hwnd 上创建 WebView 并导航到 url。
    // 返回 false 时 err 说明原因（多是 SDK 缺失或 COM 初始化失败）。
    bool attach(void* hwnd, const std::wstring& url, std::string& err);

    // 同步 WebView 的尺寸（窗口 resize 时必须调用，否则内容不跟着变）
    void setBounds(int width, int height);

    void show();
    void hide();
    void close();

    // ── 进程退出前的显式收尾 ─────────────────────────────────
    //
    // 与 close() 的分工（这个区别是**本质**的，不要合并成一个）：
    //   · close()   只拆本对象的两个 controller，**不碰**进程级共享 Environment
    //               —— 断线重连要重建窗口，那时还得接着用同一个 Environment
    //               （同一个 user data folder 在进程内只能有一个，见 .cpp）
    //   · 本函数    是「本进程要退出了」专用的收尾，顺序固定：
    //               ① 先取到 WebView2 子进程句柄（必须在关闭之前，见 .cpp 说明）
    //               ② 关掉**全部** controller（设置窗口的在前，悬浮窗的在后）
    //               ③ 最后释放共享 Environment 的引用（两份都要放，见 .cpp：
    //                  它是进程级单例，没有任何一处会顺手放掉它）
    //               ④ 显式等子进程退出：事件 + 超时，绝不无限等
    //
    // 返回 true 表示在 timeoutMs 内观察到 WebView2 子进程已退出。
    // 超时返回 false，调用方应如实记日志后继续退出——不能因此卡死。
    //
    // 为什么要显式做，而不是靠进程退出让系统回收：WebView2 的浏览器进程是
    // **独立进程**，进程一 ExitProcess，我们持有的那些 WebView2 对象就随进程
    // 一起消失，回收与否全看对方何时发现宿主没了——实测这条 OS 路径在本机也
    // 能收回（强杀后 <1s），但它是**时机**而不是保证，而且事后无从判断：
    // 留下的进程只会让人对着进程表猜。显式收尾的真正价值是这件事终于**有时序
    // 和日志**（"N 个 WebView2 子进程已退出（用时 X ms）" 或明确的超时告警）。
    bool shutdownForExit(unsigned timeoutMs);

    // WebView 是否已就绪（环境与 controller 都创建完成）
    bool ready() const;

    // 是否已发起创建但还没完成——用于区分「还没建」「正在建」「建好了」
    bool attaching() const;

    const std::string& lastError() const;

    // 当前加载的 URL（诊断用）
    std::wstring url() const;

    // ── 第二个窗口（设置窗口）────────────────────────────────
    //
    // 前端用 window.open('?page=settings') 开设置窗口，WebView2 会发
    // NewWindowRequested。**必须由 native 真的创建那个窗口并回填 NewWindow**，
    // 否则网页里的 window.open 返回 null，前端只能报错，设置入口就断了。
    //
    // 两个窗口共用**同一个 WebView2 Environment**——这既是 ADR-013 的要求，
    // 也是 WebView2 的硬性规定（回填的 WebView 必须与发起方同 Environment）。

    // 向宿主索取「用来承载新窗口的 HWND」，参数是新窗口要打开的 URL。
    // 返回 nullptr 表示宿主拒绝（这次 window.open 会被取消，不会弹野窗口）。
    //
    // 宿主负责创建/复用窗口并让它可见、负责窗口关闭时调 closeChild()；
    // 本类只往那个 HWND 里放一个 WebView。
    using ChildWindowProvider = std::function<void*(const std::wstring& uri)>;
    void setChildWindowProvider(ChildWindowProvider provider);

    // 第二个窗口的 WebView 是否已就绪
    bool childReady() const;

    // 同步第二个窗口 WebView 的尺寸（设置窗口 resize 时必须调用）
    void setChildBounds(int width, int height);

    // **不经过 window.open** 直接打开第二个窗口并导航到 url。
    //
    // 托盘菜单的「设置」需要它：那条路径上没有人调用 window.open，
    // 自然也没有 NewWindowRequested 事件可以蹭。窗口本身仍由宿主提供的
    // provider 创建（复用同一套窗口形态）。
    //
    // 若第二个窗口已经开着，本函数只让它前置，不重建 WebView。
    bool openChildWindow(const std::wstring& url, std::string& err);

    // 关闭并释放第二个窗口的 WebView。宿主销毁设置窗口后必须调用它，
    // 否则再次 window.open 会复用一个已经失效的 WebView。
    void closeChild();

    // 本次**构建**是否包含 WebView2 支持（编译期决定，不是运行期探测）
    static bool compiled();

private:
    Impl* impl_;
};

}  // namespace tn

#endif  // TN_WEBVIEW_HPP
