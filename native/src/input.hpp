// 键盘输入模拟与剪贴板 —— 发送方向的 Windows 能力层。
//
// V1 来源：input_sender.py（277 行）
//
// ⚠️ 架构边界（ADR-014）：本模块只做**合成输入事件**（SendInput）与剪贴板读写，
//    不注入进程、不读写游戏内存、不 hook 任何东西。合成的按键与用户真的敲键盘
//    在系统看来完全一样——这正是 V1 的做法，也是它没有违反 TruckersMP 规则的原因。
//
// 关于热键解析：**native 不解析热键字符串**，只接收 Go 侧解析好的 VK 码与
// MOD_* 位掩码。V1 有四处各自为政的热键解析器（缺陷 D7），V2 统一为
// 「单一 JSON 表 + 单一 Go 解析器」，native 不再重复一套。
#ifndef TN_INPUT_HPP
#define TN_INPUT_HPP

// TN_MOD_* 位掩码定义在这里——SendRequest.hotkeyMods 的取值就是它们，
// 属于接口约定，必须在使用者可见的地方声明。
#include "../include/translator_native.h"

#include <string>

namespace tn {

// 一次发送的结果。对应 V1 `send_chat_message` 的返回值约定：
// 成功时 reason 为空，失败时 reason 是可展示的原因。
struct SendResult {
    bool ok = false;
    std::string reason;

    static SendResult success() { return SendResult{true, {}}; }
    static SendResult failure(const std::string& r) { return SendResult{false, r}; }
};

// 发送请求。VK/Mods 由 Go 侧解析（见文件头说明）。
struct SendRequest {
    std::wstring text;
    unsigned int hotkeyVK = 0;
    unsigned int hotkeyMods = 0;  // MOD_ALT/CONTROL/SHIFT/WIN 位掩码
    int delayMs = 500;            // V1 默认 500ms
    int holdMs = 50;              // V1 `_press_key` 的 hold_sec=0.05
};

// ── 纯函数（可单测，不碰 Win32）──────────────────────────────

// 把 MOD_* 位掩码展开成需要按下的修饰键 VK 列表，返回写入个数。
//
// 抽成纯函数是因为这是整个输入序列里**唯一有判断逻辑**的部分：
// SendInput 本身只是机械地发事件，只有修饰键的展开顺序值得单测。
// out 至少要有 4 个元素（最多 4 个修饰键）。
int modifierVKs(unsigned int mods, unsigned int* out, int maxOut);

// ── 输入模拟 ────────────────────────────────────────────────

// 发送单个按键的按下/抬起事件。返回 false 表示 SendInput 被拒绝。
bool sendKeyEvent(unsigned int vk, bool up);

// 按一次键（按下 → hold → 抬起）。
bool pressKey(unsigned int vk, int holdMs = 50);

// 按一次组合键：修饰键顺序按下 → 主键按下抬起 → 修饰键**逆序**释放。
// 逆序释放是必须的：先松开 Ctrl 再松开 V 会让某些程序收到裸 V。
bool pressCombo(unsigned int mods, unsigned int vk, int holdMs = 50);

// ── 发送器 ──────────────────────────────────────────────────

class InputSender {
public:
    InputSender() = default;
    ~InputSender();
    InputSender(const InputSender&) = delete;
    InputSender& operator=(const InputSender&) = delete;

    // 执行完整发送序列，六步与 V1 input_sender.py:277-331 一致：
    //   1. 保存剪贴板  2. 按热键打开游戏聊天  3. 写入剪贴板
    //   4. Ctrl+V 粘贴  5. 回车发送  6. 恢复剪贴板
    SendResult sendChatMessage(const SendRequest& req);

    // 只把文本放进剪贴板（「复制译文」热键用）。
    bool setClipboardText(const std::wstring& text);

    // 读剪贴板文本；失败返回空串。
    std::wstring getClipboardText();

    // 剪贴板拥有者窗口是否就绪。未就绪时 setClipboardText 会失败。
    bool ready() const { return clipWindow_ != nullptr; }

    void destroy();

private:
    bool ensureClipboardWindow();

    void* clipWindow_ = nullptr;  // HWND：message-only 窗口
};

}  // namespace tn

#endif  // TN_INPUT_HPP
