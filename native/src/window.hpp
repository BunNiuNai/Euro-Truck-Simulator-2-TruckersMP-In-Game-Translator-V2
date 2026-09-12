// 无边框悬浮窗 —— 透明置顶、毛玻璃、鼠标穿透、原生拖拽与缩放、单实例互斥体。
//
// V1 来源：overlay.py 的窗口操作部分 + acrylic_helper.py（261 行）+ win32_constants.py
//
// 与 V1 的关键差异（这是优势，不是功能变化）：
//   V1 用 tkinter 建窗口，再靠 `GetParent(root.winfo_id())` 反查真正的 HWND
//   （见 acrylic_helper._get_real_hwnd 的注释）。V2 的 native 层**直接持有 HWND**，
//   那个 hack 连同它的失败模式一起消失了。
//
// 不做的事（非目标 N4 / ADR-014）：不注入游戏进程、不 hook 渲染、不读写游戏内存。
#ifndef TN_WINDOW_HPP
#define TN_WINDOW_HPP

#include <functional>
#include <string>

namespace tn {

// 窗口规格。x/y 为 -1 表示自动居中。
struct WindowSpec {
    int x = -1;
    int y = -1;
    int width = 620;
    int height = 360;
    bool topmost = true;
    bool clickThrough = false;
    double opacity = 0.8;
    std::string blur = "auto";  // "auto" | "mica" | "acrylic" | "none"
    std::wstring title = L"ETS2 Translator";

    // dark 控制窗口的**沉浸式深色模式**，它决定 Mica/Acrylic 材质的深浅。
    // 必须与前端主题一致：浅色主题配深色材质会出现明显错配。
    bool dark = true;

    // framed=true 创建的是**普通带边框窗口**：有系统标题栏、最小化/最大化/
    // 关闭按钮、出现在任务栏。设置窗口用的就是这个形态（V1 用的是
    // tk.Toplevel，同样是一扇普通窗口）。
    //
    // framed=false（默认）是悬浮窗那种无边框置顶浮层。
    //
    // 两者共用同一套 create/destroy/尺寸回调，只是窗口样式与窗口过程里
    // 的几处分支不同——分成两个类会把这些逐字复制一遍。
    bool framed = false;
};

// 命中区域。无边框窗口靠 WM_NCHITTEST 返回这些值来获得原生拖拽与缩放。
enum class HitZone {
    None,
    Caption,
    Left,
    Right,
    Top,
    Bottom,
    TopLeft,
    TopRight,
    BottomLeft,
    BottomRight
};

// 纯函数：根据窗口尺寸、鼠标位置与边框宽度判定命中区域。
// 抽成纯函数是为了可单测——它是拖拽/缩放正确性的全部逻辑。
HitZone hitTestZone(int width, int height, int x, int y, int border);

// 把命中区域转成 WM_NCHITTEST 的返回值。
int hitZoneToNcCode(HitZone zone);

// 窗口几何（屏幕坐标）。
struct WindowGeometry {
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;
};

// 纯函数：把「Go 下发的几何」解析成「真正要用的几何」。
//
// 规则逐条对齐 V1 `overlay.py:383-399` 的 `_restore_or_center`：
//   · x 或 y 为负 = 这次没有记住的位置（Go 送 -1）→ 两轴都取屏幕正中
//   · 记住了位置 → 钳进可见区域：坐标限制在 [0, 屏幕尺寸-100]，
//     尺寸不超过屏幕（V1 的 `min(win_w, sw)`）
//
// 抽成纯函数是为了可单测：屏幕尺寸作为参数传进来，边界（比屏幕还大的窗口、
// 负坐标、拿不到屏幕尺寸）就能直接断言，不必真去改显示器分辨率。
WindowGeometry resolveWindowGeometry(int x, int y, int width, int height,
                                     int screenWidth, int screenHeight);

// 纯函数：毛玻璃第 attempt 次重试前要等多久（毫秒），0 表示不再重试。
// 时刻表来自 V1 `overlay.py:98-100` 的 200ms / 800ms / 2000ms。
int blurRetryDelayMs(int attempt);

// 单实例互斥体（V1 main.py 的 _ensure_single_instance 等价实现）。
class SingleInstance {
public:
    ~SingleInstance();

    // 返回 true 表示「本次是第一个实例」。已存在同名互斥体时返回 false。
    bool acquire(const std::wstring& name);
    bool alreadyRunning() const { return alreadyRunning_; }
    void release();

private:
    void* handle_ = nullptr;  // HANDLE
    bool alreadyRunning_ = false;
};

// 无边框悬浮窗。
class OverlayWindow {
public:
    OverlayWindow() = default;
    ~OverlayWindow();
    OverlayWindow(const OverlayWindow&) = delete;
    OverlayWindow& operator=(const OverlayWindow&) = delete;

