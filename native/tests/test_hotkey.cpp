// hotkey 模块自测。
//
// 重点在**纯函数**（组合键匹配）与**注册/降级行为**：
//   · comboMatches 是轮询降级路径的全部判断逻辑，值得逐条钉住
//   · RegisterHotKey 能否成功取决于运行时环境（可能被别的程序占用），
//     所以断言的是「要么注册成功、要么降级」这个不变量，而不是具体走哪条
#include "../src/hotkey.hpp"
#include "../src/window.hpp"  // hasDesktopSession：无桌面会话时跳过注册断言
#include "../include/translator_native.h"

#include <windows.h>

#include <cstdio>
#include <string>
#include <vector>

namespace {

int g_failed = 0;
int g_passed = 0;
int g_skipped = 0;

void check(bool cond, const std::string& what) {
    if (cond) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s\n", what.c_str());
}

void skip(const std::string& what) {
    ++g_skipped;
    std::printf("  SKIP: %s\n", what.c_str());
}

void checkEqInt(int got, int want, const std::string& what) {
    if (got == want) { ++g_passed; return; }
    ++g_failed;
    std::printf("  FAIL: %s  got=%d want=%d\n", what.c_str(), got, want);
}

// ── 纯函数：组合键匹配 ──────────────────────────────────────

void testComboMatches() {
    const bool F = false, T = true;

    // 主键没按下 → 永远不成立
    check(!tn::HotkeyManager::comboMatches(0, F, F, F, F, F), "主键未按下不成立");
    check(!tn::HotkeyManager::comboMatches(TN_MOD_CONTROL, T, F, F, F, F),
          "主键未按下时修饰键再多也不成立");

    // 无修饰键：只有主键按下，且四个修饰键都抬起
    check(tn::HotkeyManager::comboMatches(0, F, F, F, F, T), "裸键按下应成立");
    check(!tn::HotkeyManager::comboMatches(0, T, F, F, F, T),
          "裸键配置在按住 Ctrl 时不应成立（否则会抢走游戏的 Ctrl+Y）");
    check(!tn::HotkeyManager::comboMatches(0, F, T, F, F, T), "裸键 + Shift 不应成立");
    check(!tn::HotkeyManager::comboMatches(0, F, F, T, F, T), "裸键 + Alt 不应成立");
    check(!tn::HotkeyManager::comboMatches(0, F, F, F, T, T), "裸键 + Win 不应成立");

    // 单修饰键
    check(tn::HotkeyManager::comboMatches(TN_MOD_CONTROL, T, F, F, F, T), "Ctrl+键 应成立");
    check(!tn::HotkeyManager::comboMatches(TN_MOD_CONTROL, F, F, F, F, T),
          "配了 Ctrl 但没按 Ctrl 不成立");
    check(!tn::HotkeyManager::comboMatches(TN_MOD_CONTROL, T, T, F, F, T),
          "多按了 Shift 不应成立（要求精确匹配）");

    // 多修饰键
    const unsigned int ctrlShift = TN_MOD_CONTROL | TN_MOD_SHIFT;
    check(tn::HotkeyManager::comboMatches(ctrlShift, T, T, F, F, T), "Ctrl+Shift+键 应成立");
    check(!tn::HotkeyManager::comboMatches(ctrlShift, T, F, F, F, T),
          "Ctrl+Shift 配置只按 Ctrl 不成立");

    // Shift 要能区分左右之外的通用状态（GetAsyncKeyState 的 VK_SHIFT 反映两侧）
    check(tn::HotkeyManager::comboMatches(TN_MOD_SHIFT, F, T, F, F, T), "Shift+键 应成立");

    // 四个修饰键全上
    const unsigned int all = TN_MOD_CONTROL | TN_MOD_SHIFT | TN_MOD_ALT | TN_MOD_WIN;
    check(tn::HotkeyManager::comboMatches(all, T, T, T, T, T), "四修饰键齐全应成立");
    check(!tn::HotkeyManager::comboMatches(all, T, T, T, F, T), "四修饰键缺 Win 不成立");

    // Alt 单独
    check(tn::HotkeyManager::comboMatches(TN_MOD_ALT, F, F, T, F, T), "Alt+键 应成立");
    // Win 单独（V1 的轮询不支持 Win 键，V2 支持）
    check(tn::HotkeyManager::comboMatches(TN_MOD_WIN, F, F, F, T, T), "Win+键 应成立（V2 补齐）");
}

void testPollInterval() {
    // 与 V1 overlay.py:801 的 50ms 一致
    checkEqInt(tn::HotkeyManager::pollIntervalMs(), 50, "轮询间隔应为 50ms");
}

// ── 注册与降级 ──────────────────────────────────────────────

void testApplyAndUnregister() {
    tn::HotkeyManager hk;

    std::vector<tn::HotkeySpec> specs = {
        {"copy", 'C', TN_MOD_CONTROL, true},
        {"focus", 'Y', TN_MOD_SHIFT, true},
    };

    std::string err;
    check(hk.apply(specs, err), "apply 应成功");
    checkEqInt(static_cast<int>(hk.size()), 2, "应记录 2 个热键");

    // 不变量：每个热键要么注册成功、要么降级，不能两者都不是。
    // （RegisterHotKey 可能因被占用而失败，所以不断言具体走哪条）
    const int fallbackCount = static_cast<int>(hk.fallbackIDs().size());
    check(fallbackCount >= 0 && fallbackCount <= 2, "降级数量应在 0..2 之间");
    check(hk.usingFallback() == (fallbackCount > 0), "usingFallback 与降级列表应一致");

    if (fallbackCount == 0) {
        std::printf("    （本机两个热键都注册成功，走 RegisterHotKey）\n");
    } else {
        std::printf("    （本机有 %d 个热键降级为轮询）\n", fallbackCount);
    }

    // 全量替换：旧的要被注销
    std::vector<tn::HotkeySpec> next = {{"only", 'X', TN_MOD_ALT, true}};
    check(hk.apply(next, err), "第二次 apply 应成功");
    checkEqInt(static_cast<int>(hk.size()), 1, "全量替换后只应有 1 个");

    hk.release();
    checkEqInt(static_cast<int>(hk.size()), 0, "release 后应为空");
    check(!hk.usingFallback(), "release 后不应有降级项");
}

// 禁用的条目与无效 VK 都不应被注册
void testSkipsDisabledAndInvalid() {
    tn::HotkeyManager hk;

    std::vector<tn::HotkeySpec> specs = {
        {"enabled", 'A', 0, true},
        {"disabled", 'B', 0, false},
        {"invalid", 0, 0, true},  // vk=0
    };
    std::string err;
    hk.apply(specs, err);

    checkEqInt(static_cast<int>(hk.size()), 1, "只应注册启用的那 1 个");
    check(!err.empty(), "跳过了无效条目应有 warning");

    hk.release();
}

// vk=0 的条目不应导致崩溃，空列表也应安全
void testEdgeCases() {
    tn::HotkeyManager hk;
    std::string err;

    hk.apply({}, err);
    checkEqInt(static_cast<int>(hk.size()), 0, "空列表应注册 0 个");

    // 重复释放
    hk.release();
    hk.release();
    check(true, "重复 release 不应崩溃");
}

// ── 轮询路径 ────────────────────────────────────────────────

// 没有降级项时，poll 应是空操作（不必每秒 20 次 GetAsyncKeyState）
void testPollNoopWhenAllRegistered() {
    tn::HotkeyManager hk;
    std::string err;
    hk.apply({{"x", 'X', TN_MOD_CONTROL | TN_MOD_SHIFT | TN_MOD_ALT, true}}, err);

    if (!hk.usingFallback()) {
        const auto fired = hk.poll();
        check(fired.empty(), "无降级项时 poll 应为空");
    } else {
        skip("该热键在本机降级了，无法验证「无降级项时空转」");
    }
    hk.release();
}

// 轮询节流：连续调用不应每次都真去查按键
void testPollThrottled() {
    tn::HotkeyManager hk;
    std::string err;
    // 用一个必定注册失败的组合来强制降级：Ctrl+Alt+Shift+Win+F12 大概率没被占
    // ——但也不是保证，所以这里只验证「节流不崩溃 + 首次可返回」
    hk.apply({{"rare", VK_F24, TN_MOD_CONTROL | TN_MOD_SHIFT | TN_MOD_ALT | TN_MOD_WIN, true}}, err);

    const auto a = hk.poll();
    const auto b = hk.poll();  // 立刻再调，应被节流
    check(b.size() <= a.size(), "节流后不应比首次触发更多");
    check(true, "连续 poll 不应崩溃");
    hk.release();
}

// WM_HOTKEY 之外的窗口消息不应被当成热键
void testOnMessageIgnoresOtherMessages() {
    tn::HotkeyManager hk;
    std::string err;
    hk.apply({{"x", 'X', TN_MOD_CONTROL, true}}, err);

    check(hk.onMessage(WM_PAINT, 1).empty(), "WM_PAINT 不应触发热键");
    check(hk.onMessage(WM_KEYDOWN, 1).empty(), "WM_KEYDOWN 不应触发热键");
    // 未知的 native id 也不应误触发
    check(hk.onMessage(WM_HOTKEY, 0x7FFF).empty(), "未知 id 不应触发热键");

    hk.release();
}

}  // namespace

int main() {
    std::printf("=== translator_native hotkey 自测 ===\n");

    testComboMatches();
    testPollInterval();
    testSkipsDisabledAndInvalid();
    testEdgeCases();
    testOnMessageIgnoresOtherMessages();

    if (tn::OverlayWindow::hasDesktopSession()) {
        testApplyAndUnregister();
        testPollNoopWhenAllRegistered();
        testPollThrottled();
    } else {
        skip("无桌面会话，跳过热键注册断言");
    }

    std::printf("通过 %d 项，失败 %d 项，跳过 %d 项\n", g_passed, g_failed, g_skipped);
    if (g_failed > 0) {
        std::printf("=== 存在失败 ===\n");
        return 1;
    }
    std::printf("=== 全部通过 ===\n");
    return 0;
}
