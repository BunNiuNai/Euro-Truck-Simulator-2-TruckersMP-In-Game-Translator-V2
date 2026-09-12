// 输入模拟与剪贴板实现（Windows）。
#include "input.hpp"

#include "../include/translator_native.h"

#include <windows.h>

#include <cstdio>
#include <cstring>
#include <string>

namespace tn {

namespace {

// 粘贴用的 V。Windows SDK 只为功能键定义了 VK_* 宏，
// 字母与数字键没有（VK_A..VK_Z 不存在），直接用字符码：'V' == 0x56。
constexpr unsigned int kVKV = 'V';

// SendInput 的 INPUT 结构在 user32 里；这里只做薄封装。
bool rawKey(unsigned int vk, bool up) {
    INPUT in{};
    in.type = INPUT_KEYBOARD;
    in.ki.wVk = static_cast<WORD>(vk);
    in.ki.wScan = 0;
    in.ki.dwFlags = up ? KEYEVENTF_KEYUP : 0;
    in.ki.time = 0;
    in.ki.dwExtraInfo = 0;

    const UINT sent = SendInput(1, &in, sizeof(INPUT));
    if (sent != 1) {
        // 返回 0 通常意味着被 UIPI 拦下——目标窗口比本进程权限高
        // （例如游戏以管理员身份运行）。V1 也只是记日志，这里保持一致，
        // 由调用方通过 pressKey 的返回值决定怎么报给用户。
        std::printf("[input] SendInput 被拒绝 VK=%u err=%lu\n", vk, GetLastError());
        std::fflush(stdout);
        return false;
    }
    return true;
}

void sleepMs(int ms) {
    if (ms <= 0) return;

    // 等待期间**照常泵窗口消息**。
    //
    // 为什么不是直接 Sleep：一次发送序列有三段 500ms 等待，共约 1.5 秒。
    // 直接 Sleep 会让悬浮窗在这 1.5 秒里完全不响应（拖不动、右键菜单不出来），
    // 用户会以为程序卡死了。
    //
    // V1 用的是「把发送丢到后台线程」——那样要处理跨线程的剪贴板窗口归属
    // （Windows 的剪贴板拥有者窗口是线程亲和的），复杂且容易出错。
    // 这里改成在等待中泵消息：单线程、无竞态，窗口全程可用。
    const DWORD deadline = GetTickCount() + static_cast<DWORD>(ms);
    MSG msg;
    for (;;) {
        while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
            TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }
        const DWORD now = GetTickCount();
        if (now >= deadline) break;
        const DWORD left = deadline - now;
        Sleep(left > 10 ? 10 : left);
    }
}

}  // namespace

// ── 纯函数 ──────────────────────────────────────────────────

int modifierVKs(unsigned int mods, unsigned int* out, int maxOut) {
    if (out == nullptr || maxOut <= 0) return 0;

    int n = 0;
    // 顺序固定为 Ctrl → Shift → Alt → Win：Win32 自己也是这个展开顺序，
    // 换顺序在某些程序里会触发意外组合（例如 Alt+Shift 切输入法）。
    if (mods & TN_MOD_CONTROL && n < maxOut) out[n++] = VK_CONTROL;
    if (mods & TN_MOD_SHIFT && n < maxOut) out[n++] = VK_SHIFT;
    if (mods & TN_MOD_ALT && n < maxOut) out[n++] = VK_MENU;
    if (mods & TN_MOD_WIN && n < maxOut) out[n++] = VK_LWIN;
    return n;
}

// ── 输入模拟 ────────────────────────────────────────────────

bool sendKeyEvent(unsigned int vk, bool up) { return rawKey(vk, up); }

bool pressKey(unsigned int vk, int holdMs) {
    if (vk == 0) return false;
    const bool down = rawKey(vk, false);
    sleepMs(holdMs);
    const bool up = rawKey(vk, true);
    return down && up;
}

bool pressCombo(unsigned int mods, unsigned int vk, int holdMs) {
    if (vk == 0) return false;

    unsigned int keys[4] = {0, 0, 0, 0};
    const int n = modifierVKs(mods, keys, 4);

    bool ok = true;
    for (int i = 0; i < n; ++i) ok = rawKey(keys[i], false) && ok;
    ok = rawKey(vk, false) && ok;
    sleepMs(holdMs);
    ok = rawKey(vk, true) && ok;
    // 逆序释放：先松开 Ctrl 再松开 V 会让某些程序收到一个裸 V
    for (int i = n - 1; i >= 0; --i) ok = rawKey(keys[i], true) && ok;
    return ok;
}

// ── InputSender ─────────────────────────────────────────────

InputSender::~InputSender() { destroy(); }

void InputSender::destroy() {
    if (clipWindow_) {
        DestroyWindow(static_cast<HWND>(clipWindow_));
        clipWindow_ = nullptr;
    }
}

