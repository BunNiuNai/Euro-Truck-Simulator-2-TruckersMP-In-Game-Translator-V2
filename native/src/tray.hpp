// 系统托盘图标 —— V1 `tray_icon.py`（324 行）的等价实现。
//
// 职责边界（矩阵 W7）：
//   native 只负责**显示**托盘与菜单、把「用户点了哪一项」记下来；
//   **点完之后做什么由 Go 决定**——「当前是不是穿透」「该切到哪个模式」
//   属于期望状态，只有 Go 持有（ADR-012）。所以这里不做任何业务判断。
//
// ⚠️ 菜单消息循环必须跑在**独立线程**上。
//
//   `TrackPopupMenu` 是**模态**的：它一直阻塞到自己被选中或取消为止。
//   若放在 native 主线程上，用户把菜单开着不放，主线程就再也读不到管道——
//   而心跳是每秒一次、超时 5 秒，于是连接会被判死并重连、窗口跟着重建。
//   这正是悬浮窗拖动不能用系统模态移动循环的同一个原因。
//
//   独立线程带来一个新的约束：**命令不能直接从托盘线程发给 Go**
//   （管道写入是主线程的职责，跨线程写会交错）。所以托盘线程只把命令塞进
//   队列，主循环用 takeCommands() 取走再上报。
//
// V1 也是这么做的（tray_icon.py 用 threading.Thread + 自己的 GetMessage 循环），
// 这里与它一致。
#ifndef TN_TRAY_HPP
#define TN_TRAY_HPP

#include <functional>
#include <mutex>
#include <string>
#include <vector>

namespace tn {

// 一个菜单项。separator 为 true 时其余字段被忽略。
struct TrayMenuItem {
    std::string id;         // 命令 id，原样上报给 Go
    std::string label;      // 显示文本
    bool separator = false;
    bool checked = false;   // 是否打勾（由 Go 决定，对应 V1 的 checked lambda）
    bool isDefault = false; // 默认项（加粗），对应 V1 的 MF_DEFAULT
};

struct TraySpec {
    bool enabled = false;
    std::string tip;                 // 悬停提示
    std::vector<TrayMenuItem> menu;
};

// 托盘图标。整个对象**不是**线程安全的对外接口，但内部对菜单与命令队列加了锁：
// 主线程调用 apply/destroy/takeCommands，托盘线程读菜单、写命令队列。
class TrayIcon {
public:
    TrayIcon() = default;
    ~TrayIcon();
    TrayIcon(const TrayIcon&) = delete;
    TrayIcon& operator=(const TrayIcon&) = delete;

    // 应用期望状态：第一次调用会创建托盘，之后每次调用都是更新
    // （菜单内容与勾选状态都可能变，比如用户改了穿透）。
    bool apply(const TraySpec& spec, std::string& err);

    // 移除托盘图标并结束托盘线程。可重复调用。
    //
    // ⚠️ 契约：**无条件返回**，绝不会卡在托盘线程上。
    //    托盘窗口是托盘线程异步建出来的，调用 destroy() 时它完全可能还没建好
    //    （甚至已经建失败），此时没有任何窗口句柄可发消息——停止请求靠
    //    Impl 里的 stopRequested 标志传递，不依赖任何窗口存在。
    //    调用方通常是退出路径，在这里挂住等于进程关不掉。
    void destroy();

    bool active() const;

    // 主线程取走托盘线程累积的命令 id（取走即清空）。
    std::vector<std::string> takeCommands();

    // 是否有可用的桌面会话（无会话时托盘建不出来，测试应据此跳过）
    static bool hasDesktopSession();

    // ⚠️ 必须是 public：托盘窗口过程在类外，要拿 Impl 指针才能用 GWLP_USERDATA
    //    存回去。内容仍然只在 .cpp 里定义，外部看不到实现细节。
    //    （与 webview.hpp 里 WebViewHost::Impl 同样的理由。）
    struct Impl;

    // 最近一次失败原因（空表示没有失败）。托盘是异步创建的，所以失败不能
    // 靠 apply() 的返回值报告，要在这里取。
    std::string lastError() const;

private:
    Impl* impl_ = nullptr;
};

}  // namespace tn

#endif  // TN_TRAY_HPP
