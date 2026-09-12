// Named Pipe 服务端 —— Go ↔ Native 的唯一通道（ADR-004）。
//
// 帧格式（与 Go 侧 internal/ipc 严格一致）：
//   +----------------+------------------------------+
//   | 4 bytes LE     | N bytes                      |
//   | payload length | UTF-8 JSON payload           |
//   +----------------+------------------------------+
//   上限 TN_MAX_FRAME_BYTES（8MB），超限即断开并记录错误。
//
// ⚠️ 为什么必须用 **overlapped（重叠）I/O**：
//   Windows 对以同步方式打开的句柄会**序列化其上的 I/O 操作**。而 Go 侧的
//   结构是「常驻读循环 + 调用方写请求」——读长期阻塞在 ReadFile 上时，
//   写请求会被排到它后面，于是双方互相等待：Go 等写完成，服务端等数据，
//   而数据要等写完成。表现为「连上了但一句话都说不通」，且因为阻塞发生在
//   系统调用里，任何 context 超时都救不回来。
//   把两端都改成 overlapped 后，读写可以真正并发，而且等待可以带超时。
//
// 设计要点：
//   - 串行处理连接（单客户端：Go 是唯一调用方），避免并发状态
//   - ReadFile/WriteFile 可能短读短写，必须循环到满足长度——这是管道编程最常见的坑
//   - 不在这里做业务判断，只负责「收一帧、发一帧」
#ifndef TN_PIPE_HPP
#define TN_PIPE_HPP

#include <string>

namespace tn {

// 非阻塞探测的结果。必须区分「暂无数据」与「对端断开」——
// 合成一个 bool 会让主循环在断线后空转，永远发现不了连接已经没了。
enum class PipeStatus {
    NoData,  // 连接还在，但暂时没有可读数据
    Data,    // 至少有一帧的开头可读
    Broken   // 对端已断开或管道出错，调用方应断开重连
};

class PipeServer {
public:
    PipeServer() = default;
    ~PipeServer();
    PipeServer(const PipeServer&) = delete;
    PipeServer& operator=(const PipeServer&) = delete;

    // 创建管道实例（名字形如 \\.\pipe\xxx）
    bool create(const std::wstring& fullName);

    // 阻塞等待客户端连接。返回 false 表示出错（可重试）。
    bool waitForClient();

    // 非阻塞探测：连接上是否已有可读数据。
    //
    // 为什么需要它：主循环若直接阻塞在 readFrame 上，窗口消息就没人泵，
    // 建出来的窗口会假死（不重绘、不响应 WM_NCHITTEST，拖拽缩放全失效）。
    PipeStatus poll();

    // 底层句柄，供 MsgWaitForMultipleObjects 同时等待「管道数据」与「窗口消息」
    void* rawHandle() const { return handle_; }

    // 读一帧；返回 false 表示对端关闭或出错（调用方应断开重来）
    bool readFrame(std::string& out, std::string& err);

    // 写一帧
    bool writeFrame(const std::string& json, std::string& err);

    // 断开当前连接（保留管道实例，可再次 waitForClient）
    void disconnect();

    // 彻底关闭句柄
    void close();

    const std::wstring& name() const { return name_; }

private:
    bool readExact(char* buf, unsigned long want);
    bool writeExact(const char* buf, unsigned long want);

    // 等待一个重叠操作完成。超时返回 false（并已取消该操作）。
    bool waitOverlapped(void* overlapped, unsigned long* transferred);

    void* handle_ = nullptr;        // HANDLE，用 void* 避免在本头文件里引入 windows.h
    void* connectEvent_ = nullptr;  // HANDLE：ConnectNamedPipe 用
    void* readEvent_ = nullptr;     // HANDLE：读完成通知
    void* writeEvent_ = nullptr;    // HANDLE：写完成通知
    std::wstring name_;
};

}  // namespace tn

#endif  // TN_PIPE_HPP
