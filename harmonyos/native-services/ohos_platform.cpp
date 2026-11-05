#include "ohos_platform.h"
#include <AbilityKit/native_child_process.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <cerrno>
#include <fstream>
#include <sstream>
#include <stdexcept>
#include <utility>

namespace pico::services {
namespace {
std::mutex callbackMutex;
Platform::Exit callback;
const void *callbackOwner = nullptr;

void OfficialExit(int32_t pid, int32_t signal) {
    Platform::Exit handler;
    { std::lock_guard<std::mutex> lock(callbackMutex); handler = callback; }
    if (handler) handler(pid, signal);
}
uint64_t ReadTicks(int32_t pid) {
    if (pid <= 1) return 0;
    std::string root = "/proc/" + std::to_string(pid);
    const int directory = open(root.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (directory < 0) return 0;
    struct stat info{};
    if (fstat(directory, &info) || info.st_uid != getuid()) { close(directory); return 0; }
    const int fd = openat(directory, "stat", O_RDONLY | O_CLOEXEC);
    close(directory);
    if (fd < 0) return 0;
    char buffer[8192]; ssize_t count;
    do { count = read(fd, buffer, sizeof(buffer)); } while (count < 0 && errno == EINTR);
    close(fd);
    if (count <= 0 || count == static_cast<ssize_t>(sizeof(buffer))) return 0;
    std::string text(buffer, static_cast<size_t>(count));
    auto last = text.rfind(')');
    if (last == std::string::npos) return 0;
    std::istringstream fields(text.substr(last + 1));
    std::string field;
    for (int n = 3; n <= 22; ++n) {
        if (!(fields >> field)) return 0;
        if (n == 3 && (field == "Z" || field == "X" || field == "x")) return 0;
    }
    try { size_t end = 0; uint64_t ticks = std::stoull(field, &end); return end == field.size() ? ticks : 0; }
    catch (...) { return 0; }
}

class OHOSPlatform final : public Platform {
public:
    OHOSPlatform(const std::string &path, std::map<Role, std::string> entries,
                 std::function<std::string(const Record &)> bootstrap)
        : entries_(std::move(entries)), bootstrap_(std::move(bootstrap)) {
        audit_ = open(path.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_APPEND | O_CLOEXEC | O_NOFOLLOW, 0600);
        if (audit_ < 0) throw std::runtime_error("exclusive native lifecycle audit unavailable");
        for (const auto &[role, entry] : entries_) {
            (void)role;
            const auto colon = entry.find(':');
            if (colon == std::string::npos || colon == 0 || colon + 1 == entry.size() ||
                entry.find('/') != std::string::npos || entry.find("..") != std::string::npos ||
                entry.find('\0') != std::string::npos) {
                close(audit_); audit_ = -1;
                throw std::invalid_argument("invalid trusted native child entry");
            }
        }
    }
    ~OHOSPlatform() override { SetExitHandler({}); if (audit_ >= 0) close(audit_); }
    bool SetExitHandler(Exit handler) override {
        if (!handler) {
            bool ours = false;
            {
                std::lock_guard<std::mutex> lock(callbackMutex);
                if (callbackOwner == this) { callback = {}; callbackOwner = nullptr; ours = true; }
            }
            return !ours || OH_Ability_UnregisterNativeChildProcessExitCallback(OfficialExit) == NCP_NO_ERROR;
        }
        {
            std::lock_guard<std::mutex> lock(callbackMutex);
            if (callbackOwner && callbackOwner != this) return false;
            callbackOwner = this; callback = std::move(handler);
        }
        if (OH_Ability_RegisterNativeChildProcessExitCallback(OfficialExit) != NCP_NO_ERROR) {
            std::lock_guard<std::mutex> lock(callbackMutex);
            callback = {}; callbackOwner = nullptr; return false;
        }
        return true;
    }
    Started Start(const Record &record) override {
        auto entry = entries_.find(record.spec.role);
        if (entry == entries_.end()) return {NCP_ERR_INVALID_PARAM, -1, 0};
        std::string bootstrap;
        try { bootstrap = bootstrap_ ? bootstrap_(record) : Bootstrap(record); }
        catch (...) { return {NCP_ERR_INVALID_PARAM, -1, 0}; }
        if (bootstrap.empty() || bootstrap.size() > 65536 || bootstrap.find('\0') != std::string::npos)
            return {NCP_ERR_INVALID_PARAM, -1, 0};
        NativeChildProcess_Args args{bootstrap.data(), {nullptr}};
        NativeChildProcess_Options options{NCP_ISOLATION_MODE_NORMAL, 0};
        int32_t pid = -1;
        auto code = OH_Ability_StartNativeChildProcess(entry->second.c_str(), args, options, &pid);
        // Callback may precede return; Registry reconciles that event. A missing
        // identity without a callback remains failed/unresolved, never "gone".
        uint64_t ticks = code == NCP_NO_ERROR ? ReadTicks(pid) : 0;
        int callbackCode = 0;
        if (code == NCP_NO_ERROR) {
            // Re-register the SAME callback after each successful creation.
            // The SDK documents repeated registration as keeping one callback.
            // In the emulator a full child-tree turnover can replace the
            // underlying child host; do not assume a prior subscription survives.
            callbackCode = OH_Ability_RegisterNativeChildProcessExitCallback(OfficialExit);
            Record proof = record;
            proof.pid = pid; proof.startTicks = ticks; proof.officialCreated = true;
            if (callbackCode) proof.error = "callback rearm code=" + std::to_string(callbackCode);
            if (!Audit("exit_callback_rearmed", proof)) callbackCode = -1;
        }
        return {code, pid, ticks, callbackCode};
    }
    bool Matches(int32_t pid, uint64_t ticks) override { return ticks && ReadTicks(pid) == ticks; }
    ExitObservation ObserveExit(int32_t pid, uint64_t ticks) override {
        if (pid <= 1) return ExitObservation::Unknown;
        const auto path = "/proc/" + std::to_string(pid);
        int fd = open(path.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
        if (fd < 0) return errno == ENOENT || errno == ESRCH ? ExitObservation::Gone : ExitObservation::Unknown;
        close(fd);
        return ticks && ReadTicks(pid) == ticks ? ExitObservation::Alive : ExitObservation::Unknown;
    }
    int Kill(int32_t pid) override { return OH_Ability_KillChildProcess(pid); }
    bool Audit(const char *event, const Record &record) override {
        std::string line = AuditJSON(event, record);
        std::lock_guard<std::mutex> lock(auditMutex_);
        size_t offset = 0;
        while (offset < line.size()) {
            ssize_t count = write(audit_, line.data() + offset, line.size() - offset);
            if (count < 0 && errno == EINTR) continue;
            if (count <= 0) return false;
            offset += static_cast<size_t>(count);
        }
        return fsync(audit_) == 0;
    }
private:
    int audit_ = -1;
    std::mutex auditMutex_;
    std::map<Role, std::string> entries_;
    std::function<std::string(const Record &)> bootstrap_;
};
}

std::shared_ptr<Platform> CreateOHOSPlatform(const std::string &path,
                                          const std::map<Role, std::string> &entries,
                                          std::function<std::string(const Record &)> bootstrap) {
    return std::make_shared<OHOSPlatform>(path, entries, std::move(bootstrap));
}
std::string NewSessionID() {
    int fd = open("/dev/urandom", O_RDONLY | O_CLOEXEC);
    if (fd < 0) throw std::runtime_error("session randomness unavailable");
    unsigned char bytes[32]; size_t offset = 0;
    while (offset < sizeof(bytes)) {
        ssize_t n = read(fd, bytes + offset, sizeof(bytes) - offset);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) { close(fd); throw std::runtime_error("session randomness read failed"); }
        offset += static_cast<size_t>(n);
    }
    close(fd);
    static const char hex[] = "0123456789abcdef";
    std::string session;
    for (unsigned char c : bytes) { session += hex[c >> 4]; session += hex[c & 15]; }
    return session;
}
} // namespace pico::services