bool InputSender::ensureClipboardWindow() {
    if (clipWindow_) return true;

    // 剪贴板必须有拥有者窗口：Windows 在拥有者销毁时会清空剪贴板，
    // 所以这个窗口要活到进程结束（V1 input_sender.py:162-164 的同一考虑）。
    static const wchar_t* kClass = L"TnClipboardOwner";
    HINSTANCE inst = GetModuleHandleW(nullptr);

    WNDCLASSEXW wc{};
    wc.cbSize = sizeof(wc);
    wc.lpfnWndProc = DefWindowProcW;
    wc.hInstance = inst;
    wc.lpszClassName = kClass;
    if (!RegisterClassExW(&wc)) {
        if (GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
            std::printf("[input] 注册剪贴板窗口类失败 err=%lu\n", GetLastError());
            return false;
        }
    }

    HWND h = CreateWindowExW(0, kClass, L"", 0, 0, 0, 0, 0,
                             HWND_MESSAGE, nullptr, inst, nullptr);
    if (!h) {
        std::printf("[input] 创建剪贴板窗口失败 err=%lu\n", GetLastError());
        return false;
    }
    clipWindow_ = h;
    return true;
}

bool InputSender::setClipboardText(const std::wstring& text) {
    if (!ensureClipboardWindow()) return false;
    HWND hwnd = static_cast<HWND>(clipWindow_);

    // OpenClipboard 会因别的进程正在使用而失败。V1 没有重试，直接失败；
    // 但剪贴板竞争很常见（输入法、剪贴板管理器都在抢），所以这里做有限重试。
    // 按 10ms/20ms/40ms/80ms 退避，总计约 150ms——足够躲过瞬时占用，
    // 又不会让发送序列明显变慢。
    const DWORD backoff[] = {10, 20, 40, 80};
    bool opened = false;
    for (int attempt = 0; attempt < 5 && !opened; ++attempt) {
        opened = OpenClipboard(hwnd) != FALSE;
        if (!opened && attempt < 4) sleepMs(static_cast<int>(backoff[attempt]));
    }
    if (!opened) {
        std::printf("[input] OpenClipboard 失败 err=%lu\n", GetLastError());
        return false;
    }

    bool ok = false;
    if (EmptyClipboard()) {
        const size_t bytes = (text.size() + 1) * sizeof(wchar_t);
        HGLOBAL mem = GlobalAlloc(GMEM_MOVEABLE, bytes);
        if (mem) {
            void* dst = GlobalLock(mem);
            if (dst) {
                std::memcpy(dst, text.c_str(), bytes);
                GlobalUnlock(mem);
                // SetClipboardData 成功后，内存所有权归系统，不能再 GlobalFree
                if (SetClipboardData(CF_UNICODETEXT, mem)) {
                    ok = true;
                    mem = nullptr;
                }
            }
            if (mem) GlobalFree(mem);
        }
    }
    CloseClipboard();
    return ok;
}

std::wstring InputSender::getClipboardText() {
    if (!ensureClipboardWindow()) return std::wstring();
    HWND hwnd = static_cast<HWND>(clipWindow_);

    if (!OpenClipboard(hwnd)) return std::wstring();

    std::wstring out;
    HANDLE h = GetClipboardData(CF_UNICODETEXT);
    if (h) {
        const wchar_t* src = static_cast<const wchar_t*>(GlobalLock(h));
        if (src) {
            out.assign(src);
            GlobalUnlock(h);
        }
    }
    CloseClipboard();
    return out;
}

SendResult InputSender::sendChatMessage(const SendRequest& req) {
    if (req.hotkeyVK == 0) {
        return SendResult::failure("无效的按键（热键未解析出主键）");
    }
    if (req.text.empty()) {
        return SendResult::failure("消息为空");
    }

    // 1) 先保存剪贴板——失败不算致命，只是最后不恢复而已
    std::wstring oldClip = getClipboardText();

    const int delay = req.delayMs > 0 ? req.delayMs : 500;

    // 2) 按热键打开游戏聊天（V1 顺序：热键在前，写剪贴板在后）
    if (!pressCombo(req.hotkeyMods, req.hotkeyVK, req.holdMs)) {
        // SendInput 被拒的最常见原因是 UIPI：目标进程权限更高。
        return SendResult::failure(
            "按键模拟被系统拒绝——若游戏以管理员身份运行，请以同样权限运行本程序");
    }
    sleepMs(delay);

    // 3) 写入剪贴板
    if (!setClipboardText(req.text)) {
        return SendResult::failure("写入剪贴板失败（可能被其他程序占用）");
    }

    // 4) Ctrl+V 粘贴
    //
    // ⚠️ 修饰键必须是 TN_MOD_CONTROL，**不能传 0**。
    //
    // 这里原来写的是 `pressCombo(0, kVKV, ...)`——注释明明白白写着「Ctrl+V 粘贴」，
    // 实际却按了一个**裸 V**。后果是游戏聊天框里出现的不是译文，而是一个字母 v：
    // 用户看到的现象是「不管我发什么，进去的都是 v」。
    // 广告发送与手动发送共用这条序列，所以两边症状完全一样。
    //
    // 这类错误特别难查：按键序列"执行成功"了（每个 SendInput 都返回成功）、
    // 日志里也没有任何异常，只有真的盯着游戏窗口看才会发现进错了内容。
    pressCombo(TN_MOD_CONTROL, kVKV, req.holdMs);
    sleepMs(delay);

    // 5) 回车发送
    pressKey(VK_RETURN, req.holdMs);
    sleepMs(delay);

    // 6) 恢复剪贴板（V1 放在 finally 里，这里成功路径也恢复）
    if (!oldClip.empty()) {
        setClipboardText(oldClip);
    }
    return SendResult::success();
}

}  // namespace tn
