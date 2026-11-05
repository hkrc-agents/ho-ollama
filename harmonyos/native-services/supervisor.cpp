#include "supervisor.h"
#include <algorithm>
#include <stdexcept>

namespace pico::services {
AppSupervisor::AppSupervisor(Registry &registry, RestartPolicy policy, Clock clock)
    : registry_(registry), policy_(policy), clock_(std::move(clock)) {
    if (!clock_ || !policy_.maxRestarts || policy_.firstDelay.count() <= 0 ||
        policy_.maxDelay < policy_.firstDelay || policy_.stablePeriod.count() <= 0)
        throw std::invalid_argument("invalid APP restart policy");
}
Outcome AppSupervisor::Start(const std::string &payload) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (status_.current && !registry_.Quiescent(status_.current)) return {false, 0, "previous APP tree is not quiescent"};
    desired_ = true;
    auto result = registry_.Start({Role::App, 0, payload});
    status_ = {result.instance, true, 0, result.ok ? "running" : "failed", result.error};
    began_ = clock_(); scheduled_ = false;
    if (!result.instance) { desired_ = false; status_.desired = false; }
    return result;
}
void AppSupervisor::RequestQuit() {
    // Publish cancellation even if another thread is currently in Native start.
    desired_ = false;
    std::lock_guard<std::mutex> lock(mutex_);
    status_.desired = false; scheduled_ = false;
    if (status_.current) registry_.RequestStop(status_.current);
    status_.state = "stopping";
}
void AppSupervisor::Tick() {
    std::lock_guard<std::mutex> lock(mutex_);
    registry_.Pump();
    if (!status_.current) return;
    Record current; bool drained = false, quiescent = false;
    if (!registry_.Inspect(status_.current, current, drained, &quiescent)) {
        status_.state = "failed"; status_.error = "APP instance disappeared from registry"; return;
    }
    if (!desired_) {
        status_.desired = false; scheduled_ = false;
        status_.state = quiescent ? "stopped" : "stopping";
        return;
    }
    if (!current.spec.desired) {
        desired_ = false; status_.desired = false; scheduled_ = false;
        status_.state = quiescent ? "stopped" : "stopping"; return;
    }
    if (!registry_.Healthy()) {
        status_.state = "failed"; status_.error = "lifecycle audit failure blocks APP restart"; return;
    }
    auto now = clock_();
    if (!current.exitObserved) {
        status_.state = StateName(current.state);
        if (current.state == State::Running && now - began_ >= policy_.stablePeriod) status_.restarts = 0;
        return;
    }
    if (!drained) { status_.state = "draining"; return; }
    if (status_.restarts >= policy_.maxRestarts) {
        status_.state = "failed"; status_.error = "APP crash restart budget exhausted"; return;
    }
    if (!scheduled_) {
        auto delay = policy_.firstDelay;
        for (unsigned n = 0; n < status_.restarts && delay < policy_.maxDelay; ++n)
            delay = delay > policy_.maxDelay - delay ? policy_.maxDelay : delay + delay;
        due_ = now + delay; scheduled_ = true; status_.state = "backoff"; return;
    }
    if (now < due_) return;
    auto replacement = registry_.Replace(status_.current);
    ++status_.restarts; scheduled_ = false;
    if (replacement.instance) status_.current = replacement.instance;
    status_.state = replacement.ok ? "running" : "failed";
    status_.error = replacement.error;
    began_ = now;
    if (!desired_) {
        // Quit raced the actual native creation. Do not resurrect intent.
        registry_.RequestStop(status_.current);
        status_.desired = false; status_.state = "stopping";
    }
}
SupervisorStatus AppSupervisor::Snapshot() const {
    std::lock_guard<std::mutex> lock(mutex_);
    auto result = status_; result.desired = desired_; return result;
}
} // namespace pico::services
