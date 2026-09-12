// 全局热键实现（Windows）。
#include "hotkey.hpp"

#include "../include/translator_native.h"

#include <windows.h>

#include <cstdio>

namespace tn {

namespace {

// 主键 id 的基址。RegisterHotKey 允许 0x0000–0xBFFF 的 id，
// 而 0xC000–0xFFFF 是留给 DLL 的（Atom 形式），所以从这里往上取很安全。
constexpr int kNativeIdBase = 0x4000;

// GetAsyncKeyState 的修饰键 VK。
//
// 注意用的是**通用** VK（VK_CONTROL 等）而不是左右区分的那组：
// 通用 VK 在 Windows 上会同时反映左右两个物理键的状态，
// 而用户配 "ctrl" 时并不关心他按的是左边还是右边的 Ctrl。
unsigned int modifierVKForBit(unsigned int bit) {
    switch (bit) {
        case TN_MOD_CONTROL: return VK_CONTROL;  // 0x11
        case TN_MOD_SHIFT:   return VK_SHIFT;    // 0x10
        case TN_MOD_ALT:     return VK_MENU;     // 0x12
        case TN_MOD_WIN:     return VK_LWIN;     // 0x5B
        default:             return 0;
    }
}

bool keyHeld(unsigned int vk) {
    return (GetAsyncKeyState(static_cast<int>(vk)) & 0x8000) != 0;
}

}  // namespace

int HotkeyManager::pollIntervalMs() { return 50; }  // V1 是 50ms

bool HotkeyManager::comboMatches(unsigned int mods, bool ctrlDown, bool shiftDown,
                                 bool altDown, bool winDown, bool keyDown) {
    if (!keyDown) return false;

    // 要求**精确匹配**：配置里没写的修饰键必须是抬起的。
    // 否则 "y" 会在用户按 Ctrl+Y 时也被触发——用户想发给游戏的操作被抢走。
    if ((mods & TN_MOD_CONTROL) != 0 && !ctrlDown) return false;
    if ((mods & TN_MOD_SHIFT) != 0 && !shiftDown) return false;
    if ((mods & TN_MOD_ALT) != 0 && !altDown) return false;
    if ((mods & TN_MOD_WIN) != 0 && !winDown) return false;

    if ((mods & TN_MOD_CONTROL) == 0 && ctrlDown) return false;
    if ((mods & TN_MOD_SHIFT) == 0 && shiftDown) return false;
    if ((mods & TN_MOD_ALT) == 0 && altDown) return false;
    if ((mods & TN_MOD_WIN) == 0 && winDown) return false;

    return true;
}

HotkeyManager::~HotkeyManager() { release(); }

void HotkeyManager::release() {
    for (const Entry& e : entries_) {
        if (e.nativeId != 0) {
            UnregisterHotKey(nullptr, e.nativeId);
        }
    }
    entries_.clear();
    fallbackIDs_.clear();
    nextNativeId_ = 1;
    lastPollTick_ = 0;
}

bool HotkeyManager::apply(const std::vector<HotkeySpec>& specs, std::string& err) {
    release();

    int skipped = 0;
    for (const HotkeySpec& s : specs) {
        if (!s.enabled) continue;
        if (s.vk == 0) {
            ++skipped;
            continue;
        }

        Entry e;
        e.id = s.id;
        e.vk = s.vk;
        e.mods = s.mods;

        // hWnd 传 nullptr：注册为**线程热键**，WM_HOTKEY 会投递到本线程的
        // 消息队列。这样就不依赖「窗口已经建好」——热键通常在显示层之前
        // 就该可用（Go 可能先设热键再建窗）。
        const int nativeId = kNativeIdBase + nextNativeId_;
        if (RegisterHotKey(nullptr, nativeId, s.mods, s.vk)) {
            e.nativeId = nativeId;
            ++nextNativeId_;
        } else {
            // 注册失败最常见的两个原因：
            //   ERROR_HOTKEY_ALREADY_REGISTERED —— 被别的程序占了
            //   权限不足
            // 两种情况都降级为轮询，而不是让这个热键静默失效。
            e.nativeId = 0;
            e.wasDown = true;  // 避免降级瞬间就把「当前正按着」误判成一次触发
            fallbackIDs_.push_back(s.id);

            const DWORD code = GetLastError();
            std::printf("[hotkey] %s 注册失败 err=%lu，降级为轮询\n",
                        s.id.c_str(), code);
            std::fflush(stdout);
        }
        entries_.push_back(e);
    }

    if (skipped > 0) {
        err = "有 " + std::to_string(skipped) + " 个热键没有解析出主键，已跳过";
    }
    return true;
}

std::string HotkeyManager::onMessage(unsigned int msg, unsigned long long wparam) {
    if (msg != WM_HOTKEY) return std::string();

    const int nativeId = static_cast<int>(wparam);
    for (const Entry& e : entries_) {
        if (e.nativeId == nativeId) return e.id;
    }
    return std::string();
}

std::vector<std::string> HotkeyManager::poll() {
    std::vector<std::string> fired;

    // 全部走 RegisterHotKey 时不必轮询——省掉每秒 20 次的 GetAsyncKeyState
    bool hasFallback = false;
    for (const Entry& e : entries_) {
        if (e.nativeId == 0) { hasFallback = true; break; }
    }
    if (!hasFallback) return fired;

    const unsigned long long now = GetTickCount64();
    if (now - lastPollTick_ < static_cast<unsigned long long>(pollIntervalMs())) {
        return fired;
    }
    lastPollTick_ = now;

    const bool ctrl = keyHeld(VK_CONTROL);
    const bool shift = keyHeld(VK_SHIFT);
    const bool alt = keyHeld(VK_MENU);
    const bool win = keyHeld(VK_LWIN);

    for (Entry& e : entries_) {
        if (e.nativeId != 0) continue;  // 这条走的是系统热键

        const bool down = comboMatches(e.mods, ctrl, shift, alt, win, keyHeld(e.vk));
        // 边沿检测：只在「从没按到按下」的那一刻触发。
        // 按住不放不会连续触发——否则长按一次会呼出好几十次。
        if (down && !e.wasDown) fired.push_back(e.id);
        e.wasDown = down;
    }
    return fired;
}

}  // namespace tn
