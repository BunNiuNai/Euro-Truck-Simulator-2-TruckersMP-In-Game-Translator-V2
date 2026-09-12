// Named Pipe 服务端实现（Windows）。
//
// ⚠️ 全程使用 **overlapped（重叠）I/O**，原因见 pipe.hpp 的文件头：
//   同步句柄会被 Windows 序列化，读长期阻塞时写请求永远排不到，
//   与 Go 侧「常驻读循环 + 调用方写」的结构必然死锁。
#include "pipe.hpp"

#include "../include/translator_native.h"

#include <windows.h>

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <vector>

namespace tn {

namespace {

constexpr DWORD kBufferSize = 64 * 1024;

// 单次重叠操作的等待上限。给得足够宽（一帧的读写是毫秒级的事），
// 只用于在真出问题时把线程捞回来，而不是当作正常的超时机制。
constexpr DWORD kIoWaitMs = 30000;

HANDLE asHandle(void* p) { return reinterpret_cast<HANDLE>(p); }
void* asVoid(HANDLE h) { return reinterpret_cast<void*>(h); }

// 4 字节小端长度前缀
void putLength(std::vector<char>& out, std::uint32_t n) {
    out.push_back(static_cast<char>(n & 0xFF));
    out.push_back(static_cast<char>((n >> 8) & 0xFF));
    out.push_back(static_cast<char>((n >> 16) & 0xFF));
    out.push_back(static_cast<char>((n >> 24) & 0xFF));
}

std::uint32_t getLength(const char* p) {
    return static_cast<std::uint32_t>(static_cast<unsigned char>(p[0])) |
           (static_cast<std::uint32_t>(static_cast<unsigned char>(p[1])) << 8) |
           (static_cast<std::uint32_t>(static_cast<unsigned char>(p[2])) << 16) |
           (static_cast<std::uint32_t>(static_cast<unsigned char>(p[3])) << 24);
}

}  // namespace

PipeServer::~PipeServer() { close(); }

bool PipeServer::create(const std::wstring& fullName) {
    close();
    name_ = fullName;

    // FILE_FLAG_OVERLAPPED 是这里的重点：见文件头说明。
    HANDLE h = CreateNamedPipeW(
        fullName.c_str(),
        PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED,
        PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT,
        1,             // 单实例：Go 是唯一调用方
        kBufferSize,
        kBufferSize,
        0,
        nullptr);
    if (h == INVALID_HANDLE_VALUE) return false;

    // 每个方向一个事件。用自动重置事件：系统在完成时置位，
    // WaitForSingleObject 取走后自动复位，不必手动 ResetEvent。
    connectEvent_ = asVoid(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    readEvent_ = asVoid(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    writeEvent_ = asVoid(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    if (!connectEvent_ || !readEvent_ || !writeEvent_) {
        close();
        return false;
    }

    handle_ = asVoid(h);
    return true;
}

bool PipeServer::waitOverlapped(void* overlapped, unsigned long* transferred) {
    auto* ov = static_cast<OVERLAPPED*>(overlapped);
    const DWORD w = WaitForSingleObject(ov->hEvent, kIoWaitMs);
    if (w != WAIT_OBJECT_0) {
        // 超时或事件出错：必须取消这次 I/O，否则 OVERLAPPED 结构被销毁后
        // 系统仍可能往里写，那会踩坏栈。
        CancelIo(asHandle(handle_));
        std::printf("[pipe] 重叠操作等待失败 code=%lu\n", w);
        std::fflush(stdout);
        return false;
    }

    DWORD n = 0;
    if (!GetOverlappedResult(asHandle(handle_), ov, &n, FALSE)) {
        const DWORD e = GetLastError();
        if (e != ERROR_BROKEN_PIPE && e != ERROR_OPERATION_ABORTED) {
            std::printf("[pipe] GetOverlappedResult 失败 err=%lu\n", e);
            std::fflush(stdout);
        }
        return false;
    }
    if (transferred) *transferred = n;
    return true;
}

bool PipeServer::waitForClient() {
    if (!handle_) return false;

    OVERLAPPED ov{};
    ov.hEvent = asHandle(connectEvent_);

    const BOOL ok = ConnectNamedPipe(asHandle(handle_), &ov);
    if (ok) return true;  // 极少见：连接在我们调用前就已完成

    const DWORD err = GetLastError();
    if (err == ERROR_PIPE_CONNECTED) return true;  // 客户端先连上了

    if (err != ERROR_IO_PENDING) {
        std::printf("[pipe] ConnectNamedPipe 失败 err=%lu\n", err);
        std::fflush(stdout);
        return false;
    }
    return waitOverlapped(&ov, nullptr);
}

PipeStatus PipeServer::poll() {
    if (!handle_) return PipeStatus::Broken;

    DWORD avail = 0;
    // PeekNamedPipe 从不阻塞。返回 FALSE 即对端已断开
    // （ERROR_BROKEN_PIPE / ERROR_PIPE_NOT_CONNECTED / ERROR_BAD_PIPE）。
    if (!PeekNamedPipe(asHandle(handle_), nullptr, 0, nullptr, &avail, nullptr)) {
        return PipeStatus::Broken;
    }
    return avail > 0 ? PipeStatus::Data : PipeStatus::NoData;
}

bool PipeServer::readExact(char* buf, unsigned long want) {
    unsigned long got = 0;
    while (got < want) {
        OVERLAPPED ov{};
        ov.hEvent = asHandle(readEvent_);

        DWORD n = 0;
        if (ReadFile(asHandle(handle_), buf + got, want - got, &n, &ov)) {
            // 立即完成（数据已在缓冲区里）
        } else {
            const DWORD err = GetLastError();
            if (err != ERROR_IO_PENDING) {
                std::printf("[pipe] ReadFile 失败 err=%lu（已读 %lu/%lu 字节）\n", err, got, want);
                std::fflush(stdout);
                return false;
            }
            DWORD done = 0;
            if (!waitOverlapped(&ov, &done)) return false;
            n = done;
        }

        if (n == 0) {
            std::printf("[pipe] 读到 0 字节（对端已关闭）\n");
            std::fflush(stdout);
            return false;
        }
        got += n;
    }
    return true;
}

bool PipeServer::writeExact(const char* buf, unsigned long want) {
    unsigned long sent = 0;
    while (sent < want) {
        OVERLAPPED ov{};
        ov.hEvent = asHandle(writeEvent_);

        DWORD n = 0;
        if (WriteFile(asHandle(handle_), buf + sent, want - sent, &n, &ov)) {
            // 立即完成
        } else {
            const DWORD err = GetLastError();
            if (err != ERROR_IO_PENDING) {
                std::printf("[pipe] WriteFile 失败 err=%lu（已写 %lu/%lu 字节）\n", err, sent, want);
                std::fflush(stdout);
                return false;
            }
            DWORD done = 0;
            if (!waitOverlapped(&ov, &done)) return false;
            n = done;
        }

        if (n == 0) return false;
        sent += n;
    }
    return true;
}

bool PipeServer::readFrame(std::string& out, std::string& err) {
    if (!handle_) { err = "管道未打开"; return false; }

    char hdr[4];
    if (!readExact(hdr, 4)) { err = "读取帧头失败（对端可能已断开）"; return false; }

    const std::uint32_t len = getLength(hdr);
    if (len > TN_MAX_FRAME_BYTES) {
        err = "帧长度超过上限 " + std::to_string(TN_MAX_FRAME_BYTES);
        return false;
    }
    if (len == 0) { out.clear(); return true; }

    std::vector<char> body(len);
    if (!readExact(body.data(), len)) { err = "读取帧体失败"; return false; }
    out.assign(body.data(), len);
    return true;
}

bool PipeServer::writeFrame(const std::string& json, std::string& err) {
    if (!handle_) { err = "管道未打开"; return false; }
    if (json.size() > TN_MAX_FRAME_BYTES) { err = "待发送帧超过上限"; return false; }

    std::vector<char> frame;
    frame.reserve(json.size() + 4);
    putLength(frame, static_cast<std::uint32_t>(json.size()));
    frame.insert(frame.end(), json.begin(), json.end());

    if (!writeExact(frame.data(), static_cast<unsigned long>(frame.size()))) {
        err = "写入帧失败";
        return false;
    }
    return true;
}

void PipeServer::disconnect() {
    if (!handle_) return;
    FlushFileBuffers(asHandle(handle_));
    DisconnectNamedPipe(asHandle(handle_));
}

void PipeServer::close() {
    for (void** ev : {&connectEvent_, &readEvent_, &writeEvent_}) {
        if (*ev) {
            CloseHandle(asHandle(*ev));
            *ev = nullptr;
        }
    }
    if (handle_) {
        CloseHandle(asHandle(handle_));
        handle_ = nullptr;
    }
}

}  // namespace tn
