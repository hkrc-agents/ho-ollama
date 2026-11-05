#include "control_server.h"
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>
#include <array>
#include <cerrno>
#include <cstring>
#include <stdexcept>

namespace pico::services {
namespace {
constexpr size_t headerSize = 60;
uint64_t U64(const unsigned char *p) {
    uint64_t value = 0; for (size_t n = 0; n < 8; ++n) value = (value << 8) | p[n]; return value;
}
uint32_t U32(const unsigned char *p) {
    uint32_t value = 0; for (size_t n = 0; n < 4; ++n) value = (value << 8) | p[n]; return value;
}
bool Read(int fd, void *output, size_t size) {
    auto *p = static_cast<unsigned char *>(output);
    size_t done = 0;
    while (done < size) {
        ssize_t n = recv(fd, p + done, size - done, 0);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return false;
        done += static_cast<size_t>(n);
    }
    return true;
}
bool Write(int fd, const void *input, size_t size) {
    const auto *p = static_cast<const unsigned char *>(input);
    size_t done = 0;
    while (done < size) {
        ssize_t n = send(fd, p + done, size - done, MSG_NOSIGNAL);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return false;
        done += static_cast<size_t>(n);
    }
    return true;
}
void Reply(int fd, uint32_t status, const std::string &body) {
    uint32_t header[3] = {htonl(0x504e5231), htonl(status), htonl(static_cast<uint32_t>(body.size()))}; // PNR1
    if (Write(fd, header, sizeof(header))) Write(fd, body.data(), body.size());
}
std::string Error(const std::string &message) {
    Record error; error.error = message;
    return "{\"ok\":false,\"diagnostic\":" + AuditJSON("control_error", error) + "}";
}
}

ControlServer::ControlServer(Registry &registry, std::string hex) : registry_(registry) {
    if (hex.size() != 64) throw std::invalid_argument("private capability must contain 32 random bytes");
    for (size_t n = 0; n < hex.size(); n += 2) {
        auto digit = [](char c) -> int { return c >= '0' && c <= '9' ? c - '0' : c >= 'a' && c <= 'f' ? c - 'a' + 10 : -1; };
        int a = digit(hex[n]), b = digit(hex[n+1]);
        if (a < 0 || b < 0) throw std::invalid_argument("invalid capability hex");
        capability_ += static_cast<char>((a << 4) | b);
    }
}
ControlServer::~ControlServer() { Stop(); }
std::string ControlServer::Start() {
    if (listener_ >= 0 || stopping_) throw std::runtime_error("control server cannot be restarted");
    listener_ = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (listener_ < 0) throw std::runtime_error("control socket unavailable");
    sockaddr_in addr{}; addr.sin_family = AF_INET; addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    socklen_t size = sizeof(addr);
    if (bind(listener_, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) || listen(listener_, 8) ||
        getsockname(listener_, reinterpret_cast<sockaddr *>(&addr), &size)) {
        close(listener_); listener_ = -1; throw std::runtime_error("control loopback listen failed");
    }
    try {
        for (int n = 0; n < 2; ++n) workers_.emplace_back(&ControlServer::Worker, this);
        workers_.emplace_back([this]() {
            while (!stopping_) { registry_.Pump(); usleep(25000); }
        });
    }
    catch (...) { Stop(); throw; }
    return "127.0.0.1:" + std::to_string(ntohs(addr.sin_port));
}
void ControlServer::Stop() {
    if (stopping_.exchange(true)) return;
    if (listener_ >= 0) shutdown(listener_, SHUT_RDWR);
    {
        std::lock_guard<std::mutex> lock(socketsMutex_);
        for (int fd : sockets_) shutdown(fd, SHUT_RDWR);
    }
    for (auto &thread : workers_) if (thread.joinable()) thread.join();
    workers_.clear();
    if (listener_ >= 0) { close(listener_); listener_ = -1; }
}
void ControlServer::Worker() {
    while (!stopping_) {
        int fd = accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
        if (fd < 0) { if (errno == EINTR) continue; if (stopping_) break; continue; }
        {
            std::lock_guard<std::mutex> lock(socketsMutex_);
            if (stopping_) { close(fd); break; }
            sockets_.insert(fd);
        }
        timeval timeout{2, 0};
        if (!setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) &&
            !setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout))) {
            try { Handle(fd); } catch (...) { Reply(fd, 500, Error("native control failed")); }
        }
        { std::lock_guard<std::mutex> lock(socketsMutex_); sockets_.erase(fd); close(fd); }
    }
}
void ControlServer::Handle(int fd) {
    std::array<unsigned char, headerSize> header{};
    if (!Read(fd, header.data(), header.size())) return;
    if (memcmp(header.data(), "PNS1", 4) || header[6] || header[7]) { Reply(fd, 400, Error("invalid protocol header")); return; }
    unsigned difference = 0;
    for (size_t n = 0; n < 32; ++n) difference |= header[28+n] ^ static_cast<unsigned char>(capability_[n]);
    if (difference) { Reply(fd, 401, Error("invalid private capability")); return; }
    uint32_t size = U32(header.data()+24);
    if (size > 16384) { Reply(fd, 400, Error("oversized native control payload")); return; }
    std::string payload(size, '\0');
    if (size && !Read(fd, payload.data(), size)) return;
    const Instance caller = U64(header.data()+8), target = U64(header.data()+16);
    unsigned opcode = header[4];
    if (opcode == 1) {
        if (target || header[5] == 0 || header[5] > 4 || !registry_.CanManage(caller, caller)) {
            Reply(fd, 403, Error("caller cannot create this service")); return;
        }
        auto start = registry_.Start({static_cast<Role>(header[5]), caller, payload});
        Reply(fd, start.ok ? 0 : 409, "{\"ok\":" + std::string(start.ok ? "true" : "false") +
            ",\"instance\":" + std::to_string(start.instance) + ",\"diagnostic\":" + Error(start.error) + "}");
        return;
    }
    if (size || header[5] || !registry_.CanManage(caller, target)) {
        Reply(fd, 403, Error("caller cannot manage this instance")); return;
    }
    if (opcode == 2) {
        Record record; bool drained = false, quiescent = false;
        if (registry_.Inspect(target, record, drained, &quiescent)) {
            Reply(fd, 0, "{\"record\":" + AuditJSON("snapshot", record) +
                ",\"drained\":" + (drained ? "true" : "false") +
                ",\"quiescent\":" + (quiescent ? "true" : "false") + "}"); return;
        }
    } else if (opcode == 3) {
        bool ok = registry_.AbortTree(target); registry_.Pump();
        Reply(fd, ok ? 0 : 409, std::string("{\"ok\":") + (ok ? "true}" : "false}")); return;
    } else if (opcode == 4) {
        bool ok = registry_.CancelIntent(target);
        Reply(fd, ok ? 0 : 409, std::string("{\"ok\":") + (ok ? "true}" : "false}")); return;
    } else if (opcode == 5) {
        auto replacement = registry_.Replace(target);
        Reply(fd, replacement.ok ? 0 : 409, "{\"ok\":" + std::string(replacement.ok ? "true" : "false") +
            ",\"instance\":" + std::to_string(replacement.instance) + "}"); return;
    } else if (opcode == 6) {
        bool ok = registry_.RequestStop(target);
        Reply(fd, ok ? 0 : 409, std::string("{\"ok\":") + (ok ? "true}" : "false}")); return;
    }
    Reply(fd, 400, Error("unknown native control opcode"));
}
} // namespace pico::services
