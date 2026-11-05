#pragma once
#include "registry.h"
#include <atomic>
#include <chrono>

namespace pico::services {

struct RestartPolicy {
    unsigned maxRestarts = 3;
    std::chrono::milliseconds firstDelay{1000};
    std::chrono::milliseconds maxDelay{8000};
    std::chrono::milliseconds stablePeriod{30000};
};
struct SupervisorStatus {
    Instance current = 0;
    bool desired = false;
    unsigned restarts = 0;
    std::string state = "stopped";
    std::string error;
};

// Only the top-level Go APP service needs this HAP-side restart owner. The
// original Go APP supervision retains core/standalone policy via the Native
// client, rather than competing with a second native core restart loop.
// Tick must run in the HAP Native context, never inside an official child.
class AppSupervisor {
public:
    using Clock = std::function<std::chrono::steady_clock::time_point()>;
    AppSupervisor(Registry &registry, RestartPolicy policy = {}, Clock clock = std::chrono::steady_clock::now);
    Outcome Start(const std::string &payload);
    void RequestQuit(); // cancel intent, but do not bypass graceful drain
    void Tick();
    SupervisorStatus Snapshot() const;
private:
    Registry &registry_;
    RestartPolicy policy_;
    Clock clock_;
    mutable std::mutex mutex_;
    std::atomic<bool> desired_{false};
    SupervisorStatus status_;
    bool scheduled_ = false;
    std::chrono::steady_clock::time_point due_{}, began_{};
};
} // namespace pico::services
