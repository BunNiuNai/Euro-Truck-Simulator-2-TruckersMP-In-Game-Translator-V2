// 全局热键 —— RegisterHotKey 为主、轮询为降级（ADR-015，Q9 已决）。
//
// ⚠️ 为什么是双路径：
//   `RegisterHotKey` 是系统级的，不会被游戏窗口拦截，这是它相对轮询的**唯一**
//   但有决定性意义的优势。可它有一个硬限制：**同一个 (mods, vk) 组合在同一
//   会话内只能被注册一次**。别的程序占了（或者本程序上一次没注销干净）就
//   注册不上，此时功能会**静默失效**——这正是最糟的失败方式。
//   所以注册失败时降级为轮询：多耗一点 CPU，但功能还在。
//
//   V1 的 README 一直宣称用的是 RegisterHotKey（见缺陷 D11），实际生效的
//   却一直是 `overlay.py` 里每 50ms 一次的 GetAsyncKeyState 轮询；
//   为 RegisterHotKey 写的那 203 行 `hotkey_manager.py` 从未被接线（D10）。
//   V2 两者都做，且如实上报当前走的是哪条路径。
//
// 关于热键解析：native **不解析热键字符串**，只接收 Go 侧解析好的 VK 与
// MOD_* 位掩码（缺陷 D7：V1 有四处解析器）。
#ifndef TN_HOTKEY_HPP
#define TN_HOTKEY_HPP

#include <string>
#include <vector>

namespace tn {

// 一个热键的期望状态。字段与 IPC 的 hotkey.set 载荷一致。
struct HotkeySpec {
    std::string id;         // 由 Go 分配，事件里原样回传（如 "toggle" / "copy"）
    unsigned int vk = 0;    // 虚拟键码（0 表示无效，会被跳过）
    unsigned int mods = 0;  // TN_MOD_* 位掩码
    bool enabled = true;
};

// 热键管理器。
class HotkeyManager {
public:
    HotkeyManager() = default;
    ~HotkeyManager();
    HotkeyManager(const HotkeyManager&) = delete;
    HotkeyManager& operator=(const HotkeyManager&) = delete;

    // 全量替换热键列表（对应 IPC 的 hotkey.set）。
    //
    // 先注销旧的再注册新的——期望状态是全量的，不做增量合并。
    // 注册失败的条目自动落到轮询路径，不会整体失败。
    bool apply(const std::vector<HotkeySpec>& specs, std::string& err);

    // 处理一条窗口消息。返回被触发的热键 id（空串表示与热键无关）。
    std::string onMessage(unsigned int msg, unsigned long long wparam);

    // 轮询降级路径。内部按 kPollIntervalMs 节流，可以每帧调用。
    // 返回**本次新触发**的热键 id（带边沿检测，按住不会重复触发）。
    std::vector<std::string> poll();

    // 诊断：哪些热键走了降级路径。空表示全部用的 RegisterHotKey。
    const std::vector<std::string>& fallbackIDs() const { return fallbackIDs_; }
    bool usingFallback() const { return !fallbackIDs_.empty(); }

    // 注销全部热键（停止/重放前调用）。
    void release();

    // 已注册的热键数量（含降级的）。
    size_t size() const { return entries_.size(); }

    // ── 纯函数（可单测）────────────────────────────────────

    // 判断组合键是否成立。
    //
    // 抽成纯函数是为了可单测：轮询路径的全部判断逻辑都在这里，
    // GetAsyncKeyState 只是给它喂当前状态。mods 为 0 时只看主键。
    static bool comboMatches(unsigned int mods, bool ctrlDown, bool shiftDown,
                             bool altDown, bool winDown, bool keyDown);

    // 轮询间隔（毫秒）。V1 是 50ms（overlay.py:801）。
    static int pollIntervalMs();

private:
    struct Entry {
        std::string id;
        unsigned int vk = 0;
        unsigned int mods = 0;
        int nativeId = 0;   // RegisterHotKey 用的整数 id；0 表示走轮询降级
        bool wasDown = false;  // 轮询用的边沿检测状态
    };

    std::vector<Entry> entries_;
    std::vector<std::string> fallbackIDs_;
    int nextNativeId_ = 1;
    unsigned long long lastPollTick_ = 0;
};

}  // namespace tn

#endif  // TN_HOTKEY_HPP
