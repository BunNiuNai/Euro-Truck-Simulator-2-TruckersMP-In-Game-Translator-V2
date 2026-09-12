// input 模块自测。
//
// 分两部分：
//   1. 纯函数断言（修饰键位掩码展开）—— 任何环境都能跑
//   2. 真实剪贴板读写与按键模拟 —— 需要交互式桌面会话；没有会话时明确跳过
//
// 不测「发送完整序列」：那会真的往当前焦点窗口敲键盘、粘贴文本、按回车，
// 在自动化测试里既不可控也有破坏性（可能把内容敲进任意程序）。它的组成部件
// 已经分别被测到了。
#include "../src/input.hpp"
#include "../src/window.hpp"  // hasDesktopSession：无桌面会话时跳过剪贴板断言

#include <windows.h>

#include <cstdio>
#include <string>

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

// ── 1. 纯函数：修饰键展开 ────────────────────────────────────

void testModifierVKs() {
    unsigned int keys[4] = {0, 0, 0, 0};

    checkEqInt(tn::modifierVKs(0, keys, 4), 0, "无修饰键应为 0 个");

    checkEqInt(tn::modifierVKs(TN_MOD_CONTROL, keys, 4), 1, "Ctrl 应为 1 个");
    checkEqInt(static_cast<int>(keys[0]), static_cast<int>(VK_CONTROL), "Ctrl 的 VK");

    checkEqInt(tn::modifierVKs(TN_MOD_SHIFT, keys, 4), 1, "Shift 应为 1 个");
    checkEqInt(static_cast<int>(keys[0]), static_cast<int>(VK_SHIFT), "Shift 的 VK");

    checkEqInt(tn::modifierVKs(TN_MOD_ALT, keys, 4), 1, "Alt 应为 1 个");
    checkEqInt(static_cast<int>(keys[0]), static_cast<int>(VK_MENU), "Alt 的 VK");

    checkEqInt(tn::modifierVKs(TN_MOD_WIN, keys, 4), 1, "Win 应为 1 个");
    checkEqInt(static_cast<int>(keys[0]), static_cast<int>(VK_LWIN), "Win 的 VK");

    // 组合：顺序固定为 Ctrl → Shift → Alt → Win。
    // 顺序有实际影响（Alt+Shift 会切输入法），所以固定下来并锁在测试里。
    const unsigned int all = TN_MOD_CONTROL | TN_MOD_SHIFT | TN_MOD_ALT | TN_MOD_WIN;
    checkEqInt(tn::modifierVKs(all, keys, 4), 4, "四个修饰键应展开为 4 个");
    checkEqInt(static_cast<int>(keys[0]), static_cast<int>(VK_CONTROL), "第 1 个是 Ctrl");
    checkEqInt(static_cast<int>(keys[1]), static_cast<int>(VK_SHIFT), "第 2 个是 Shift");
    checkEqInt(static_cast<int>(keys[2]), static_cast<int>(VK_MENU), "第 3 个是 Alt");
    checkEqInt(static_cast<int>(keys[3]), static_cast<int>(VK_LWIN), "第 4 个是 Win");

    // 缓冲区不足时不能越界写。
    //
    // ⚠️ 变量不能叫 small —— rpcndr.h 里有 `#define small char`（给 MIDL 用的
    // 老宏），会把这一行展开成 `unsigned int char[2]`。
    // Windows 头文件里这类「看似普通的标识符其实是宏」的坑不止一个
    // （near / far / small / interface 都中招），命名时避开它们。
    unsigned int twoKeys[2] = {0, 0};
    checkEqInt(tn::modifierVKs(all, twoKeys, 2), 2, "缓冲区只有 2 个时应只写 2 个");

    // 非法参数不崩
    checkEqInt(tn::modifierVKs(all, nullptr, 4), 0, "out 为 null 应返回 0");
    checkEqInt(tn::modifierVKs(all, keys, 0), 0, "maxOut 为 0 应返回 0");

    // 未知位被忽略
    checkEqInt(tn::modifierVKs(0x8000u, keys, 4), 0, "未知修饰键位应被忽略");
}

