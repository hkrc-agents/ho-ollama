#pragma once

#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <vector>

namespace pico::services {

using Instance = uint64_t;
enum class Role { App, Core, Runner, Standalone, Worker };
enum class State { Starting, Running, Stopping, Exited, Failed };
enum class ExitObservation { Alive, Gone, Unknown };

struct Spec {
    Role role = Role::App;
    Instance owner = 0;
    std::string payload;
    bool desired = true;
};

struct Record {
    std::string session;
    Instance instance = 0;
    Spec spec;
    int32_t pid = -1;
    uint64_t startTicks = 0;
    State state = State::Starting;
    bool exitObserved = false;
    int32_t exitSignal = 0; // Official callback signal, NOT a program exit code.
    bool exitSignalKnown = false;
    std::string exitProof;
    bool killIssued = false;
    bool officialCreated = false;
    bool stopRequested = false; // Cooperative stop for THIS instance only.
    Instance replacedBy = 0;
    std::string error;
};

struct Started {
    int code = -1;
    int32_t pid = -1;
    uint64_t startTicks = 0;
    int callbackCode = 0;
};

// Only the OHOS implementation starts processes. Host tests inject a model of
// the official API, not a fork/exec fallback. All callbacks may race Start().
class Platform {
public:
    using Exit = std::function<void(int32_t, int32_t)>;
    virtual ~Platform() = default;
    virtual bool SetExitHandler(Exit handler) = 0;
    virtual Started Start(const Record &record) = 0;
    virtual bool Matches(int32_t pid, uint64_t ticks) = 0;
    // Only a positive disappearance proof may return Gone. Permission errors,
    // unreadable procfs, and mismatched/reused identities must return Unknown.
    virtual ExitObservation ObserveExit(int32_t, uint64_t) { return ExitObservation::Unknown; }
    virtual int Kill(int32_t pid) = 0;
    virtual bool Audit(const char *event, const Record &record) = 0;
};

struct Outcome {
    bool ok = false;
    Instance instance = 0;
    std::string error;
};

// One registry per HAP session. No records are restored from a previous
// session; no API accepts caller-supplied PIDs as authority to kill.
class Registry {
public:
    Registry(std::string session, std::shared_ptr<Platform> platform);
    ~Registry();
    Registry(const Registry &) = delete;
    Registry &operator=(const Registry &) = delete;

    Outcome Start(const Spec &spec);
    Outcome Replace(Instance previous);
    // Intent only: normal shutdown must still get the service's drain ACK.
    bool CancelIntent(Instance instance);
    bool RequestStop(Instance instance);
    // Crash cleanup / explicit abort. Official Kill is not a graceful ACK.
    bool AbortTree(Instance instance);
    // Run pending official termination calls; never invoked in exit callback.
    void Pump();
    bool Drained(Instance instance) const;
    bool Quiescent(Instance instance) const; // includes replacement generations
    std::vector<Record> Snapshot() const;
    bool Healthy() const;
    bool CanManage(Instance caller, Instance target) const;
    bool Inspect(Instance instance, Record &record, bool &drained, bool *quiescent = nullptr) const;

private:
    struct Data;
    std::shared_ptr<Data> data_;
    std::mutex operations_;
    Outcome StartLocked(const Spec &spec, Instance predecessor = 0);
};

const char *RoleName(Role role);
const char *StateName(State state);
std::string Bootstrap(const Record &record);
std::string AuditJSON(const char *event, const Record &record);

} // namespace pico::services