    bool create(const WindowSpec& spec);
    bool destroy();

    void show();
    void hide();
    bool isVisible() const;

    // 把窗口带到前台并获得键盘焦点。热键「呼出输入框」用。
    //
    // ⚠️ 不能只调 SetForegroundWindow：系统只允许「前台窗口属于本进程」或
    //    「持有前台权限」的进程抢前台，而后台热键触发时两条都不满足——
    //    表现就是热键按下去毫无反应。这里用 AttachThreadInput 把本线程挂到
    //    前台线程上借它的权限，抢完再解挂（V1 overlay.py:809-849 同一手法）。
    //
    // 窗口隐藏时先显示出来（V1 的 `deiconify`）。
    bool focus();

    // 鼠标穿透：切换 WS_EX_TRANSPARENT
    bool setClickThrough(bool enable);
    bool isClickThrough() const;

    // 毛玻璃。mode: "auto"（按系统版本择优）| "mica" | "acrylic" | "none"
    // 返回 true 表示成功应用了某种效果（"none" 恒为 true）。
    bool applyBlur(const std::string& mode);

    // 切换窗口的深色/浅色模式（影响材质深浅）。已应用的材质会被重新应用。
    bool setDarkMode(bool dark);

    // 透明度。仅在毛玻璃不可用时才真正影响观感（与 V1 一致：
    // 走 -alpha 的窗口会有拖影，所以只在降级路径使用）。
    bool setOpacity(double opacity);

    // 处理当前线程的待处理消息；返回 false 表示收到了 WM_QUIT/窗口已销毁。
    bool pump();

    // 由**前端**发起的拖动/缩放。
    //
    // 为什么不是靠 WM_NCHITTEST 自己搞定：WebView2 的子窗口铺满客户区后，
    // 鼠标事件全被它接走，宿主窗口在四条边上收不到 WM_NCHITTEST——实测把
    // 光标停在窗口四边，指针始终是 IDC_ARROW，窗口既拖不动也缩不了。
    // 所以改由前端在最外圈监听 mousedown，经 IPC 转到这里。
    //
    // 为什么**不**用 WM_NCLBUTTONDOWN 交给系统模态循环（那是最常见的做法）：
    // 那条循环会阻塞 native 的主线程，而管道读取正跑在这个线程上。结果是
    // 拖动期间读不到管道——而心跳每 1s 一次 Call、超时只有 5s，一次稍长的
    // 拖动就会被判成断线并重连，窗口跟着重建。代价完全不可接受。
    //
    // 所以这里自己跟踪：beginMoveResize 只记下起点立刻返回，真正的位移由
    // 主循环每轮调 tickMoveResize 推进。主循环始终在跑，管道与心跳都正常。
    // 这也与 V1 一致——V1 的 tkinter 同样是自己在 <Motion> 里算几何的。
    //
    // 代价：没有 Aero Snap 贴边吸附（系统模态循环才带）。悬浮窗不需要它。
    bool beginMoveResize(HitZone zone);

    // 推进一次拖动/缩放。主循环每轮调用一次。
    // 返回 true 表示**本次**推进后仍在拖动中。
    bool tickMoveResize();

    // 当前是否正在拖动/缩放。
    bool isMoveResizing() const { return dragging_; }

    // 取走一次「用户改过窗口几何」的通知（取走即清空），返回 true 时
    // x/y/width/height 是新几何。
    //
    // ⚠️ 只记**用户拖动/缩放**造成的改变，程序自己调的 setBounds 不算——
    //    否则每次下发期望状态都会触发一轮「上报 → 保存」，自己把自己绕进去。
    //
    // 存在的理由：几何是**用户**改的，Go 的期望状态不知道，重连重放就会
    // 把窗口弹回旧位置（V1 靠 `_schedule_save_position` 解决同一问题）。
    bool takeUserGeometryChange(int& x, int& y, int& width, int& height);

    // 标记「用户改过窗口几何」。供窗口过程在 WM_SIZE/WM_MOVE 里调用——
    // 带边框窗口的位置尺寸是**系统标题栏**改的，主循环无从得知。
    void markUserGeometryChanged() { geometryDirty_ = true; }

    // 取走一次「用户把悬浮窗关掉（WM_CLOSE）」的通知（取走即清空）。
    //
    // 悬浮窗的关闭＝**隐藏**，不是销毁（V1 `main.py:85,210-212`）。隐藏是
    // 用户做出的事实，Go 的期望状态并不知道——不报的话 Go 那边永远认为窗口
    // 还是可见的，托盘「显示/隐藏」会取反成隐藏（正好反了）、而新消息到达时
    // 也不会再把窗口叫回来（`cmd/translator/main.go:977-983` 就是靠实际可见性
    // 判断要不要调 SetVisible(true) 的）。
    bool takeUserHide();