// ── 2. 真实剪贴板 ────────────────────────────────────────────

void testClipboardRoundTrip() {
    tn::InputSender in;
    if (!in.ready()) {
        // ready() 只在窗口创建失败时为 false；这里主动触发一次
    }

    const std::wstring original = in.getClipboardText();

    const std::wstring probe = L"ETS2 Translator 剪贴板自测 — 中文与 emoji 🚚";
    if (!in.setClipboardText(probe)) {
        skip("写入剪贴板失败（桌面会话不可用或剪贴板被独占）");
        return;
    }
    check(true, "写入剪贴板");

    const std::wstring got = in.getClipboardText();
    check(got == probe, "读回的剪贴板内容应与写入一致（含中文与 emoji）");

    // 长文本（超过 CF_UNICODETEXT 的小缓冲）
    std::wstring longText;
    for (int i = 0; i < 500; ++i) longText += L"行" + std::to_wstring(i) + L" ";
    check(in.setClipboardText(longText), "写入长文本");
    check(in.getClipboardText() == longText, "长文本往返一致");

    // 空串往返（清空剪贴板内容）
    check(in.setClipboardText(L""), "写入空串");
    checkEqInt(static_cast<int>(in.getClipboardText().size()), 0, "空串写入后读回也是空");

    // 还原用户原有的剪贴板内容——测试不该破坏用户的数据
    if (!original.empty()) {
        check(in.setClipboardText(original), "还原原剪贴板内容");
    }
}

// 剪贴板窗口必须在多次操作之间保持存活：
// Windows 在拥有者窗口销毁时会清空剪贴板，如果每次操作都重建窗口，
// 上一次写的内容就没了。
//
// ⚠️ 剪贴板是**用户的**资源：测试改了就必须还原。这个用例曾经忘了还原，
// 结果每跑一次测试，用户的剪贴板就被留成 "persist-2"。
void testClipboardOwnerPersists() {
    tn::InputSender in;

    const std::wstring original = in.getClipboardText();

    if (!in.setClipboardText(L"persist-check")) {
        skip("剪贴板不可用，跳过拥有者存活性检查");
        return;
    }
    // 中间做点别的（触发内部状态变化）
    for (int i = 0; i < 3; ++i) {
        check(in.setClipboardText(L"persist-" + std::to_wstring(i)), "连续写入");
    }
    check(in.getClipboardText() == L"persist-2", "最后一次写入应仍然可读（拥有者窗口未被重建）");
    check(in.ready(), "剪贴板窗口应保持就绪");

    // 还原用户原有的剪贴板内容
    if (!original.empty()) {
        check(in.setClipboardText(original), "还原原剪贴板内容");
    }
}

void testPressKeyRejectsInvalidVK() {
    // VK=0 不是合法按键，必须被拒绝而不是发出一个空事件
    check(!tn::pressKey(0), "pressKey(0) 应返回 false");
    check(!tn::pressCombo(0, 0), "pressCombo(0,0) 应返回 false");
}

}  // namespace

int main() {
    std::printf("=== translator_native input 自测 ===\n");

    testModifierVKs();
    testPressKeyRejectsInvalidVK();

    if (tn::OverlayWindow::hasDesktopSession()) {
        testClipboardRoundTrip();
        testClipboardOwnerPersists();
    } else {
        skip("无交互式桌面会话，跳过剪贴板相关断言");
    }

    std::printf("通过 %d 项，失败 %d 项，跳过 %d 项\n", g_passed, g_failed, g_skipped);
    if (g_failed > 0) {
        std::printf("=== 存在失败 ===\n");
        return 1;
    }
    std::printf("=== 全部通过 ===\n");
    return 0;
}
