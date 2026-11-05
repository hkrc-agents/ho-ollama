#include "registry.h"
#include <algorithm>
#include <limits>
#include <sstream>
#include <stdexcept>
#include <utility>

namespace pico::services {
namespace {
std::string Quote(const std::string &text) {
    static const char hex[] = "0123456789abcdef";
    std::string result = "\"";
    for (unsigned char c : text) {
        if (c == '"' || c == '\\') { result += '\\'; result += static_cast<char>(c); }
        else if (c < 32) { result += "\\u00"; result += hex[c >> 4]; result += hex[c & 15]; }
        else result += static_cast<char>(c);
    }
    return result + '"';
}
bool ValidSession(const std::string &session) {
    return session.size() == 64 && std::all_of(session.begin(), session.end(), [](char c) {
        return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
    });
}
}

const char *RoleName(Role role) {
    switch (role) {
        case Role::App: return "app";
        case Role::Core: return "core";
        case Role::Runner: return "runner";
        case Role::Standalone: return "standalone";
        case Role::Worker: return "worker";
    }
    return "invalid";
}
const char *StateName(State state) {
    switch (state) {
        case State::Starting: return "starting";
        case State::Running: return "running";
        case State::Stopping: return "stopping";
        case State::Exited: return "exited";
        case State::Failed: return "failed";
    }
    return "invalid";
}
std::string Bootstrap(const Record &r) {
    return "{\"schema\":1,\"hap_session\":" + Quote(r.session) +
        ",\"instance\":" + std::to_string(r.instance) + ",\"owner\":" + std::to_string(r.spec.owner) +
        ",\"role\":" + Quote(RoleName(r.spec.role)) + ",\"payload\":" + Quote(r.spec.payload) + "}";
}
std::string AuditJSON(const char *event, const Record &r) {
    // Deliberately exclude payload: it can contain secrets/model paths/prompts.
    return "{\"event\":" + Quote(event) + ",\"hap_session\":" + Quote(r.session) +
        ",\"instance\":" + std::to_string(r.instance) + ",\"owner\":" + std::to_string(r.spec.owner) +
        ",\"role\":" + Quote(RoleName(r.spec.role)) + ",\"pid\":" + std::to_string(r.pid) +
        ",\"start_ticks\":" + std::to_string(r.startTicks) + ",\"desired_running\":" +
        (r.spec.desired ? "true" : "false") + ",\"state\":" + Quote(StateName(r.state)) +
        ",\"exit_observed\":" + (r.exitObserved ? "true" : "false") +
        ",\"official_created\":" + (r.officialCreated ? "true" : "false") +
        ",\"stop_requested\":" + (r.stopRequested ? "true" : "false") +
        ",\"replaced_by\":" + std::to_string(r.replacedBy) +
        ",\"exit_signal_known\":" + (r.exitSignalKnown ? "true" : "false") +
        ",\"exit_proof\":" + Quote(r.exitProof) +
        ",\"exit_signal\":" + std::to_string(r.exitSignal) + ",\"error\":" + Quote(r.error) + "}\n";
}

struct Registry::Data {
    mutable std::mutex mutex;
    std::string session;
    std::shared_ptr<Platform> platform;
    std::map<Instance, Record> records;
    std::map<int32_t, int32_t> earlyExit;
    Instance next = 1;
    bool startInFlight = false;
    bool healthy = true;