    // 标记「用户把悬浮窗关掉了」。供窗口过程在 WM_CLOSE 里调用，
    // 理由同 markUserGeometryChanged（窗口过程在匿名命名空间里）。
    void markHiddenByUser() { hiddenByUser_ = true; }

    // 供窗口过程在 WM_TIMER 里调用：推进一次毛玻璃重试（V1 的 3 次重试）。
    void onBlurTimer();

    // 供窗口过程在 WM_SHOWWINDOW 里调用。隐藏会让 DWM 摘掉材质，所以重新显示
    // 之后要把材质重应用一次（V1 `overlay.py:101-102` 用 `<Map>` 事件做同一件事）。
    void onWindowShown();
    void onWindowHidden();

    // 正在进行的拖动区域（诊断用；未拖动时为 HitZone::None）。
    HitZone activeZone() const { return dragZone_; }

    // 窗口尺寸变化时回调（参数为新的客户区宽高）。
    // WM_SIZE 在原生缩放循环里每次变化都会到达，所以 WebView 能跟着
    // 逐帧变化，而不是等拖完才跳一下。
    void setResizeHandler(std::function<void(int, int)> handler);

    // 供窗口过程在收到 WM_SIZE 时调用。公开只是为了让匿名命名空间里的
    // overlayProc 够得着，不是给业务代码用的。
    void notifySizeChanged(int width, int height);

    // 位置与尺寸（用于「窗口位置记忆」）
    bool getBounds(int& x, int& y, int& width, int& height) const;
    bool setBounds(int x, int y, int width, int height);

    void* hwnd() const { return hwnd_; }
    bool valid() const { return hwnd_ != nullptr; }

    // 是否**曾经**创建过窗口（用于区分「还没建」与「建了又被销毁」）
    bool wasCreated() const { return created_; }

    // 是否为带边框窗口（决定窗口过程里的分支）
    bool framed() const { return framed_; }

    // 供窗口过程在 WM_DESTROY 时调用。
    //
    // ⚠️ 必须有这一步：用户点标题栏的关闭按钮时是 DefWindowProc 直接
    //    DestroyWindow，不会经过 OverlayWindow::destroy()，于是 hwnd_ 会
    //    一直指向已销毁的窗口——valid() 永远为真，上层根本察觉不到窗口没了
    //    （悬浮窗那条「窗口已销毁」的日志因此从来没打印过）。
    void onDestroyed();

    // 最后应用的毛玻璃模式（诊断用）
    const std::string& appliedBlur() const { return appliedBlur_; }

    // **期望**的毛玻璃模式（诊断用）。与 appliedBlur() 分开：失败时实际模式会
    // 退化成 "none"，而重试必须拿期望模式重来一次。
    const std::string& blurMode() const { return blurMode_; }

    // 当前是否为深色模式（决定材质深浅）
    bool isDark() const { return dark_; }

    // 系统 build 号（RtlGetVersion，不受应用清单影响——与 V1 同一做法）
    static int osBuild();

    // 是否有可用的桌面会话（无会话时建窗口会失败，测试应据此跳过）
    static bool hasDesktopSession();

private:
    // 应用一次毛玻璃，不碰重试状态（"none" 也走这里）。
    bool applyBlurNow(const std::string& mode);

    // 安排（或取消）下一次重试。失败次数用尽时会留下降级日志。
    void armBlurRetry();
    void cancelBlurRetry();

    void* hwnd_ = nullptr;
    std::string appliedBlur_ = "none";
    std::string blurMode_ = "auto";   // 期望模式，重试与「显示后重应用」都用它
    int blurRetry_ = 0;               // 已经是第几次重试（初值 0 = 还没重试过）
    bool needsBlurReapply_ = false;   // 窗口被隐藏过，再显示时要重应用材质
    bool classRegistered_ = false;
    double opacity_ = 0.8;
    bool dark_ = true;
    bool created_ = false;
    bool framed_ = false;
    bool hiddenByUser_ = false;   // 用户点了关闭（＝隐藏），等着被主循环取走
    std::function<void(int, int)> resizeHandler_;

    // ── 拖动/缩放状态 ──
    HitZone dragZone_ = HitZone::None;
    bool dragging_ = false;
    int dragOriginX_ = 0;   // 按下时的屏幕坐标（移动用绝对位移）
    int dragOriginY_ = 0;
    int dragLastX_ = 0;     // 上一次推进时的屏幕坐标（缩放用增量，与 V1 一致）
    int dragLastY_ = 0;
    int dragStartX_ = 0;    // 按下时的窗口几何
    int dragStartY_ = 0;
    int dragStartW_ = 0;
    int dragStartH_ = 0;
    bool geometryDirty_ = false;   // 用户改过几何，等着被取走
};

}  // namespace tn

#endif  // TN_WINDOW_HPP
