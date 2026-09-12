// 消息分发 —— native 的「业务层」，与管道传输解耦。
//
// 为什么单独成一个单元：放在 main.cpp 里就无法自测，而 ADR-005 要求每个
// 能力「小而自证」。抽出来之后测试可以直接喂 JSON 帧、断言回复内容与
// **真实窗口状态**，不需要真的起一个管道连接（沙箱里管道客户端是被禁的）。
//
// 职责边界：
//   - 本单元只做「消息 → 动作 → 回复 JSON」，不碰管道、不碰网络、不退出进程
//   - 实际状态一律从 OverlayWindow 现读，绝不缓存「我以为的状态」（ADR-012）
#ifndef TN_DISPATCH_HPP
#define TN_DISPATCH_HPP

#include "hotkey.hpp"
#include "input.hpp"
#include "tray.hpp"
#include "webview.hpp"
#include "window.hpp"

#include <string>
#include <vector>

namespace tn {

// 会话级状态。只放「跨命令存活」的东西。
struct Session {
    OverlayWindow window;
    InputSender input;
    HotkeyManager hotkeys;
    WebViewHost webview;

    // 设置窗口。它**声明在 webview 之后**是有意的：成员按声明逆序析构，
    // 这样设置窗口会先于 webview 销毁，它的尺寸回调不会打到已析构的 WebView 上。
    OverlayWindow settings;

    // 设置窗口的初始尺寸与位置。来自 Go 的期望状态
    // （config.settings_win_x/y/w/h），-1 表示自动居中。
    int settingsX = -1;
    int settingsY = -1;
    int settingsWidth = 540;
    int settingsHeight = 700;

    // 系统托盘。声明在最后 → 析构最早。它持有自己的线程，早一点收掉
    // 可以保证其它成员被销毁时它已经不在跑了。
    TrayIcon tray;
};

// 本进程当前真正具备的能力。Go 侧据此与期望状态比对（ADR-012）。
// 只列已实现的——夸大能力会让 Go 以为某个功能可用。
std::vector<std::string> capabilities();

// 处理一条消息，返回要回复的 JSON 文本。
// stop 置 true 表示收到 shutdown，调用方应退出主循环。
// 返回空字符串表示无需回复。
std::string handleMessage(const std::string& raw, Session& session, bool& stop);

// 悬浮窗「实际状态」的 event.stateChanged 帧（主循环在窗口可见性被**用户**
// 改动后推送，见 main.cpp 里的用法）。
//
// 为什么复用 event.stateChanged 而不新造一个事件类型：Go 侧收到它就会拿
// payload.display 更新**实际状态**（internal/native/native.go:1079-1091），
// 而托盘「显示/隐藏」的取反（cmd/translator/main.go:634-637）与新消息到达时
// 「要不要把窗口叫回来」（同文件 977-983）读的正是这个实际可见性。
// 新造一个类型反而没人认——消息是发出去了，Go 那边毫无反应。
std::string displayStateEvent(const Session& session);

}  // namespace tn

#endif  // TN_DISPATCH_HPP