    bool Descendant(Instance child, Instance owner) const {
        // Iterative and bounded, even if a future programmer corrupts a link.
        for (size_t count = 0; child && count <= records.size(); ++count) {
            if (child == owner) return true;
            auto found = records.find(child);
            if (found == records.end()) return false;
            child = found->second.spec.owner;
        }
        return false;
    }
    bool Drained(Instance id) const {
        if (records.find(id) == records.end()) return false;
        for (const auto &[key, record] : records)
            if (Descendant(key, id) && !record.exitObserved) return false;
        return true;
    }
    std::vector<Instance> Family(Instance id) const {
        std::vector<Instance> family;
        for (size_t n = 0; id && n <= records.size(); ++n) {
            auto found = records.find(id);
            if (found == records.end()) break;
            family.push_back(id);
            id = found->second.replacedBy;
        }
        return family;
    }
    bool Quiescent(Instance id) const {
        auto family = Family(id);
        if (family.empty()) return false;
        for (auto item : family) if (!Drained(item)) return false;
        return true;
    }
    void Audit(const char *event, Record &record) {
        if (!platform->Audit(event, record)) {
            healthy = false;
            record.error = "native lifecycle audit failed";
        }
    }
    void CancelDescendants(Instance owner, bool includeOwner) {
        for (auto &[id, record] : records) {
            if ((includeOwner || id != owner) && Descendant(id, owner)) {
                record.spec.desired = false;
                if (!record.exitObserved) record.state = State::Stopping;
                Audit("intent_cancelled", record);
            }
        }
    }
    void ExitKnown(Record &record, int32_t signal, bool official = true) {
        if (record.exitObserved) return;
        record.exitObserved = true;
        record.exitSignal = signal;
        record.exitSignalKnown = official;
        record.exitProof = official ? "official_callback" : "proc_absent";
        record.state = State::Exited;
        Audit(official ? "official_exit" : "exit_confirmed_absent", record);
        // Keep the dead service's desired intent for Replace(), but NEVER
        // reuse its runners or restart descendants whose owner has died.
        CancelDescendants(record.instance, false);
    }
    void OnExit(int32_t pid, int32_t signal) {
        std::lock_guard<std::mutex> lock(mutex);
        if (pid <= 1) return;
        for (auto &[id, record] : records) {
            (void)id;
            if (record.pid == pid && !record.exitObserved) {
                if (record.startTicks && platform->ObserveExit(pid, record.startTicks) == ExitObservation::Alive) {
                    // An unversioned delayed PID callback must not retire a new
                    // generation which is demonstrably still alive.
                    Audit("exit_callback_identity_still_live", record);
                    return;
                }
                ExitKnown(record, signal); return;
            }
        }
        // An official child may exit before Start returns its PID. Only retain
        // events while our serialized start is in flight; never adopt strangers.
        if (startInFlight && earlyExit.size() < 64) earlyExit.emplace(pid, signal);
    }
};

Registry::Registry(std::string session, std::shared_ptr<Platform> platform) {
    if (!ValidSession(session) || !platform) throw std::invalid_argument("invalid session/platform");
    data_ = std::make_shared<Data>();
    data_->session = std::move(session); data_->platform = std::move(platform);
    std::weak_ptr<Data> weak = data_;
    if (!data_->platform->SetExitHandler([weak](int32_t pid, int32_t signal) {
        if (auto data = weak.lock()) data->OnExit(pid, signal);
    })) throw std::runtime_error("official exit callback unavailable");
}
Registry::~Registry() {
    // No PID scan, adoption, or implicit kill from a destructor. The HAP's
    // actual lifetime is the platform backstop; normal Quit must drain first.
    data_->platform->SetExitHandler({});
}
Outcome Registry::Start(const Spec &spec) {
    std::lock_guard<std::mutex> operation(operations_);
    return StartLocked(spec);
}
Outcome Registry::StartLocked(const Spec &spec, Instance predecessor) {
    auto d = data_;
    Instance instance;
    Record pending;
    {
        std::lock_guard<std::mutex> lock(d->mutex);
        if (!d->healthy) return {false, 0, "audit failure blocks new starts"};
        if (predecessor) {
            auto old = d->records.find(predecessor);
            if (old == d->records.end() || !old->second.spec.desired || old->second.replacedBy || !d->Drained(predecessor))
                return {false, 0, "replacement intent changed before creation"};
        }
        if (spec.payload.size() > 16384 || spec.payload.find('\0') != std::string::npos)
            return {false, 0, "invalid service payload"};
        if (d->records.size() >= 4096 || d->next == std::numeric_limits<Instance>::max())
            return {false, 0, "session registry capacity reached"};
        if (!spec.desired) return {false, 0, "service is not desired"};
        if (spec.role == Role::App) {
            if (spec.owner) return {false, 0, "APP must be owned by HAP session"};
        } else {
            auto owner = d->records.find(spec.owner);
            if (owner == d->records.end() || owner->second.state != State::Running || !owner->second.spec.desired)
                return {false, 0, "owner is not a live current-session instance"};
            Role expected = spec.role == Role::Runner ? Role::Core : Role::App;
            if (spec.role != Role::Worker && owner->second.spec.role != expected)
                return {false, 0, "invalid logical service owner"};
        }
        if (std::string(RoleName(spec.role)) == "invalid") return {false, 0, "invalid role"};
        // APP/core/standalone are unique per owner. Replacement cannot overlap
        // any remaining descendant of the previous instance.
        if (spec.role == Role::App || spec.role == Role::Core || spec.role == Role::Standalone)
            for (const auto &[id, r] : d->records)
                if (r.spec.role == spec.role && r.spec.owner == spec.owner && !d->Drained(id))
                    return {false, 0, "previous service tree has not drained"};
        instance = d->next++;
        pending.session = d->session; pending.instance = instance; pending.spec = spec;
        d->records.emplace(instance, pending);
        if (predecessor) {
            // Publish the replacement link BEFORE calling the platform. A stop
            // racing native creation can now cancel the new pending generation.
            auto &old = d->records.at(predecessor);
            old.replacedBy = instance;
            d->Audit("replacement_reserved", old);
        }
        auto &record = d->records.at(instance);
        d->Audit("start_requested", record);
        if (!d->healthy) { record.state = State::Failed; record.exitObserved = true; return {false, instance, record.error}; }
        d->startInFlight = true;
    }
    // Never hold the registry lock across an API which can call us back.
    Started started = d->platform->Start(pending);
    std::lock_guard<std::mutex> lock(d->mutex);
    d->startInFlight = false;
    auto &record = d->records.at(instance);
    if (started.code != 0 || started.pid <= 1) {
        record.pid = started.pid;
        record.startTicks = started.startTicks;
        record.state = State::Failed;
        // A nonzero return plus a positive PID is an ambiguous creation, not
        // permission to forget or signal that PID. Block replacement visibly.
        record.exitObserved = started.pid <= 1;
        record.error = "official start failed: " + std::to_string(started.code);
        d->Audit("start_failed", record);
        d->earlyExit.clear();
        return {false, instance, record.error};
    }
    record.pid = started.pid; record.startTicks = started.startTicks; record.officialCreated = true;
    auto early = d->earlyExit.find(started.pid);
    if (early != d->earlyExit.end()) d->ExitKnown(record, early->second);
    else if (started.callbackCode != 0) {
        record.state = State::Failed;
        record.error = "official exit callback rearm failed: " + std::to_string(started.callbackCode);
    }
    else if (!started.startTicks) {
        record.state = State::Failed; record.error = "official child identity unavailable";
    } else if (record.state != State::Stopping) record.state = State::Running;
    if (!record.spec.desired && !record.exitObserved) record.state = State::Stopping;
    d->earlyExit.clear();
    // Owner can die while Start is in flight; the just-created child must join
    // cleanup, not become a live unowned runner.
    if (spec.owner) {
        const auto &owner = d->records.at(spec.owner);
        if (owner.exitObserved || !owner.spec.desired) {
            record.spec.desired = false;
            if (!record.exitObserved) record.state = State::Stopping;
        }
    }
    d->Audit("start_result", record);
    return {record.error.empty() && record.state == State::Running, instance, record.error};
}
Outcome Registry::Replace(Instance previous) {
    std::lock_guard<std::mutex> operation(operations_);
    Spec spec;
    {
        std::lock_guard<std::mutex> lock(data_->mutex);
        auto old = data_->records.find(previous);
        if (old == data_->records.end()) return {false, 0, "unknown service instance"};
        if (!old->second.spec.desired) return {false, 0, "restart intent cancelled"};
        if (old->second.replacedBy) return {false, 0, "replacement already issued for this generation"};
        if (!data_->Drained(previous)) return {false, 0, "old service tree has not drained"};
        spec = old->second.spec;
    }
    return StartLocked(spec, previous);
}
bool Registry::CancelIntent(Instance instance) {
    std::lock_guard<std::mutex> lock(data_->mutex);
    if (data_->records.find(instance) == data_->records.end()) return false;
    // Do not force-kill as a side effect of cancelling a normal shutdown intent.
    for (auto root : data_->Family(instance))
        for (auto &[id, record] : data_->records) if (data_->Descendant(id, root)) {
            record.spec.desired = false;
            data_->Audit("intent_cancelled", record);
        }
    return true;
}
bool Registry::AbortTree(Instance instance) {
    std::lock_guard<std::mutex> lock(data_->mutex);
    if (data_->records.find(instance) == data_->records.end()) return false;
    for (auto root : data_->Family(instance)) data_->CancelDescendants(root, true);
    return true;
}
bool Registry::RequestStop(Instance instance) {
    std::lock_guard<std::mutex> lock(data_->mutex);
    if (data_->records.find(instance) == data_->records.end()) return false;
    // Freeze retries across the family, but ask only its root generation(s)
    // to drain. Children remain running until their own owner drains/stops them.
    for (auto root : data_->Family(instance)) {
        for (auto &[id, record] : data_->records) if (data_->Descendant(id, root)) {
            record.spec.desired = false;
            if (id == root) record.stopRequested = true;
            data_->Audit("cooperative_stop_requested", record);
        }
    }
    return true;
}
void Registry::Pump() {
    std::lock_guard<std::mutex> operation(operations_);
    std::vector<Record> observe;
    {
        std::lock_guard<std::mutex> lock(data_->mutex);
        for (const auto &[id, record] : data_->records) {
            (void)id;
            if (record.officialCreated && !record.exitObserved && record.pid > 1) observe.push_back(record);
        }
    }
    for (const auto &record : observe) {
        if (data_->platform->ObserveExit(record.pid, record.startTicks) != ExitObservation::Gone) continue;
        std::lock_guard<std::mutex> lock(data_->mutex);
        auto &live = data_->records.at(record.instance);
        if (!live.exitObserved) data_->ExitKnown(live, 0, false);
    }
    for (;;) {
        Record selected;
        bool found = false;
        {
            std::lock_guard<std::mutex> lock(data_->mutex);
            for (const auto &[id, record] : data_->records) {
                if (record.state != State::Stopping || record.exitObserved || record.killIssued || !record.officialCreated || record.pid <= 1) continue;
                bool childrenGone = true;
                for (const auto &[child, item] : data_->records)
                    if (child != id && data_->Descendant(child, id) && !item.exitObserved) childrenGone = false;
                if (!childrenGone) continue;
                selected = record; found = true; break;
            }
        }
        if (!found) return;
        const bool matches = selected.startTicks && data_->platform->Matches(selected.pid, selected.startTicks);
        int code = matches ? data_->platform->Kill(selected.pid) : -1;
        std::lock_guard<std::mutex> lock(data_->mutex);
        auto &record = data_->records.at(selected.instance);
        // A synchronous exit callback may already have supplied the proof.
        if (record.exitObserved) continue;
        if (!matches || code != 0) {
            record.state = State::Failed;
            record.error = !matches ? "child identity lost; cleanup unresolved" : "official kill failed: " + std::to_string(code);
            data_->Audit("cleanup_failed", record);
        } else {
            record.killIssued = true;
            data_->Audit("kill_requested", record);
        }
        // Successful Kill is only a request. Wait for official exit callback.
    }
}
bool Registry::Drained(Instance instance) const {
    std::lock_guard<std::mutex> lock(data_->mutex); return data_->Drained(instance);
}
bool Registry::Quiescent(Instance instance) const {
    std::lock_guard<std::mutex> lock(data_->mutex); return data_->Quiescent(instance);
}
std::vector<Record> Registry::Snapshot() const {
    std::lock_guard<std::mutex> lock(data_->mutex);
    std::vector<Record> out;
    for (const auto &[id, record] : data_->records) { (void)id; out.push_back(record); }
    return out;
}
bool Registry::Healthy() const {
    std::lock_guard<std::mutex> lock(data_->mutex); return data_->healthy;
}
bool Registry::CanManage(Instance caller, Instance target) const {
    std::lock_guard<std::mutex> lock(data_->mutex);
    auto found = data_->records.find(caller);
    return found != data_->records.end() && !found->second.exitObserved && found->second.officialCreated &&
        (found->second.state == State::Running || found->second.state == State::Stopping) &&
        data_->records.find(target) != data_->records.end() && data_->Descendant(target, caller);
}
bool Registry::Inspect(Instance instance, Record &record, bool &drained, bool *quiescent) const {
    std::lock_guard<std::mutex> lock(data_->mutex);
    auto found = data_->records.find(instance);
    if (found == data_->records.end()) return false;
    record = found->second;
    drained = data_->Drained(instance);
    if (quiescent) *quiescent = data_->Quiescent(instance);
    return true;
}
} // namespace pico::services
