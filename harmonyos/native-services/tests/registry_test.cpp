#include "registry.h"
#include "control_server.h"
#include "supervisor.h"
#include <arpa/inet.h>
#include <sys/socket.h>
#include <unistd.h>
#include <array>
#include <cassert>
#include <cstdio>
#include <functional>
#include <stdexcept>
#include <thread>

using namespace pico::services;

class Fake final : public Platform {
public:
    std::mutex mutex;
    Exit exit;
    int32_t next = 100;
    std::map<int32_t, uint64_t> live;
    std::vector<int32_t> kills;
    std::vector<std::string> audits;
    bool registration = true, logging = true, synchronousKill = true, early = false;
    int startCode = 0, killCode = 0;
    std::function<void()> duringStart;
    bool SetExitHandler(Exit handler) override {
        std::lock_guard<std::mutex> lock(mutex); exit = std::move(handler); return registration;
    }
    Started Start(const Record &) override {
        int32_t pid;
        {
            std::lock_guard<std::mutex> lock(mutex);
            if (startCode) return {startCode, -1, 0};
            pid = next++; live[pid] = static_cast<uint64_t>(pid) * 10;
        }
        if (duringStart) duringStart();
        if (early) { Emit(pid); return {0, pid, 0}; }
        return {0, pid, static_cast<uint64_t>(pid) * 10};
    }
    void Emit(int32_t pid, int32_t signal = 0) {
        Exit handler;
        { std::lock_guard<std::mutex> lock(mutex); live.erase(pid); handler = exit; }
        if (handler) handler(pid, signal);
    }
    void ChangeIdentity(int32_t pid) {
        std::lock_guard<std::mutex> lock(mutex); ++live.at(pid);
    }
    bool Matches(int32_t pid, uint64_t ticks) override {
        std::lock_guard<std::mutex> lock(mutex);
        auto found = live.find(pid); return found != live.end() && found->second == ticks;
    }
    ExitObservation ObserveExit(int32_t pid, uint64_t ticks) override {
        std::lock_guard<std::mutex> lock(mutex);
        auto found=live.find(pid);
        if(found==live.end())return ExitObservation::Gone;
        return found->second==ticks?ExitObservation::Alive:ExitObservation::Unknown;
    }
    void ForgetWithoutCallback(int32_t pid) {
        std::lock_guard<std::mutex> lock(mutex);live.erase(pid);
    }
    int Kill(int32_t pid) override {
        {
            std::lock_guard<std::mutex> lock(mutex);
            kills.push_back(pid);
        }
        if (killCode) return killCode;
        if (synchronousKill) Emit(pid, 9);
        return 0;
    }
    bool Audit(const char *event, const Record &record) override {
        std::lock_guard<std::mutex> lock(mutex);
        audits.push_back(AuditJSON(event, record)); return logging;
    }
};

struct Fixture {
    std::shared_ptr<Fake> fake = std::make_shared<Fake>();
    Registry registry{std::string(64, 'a'), fake};
    Instance app = 0, core = 0, runner = 0;
    Fixture() {
        auto a = registry.Start({Role::App, 0, "app"}); assert(a.ok); app = a.instance;
        auto c = registry.Start({Role::Core, app, "core"}); assert(c.ok); core = c.instance;
        auto r = registry.Start({Role::Runner, core, "runner"}); assert(r.ok); runner = r.instance;
    }
    Record record(Instance id) {
        for (auto record : registry.Snapshot()) if (record.instance == id) return record;
        throw std::runtime_error("missing test record");
    }
};

void InvalidSessionAndCallback() {
    bool threw = false;
    try { Registry invalid("old-session", std::make_shared<Fake>()); } catch (const std::invalid_argument &) { threw = true; }
    assert(threw);
    auto fake = std::make_shared<Fake>(); fake->registration = false; threw = false;
    try { Registry invalid(std::string(64, 'b'), fake); } catch (const std::runtime_error &) { threw = true; }
    assert(threw);
}
void ValidateLogicalOwnership() {
    Fixture f;
    assert(!f.registry.Start({Role::App, f.app, ""}).ok);
    assert(!f.registry.Start({Role::Core, 0, ""}).ok);
    assert(!f.registry.Start({Role::Runner, f.app, ""}).ok);
    assert(!f.registry.Start({Role::Core, 987654, ""}).ok);
    assert(!f.registry.Start({Role::Core, f.app, "duplicate"}).ok);
    assert(!f.registry.Start({Role::Core, f.app, std::string(20000, 'x')}).ok);
    assert(!f.registry.Start({Role::Runner, f.core, std::string("x\0y", 3)}).ok);
    assert(f.registry.Start({Role::Runner, f.core, "second model"}).ok);
}
void CoreCrashCascadesBeforeReplacement() {
    Fixture f;
    auto old = f.record(f.core), runner = f.record(f.runner);
    f.fake->Emit(old.pid, 11);
    assert(!f.registry.Drained(f.core));
    assert(!f.registry.Replace(f.core).ok);
    assert(!f.registry.Start({Role::Core, f.app, "replacement"}).ok);
    f.registry.Pump();
    assert(f.fake->kills.size() == 1 && f.fake->kills[0] == runner.pid);
    assert(f.registry.Drained(f.core));
    assert(f.record(f.core).spec.desired);
    assert(!f.record(f.runner).spec.desired);
    auto replacement = f.registry.Replace(f.core);
    assert(replacement.ok && replacement.instance != f.core);
    assert(f.record(replacement.instance).pid != old.pid);
    f.fake->Emit(f.record(replacement.instance).pid);
    assert(!f.registry.Replace(f.core).ok); // old generation cannot be replayed
}
void KillRequestIsNotExitAcknowledgement() {
    Fixture f; f.fake->synchronousKill = false;
    f.fake->Emit(f.record(f.core).pid);
    f.registry.Pump();
    assert(f.record(f.runner).killIssued);
    assert(!f.registry.Drained(f.core));
    assert(!f.registry.Replace(f.core).ok);
    f.fake->Emit(f.record(f.runner).pid, 9);
    assert(f.registry.Drained(f.core));
}
void QuitCancelsRestartAndUsesReverseDependencyOrder() {
    Fixture f;
    assert(f.registry.CancelIntent(f.app));
    assert(f.fake->kills.empty()); // normal drain intent is not a hard kill
    assert(f.registry.AbortTree(f.app));
    f.registry.Pump();
    assert(f.registry.Drained(f.app));
    assert(f.fake->kills.size() == 3);
    assert(f.fake->kills[0] == f.record(f.runner).pid);
    assert(f.fake->kills[1] == f.record(f.core).pid);
    assert(f.fake->kills[2] == f.record(f.app).pid);
    assert(!f.registry.Replace(f.app).ok);
}
void AppCrashCascadesWholeLogicalTree() {
    Fixture f;
    auto standalone = f.registry.Start({Role::Standalone, f.app, "standalone"}); assert(standalone.ok);
    f.fake->Emit(f.record(f.app).pid, 9);
    f.registry.Pump();
    assert(f.registry.Drained(f.app));
    assert(f.fake->kills.size() == 3);
    assert(!f.record(f.core).spec.desired && !f.record(standalone.instance).spec.desired);
    assert(!f.registry.Replace(f.core).ok);
    assert(f.registry.Replace(f.app).ok);
}
void IdentityLossBlocksCleanupAndReplacement() {
    Fixture f;
    f.fake->ChangeIdentity(f.record(f.runner).pid);
    f.fake->Emit(f.record(f.core).pid, 9);
    f.registry.Pump();
    assert(f.fake->kills.empty());
    assert(f.record(f.runner).state == State::Failed);
    assert(!f.registry.Drained(f.core) && !f.registry.Replace(f.core).ok);
}
void KillFailureIsNotExit() {
    Fixture f; f.fake->killCode = 16010003;
    f.fake->Emit(f.record(f.core).pid);
    f.registry.Pump();
    assert(f.record(f.runner).state == State::Failed);
    assert(!f.registry.Drained(f.core));
    assert(!f.registry.Replace(f.core).ok);
}
void OwnerDiesDuringRunnerStart() {
    Fixture f;
    f.fake->duringStart = [&]() { f.fake->Emit(f.record(f.core).pid); };
    auto r = f.registry.Start({Role::Runner, f.core, "racing runner"});
    assert(!r.ok);
    assert(f.record(r.instance).state == State::Stopping);
    assert(!f.record(r.instance).spec.desired);
    f.fake->duringStart = {};
    f.registry.Pump();
    assert(f.registry.Drained(f.core));
    assert(f.fake->kills.size() == 2);
}
void ImmediateExitCannotBeLost() {
    auto fake = std::make_shared<Fake>(); fake->early = true;
    Registry registry(std::string(64, 'a'), fake);
    auto start = registry.Start({Role::App, 0, "short lived"});
    assert(!start.ok && start.instance);
    assert(registry.Drained(start.instance));
    auto all = registry.Snapshot();
    assert(all.size() == 1 && all[0].exitObserved && all[0].state == State::Exited);
}
void OfficialStartFailureDoesNotSpawnFallback() {
    auto fake = std::make_shared<Fake>(); fake->startCode = 16010006;
    Registry registry(std::string(64, 'a'), fake);
    auto start = registry.Start({Role::App, 0, ""});
    assert(!start.ok && start.instance && fake->live.empty() && fake->kills.empty());
    assert(registry.Drained(start.instance));
}
void UnknownCallbackCannotAuthorizeSignals() {
    Fixture f;
    f.fake->Emit(9999999, 9);
    f.registry.Pump();
    assert(f.fake->kills.empty());
    assert(f.registry.Snapshot().size() == 3);
    assert(!f.registry.AbortTree(9999999));
}
void BrokenAuditBlocksNewCreation() {
    auto fake = std::make_shared<Fake>(); fake->logging = false;
    Registry registry(std::string(64, 'a'), fake);
    auto start = registry.Start({Role::App, 0, "secret"});
    assert(!start.ok && fake->live.empty() && !registry.Healthy());
    fake->logging = true;
    assert(!registry.Start({Role::App, 0, "again"}).ok);
    for (auto &line : fake->audits) assert(line.find("secret") == std::string::npos);
}
void ConcurrentDuplicateExitCallbacks() {
    Fixture f;
    int32_t pid = f.record(f.core).pid;
    std::vector<std::thread> threads;
    for (int i = 0; i < 32; ++i) threads.emplace_back([&]() { f.fake->Emit(pid, 9); });
    for (auto &thread : threads) thread.join();
    f.registry.Pump();
    assert(f.fake->kills.size() == 1 && f.registry.Drained(f.core));
}
void ControlPermissionsFollowLiveLogicalOwners() {
    Fixture f;
    assert(f.registry.CanManage(f.app,f.runner));
    assert(f.registry.CanManage(f.core,f.runner));
    assert(!f.registry.CanManage(f.runner,f.core));
    assert(!f.registry.CanManage(98765,f.app));
    assert(f.registry.CancelIntent(f.app));
    assert(f.registry.CanManage(f.app,f.runner)); // drain/status still allowed
    assert(!f.registry.Start({Role::Runner,f.core,"shutdown must not create"}).ok);
    f.fake->Emit(f.record(f.core).pid);
    assert(!f.registry.CanManage(f.core,f.runner));
    assert(f.registry.CanManage(f.app,f.core));
}
void StopIntentDuringCreationPreventsPublicationAsRunning() {
    auto fake=std::make_shared<Fake>();
    Registry registry(std::string(64,'b'),fake);
    fake->duringStart=[&](){assert(registry.CancelIntent(1));};
    auto result=registry.Start({Role::App,0,""});
    assert(!result.ok && result.instance==1);
    auto record=registry.Snapshot()[0];
    assert(record.state==State::Stopping && !record.spec.desired);
    fake->duringStart={};
    registry.Pump();
    assert(registry.Drained(result.instance));
}
std::pair<uint32_t,std::string> Request(const std::string &address, unsigned opcode, unsigned role,
                                      Instance caller, Instance target, const std::string &payload="", bool auth=true) {
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0);assert(fd>=0);
    timeval timeout{3,0};assert(!setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&timeout,sizeof(timeout)));
    sockaddr_in addr{};addr.sin_family=AF_INET;addr.sin_addr.s_addr=htonl(INADDR_LOOPBACK);
    addr.sin_port=htons(static_cast<uint16_t>(std::stoi(address.substr(address.find(':')+1))));
    assert(!connect(fd,reinterpret_cast<sockaddr *>(&addr),sizeof(addr)));
    std::string message(60,'\0');message.replace(0,4,"PNS1");message[4]=static_cast<char>(opcode);message[5]=static_cast<char>(role);
    for(int n=0;n<8;++n){message[8+n]=static_cast<char>(caller>>(56-8*n));message[16+n]=static_cast<char>(target>>(56-8*n));}
    uint32_t len=static_cast<uint32_t>(payload.size());
    for(int n=0;n<4;++n)message[24+n]=static_cast<char>(len>>(24-8*n));
    for(int n=0;n<32;++n)message[28+n]=static_cast<char>(auth?0xaa:0xab);
    message+=payload;
    size_t sent=0;while(sent<message.size()){ssize_t n=send(fd,message.data()+sent,message.size()-sent,MSG_NOSIGNAL);assert(n>0);sent+=static_cast<size_t>(n);}
    auto read=[&](char *bytes,size_t size){size_t done=0;while(done<size){ssize_t n=recv(fd,bytes+done,size-done,0);assert(n>0);done+=static_cast<size_t>(n);}};
    uint32_t header[3];read(reinterpret_cast<char *>(header),sizeof(header));assert(ntohl(header[0])==0x504e5231);
    auto size=ntohl(header[2]);assert(size<65536);
    std::string body(size,'\0');read(body.data(),body.size());close(fd);
    return {ntohl(header[1]),body};
}
void PrivateControlCreatesOnlyOwnedOfficialInstances() {
    auto fake=std::make_shared<Fake>();Registry registry(std::string(64,'b'),fake);
    assert(registry.Start({Role::App,0,"APP"}).ok);
    ControlServer server(registry,std::string(64,'a'));auto address=server.Start();
    assert(address.rfind("127.0.0.1:",0)==0);
    assert(Request(address,2,0,1,1,"",false).first==401);
    assert(Request(address,1,0,1,0,"APP cannot be created by child").first==403);
    assert(Request(address,1,2,1,0,"runner requires core owner").first==409);
    auto core=Request(address,1,1,1,0,"secret_env");assert(core.first==0);
    assert(core.second.find("\"instance\":2")!=std::string::npos);
    auto runner=Request(address,1,2,2,0,"model");assert(runner.first==0);
    assert(Request(address,2,0,2,1).first==403);
    auto status=Request(address,2,0,1,2);assert(status.first==0);
    assert(status.second.find("secret_env")==std::string::npos);
    assert(Request(address,99,0,1,1).first==400);
    assert(Request(address,4,0,1,1).first==0);
    assert(Request(address,2,0,1,3).first==0); // shutdown ACK polling remains possible
    assert(Request(address,1,2,2,0,"no starts during drain").first==409);
    assert(Request(address,3,0,1,1).first==0);
    // Synchronous fake exits; actual platform uses callbacks, covered on device.
    assert(registry.Drained(1));
    server.Stop();
}
void PartialControlClientDoesNotBlockStop() {
    auto fake=std::make_shared<Fake>();Registry registry(std::string(64,'b'),fake);
    ControlServer server(registry,std::string(64,'a'));auto address=server.Start();
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0);assert(fd>=0);
    sockaddr_in addr{};addr.sin_family=AF_INET;addr.sin_addr.s_addr=htonl(INADDR_LOOPBACK);
    addr.sin_port=htons(static_cast<uint16_t>(std::stoi(address.substr(address.find(':')+1))));
    assert(!connect(fd,reinterpret_cast<sockaddr *>(&addr),sizeof(addr)));
    assert(send(fd,"PNS",3,MSG_NOSIGNAL)==3);
    std::this_thread::sleep_for(std::chrono::milliseconds(30));
    server.Stop();close(fd);
}
void MissingExitCallbackRequiresIndependentAbsenceProof() {
    Fixture f;
    auto old=f.record(f.core);
    f.fake->ForgetWithoutCallback(old.pid);
    assert(!f.registry.Drained(f.core));
    f.registry.Pump();
    auto proof=f.record(f.core);
    assert(proof.exitObserved && proof.exitProof=="proc_absent" && !proof.exitSignalKnown);
    assert(f.registry.Drained(f.core));
    assert(f.registry.Replace(f.core).ok);
}
void DelayedPIDCallbackCannotRetireLiveGeneration() {
    Fixture f;
    // Callback injected without an exit: same PID and start ticks still live.
    Platform::Exit callback;
    {std::lock_guard<std::mutex> lock(f.fake->mutex);callback=f.fake->exit;}
    callback(f.record(f.core).pid,9);
    assert(!f.record(f.core).exitObserved);
    f.registry.Pump();
    assert(f.fake->kills.empty());
}
void CancelDuringReplacementCancelsNewPendingGeneration() {
    Fixture f;
    f.fake->Emit(f.record(f.core).pid);
    f.registry.Pump();
    f.fake->duringStart=[&](){assert(f.registry.CancelIntent(f.core));};
    auto replacement=f.registry.Replace(f.core);
    assert(!replacement.ok && replacement.instance);
    assert(!f.record(replacement.instance).spec.desired);
    assert(f.registry.Drained(f.core)); // old generation's tree is gone
    assert(!f.registry.Quiescent(f.core)); // replacement must ALSO be stopped
    f.fake->duringStart={};
    assert(f.registry.AbortTree(f.core));
    f.registry.Pump();
    assert(f.registry.Quiescent(f.core));
    assert(!f.registry.Replace(replacement.instance).ok);
}
void APPRecoveryBackoffAndQuitCancellation() {
    auto fake=std::make_shared<Fake>();Registry registry(std::string(64,'a'),fake);
    auto now=std::chrono::steady_clock::time_point{};
    RestartPolicy policy;policy.firstDelay=std::chrono::milliseconds(10);policy.maxDelay=std::chrono::milliseconds(20);
    AppSupervisor supervisor(registry,policy,[&](){return now;});
    auto app=supervisor.Start("APP");assert(app.ok);
    Record record;bool drained=false;assert(registry.Inspect(app.instance,record,drained));
    fake->Emit(record.pid,11);
    supervisor.Tick();assert(supervisor.Snapshot().state=="backoff");
    supervisor.Tick();assert(supervisor.Snapshot().current==app.instance);
    now+=std::chrono::milliseconds(10);supervisor.Tick();
    auto next=supervisor.Snapshot().current;assert(next!=app.instance && supervisor.Snapshot().restarts==1);
    assert(registry.Inspect(next,record,drained));fake->Emit(record.pid,11);supervisor.Tick();
    supervisor.RequestQuit();now+=std::chrono::seconds(10);supervisor.Tick();
    assert(supervisor.Snapshot().current==next && !supervisor.Snapshot().desired);
}
void APPRecoveryBudgetIsBounded() {
    auto fake=std::make_shared<Fake>();Registry registry(std::string(64,'a'),fake);
    auto now=std::chrono::steady_clock::time_point{};RestartPolicy policy;policy.maxRestarts=2;
    policy.firstDelay=std::chrono::milliseconds(1);policy.maxDelay=std::chrono::milliseconds(2);
    AppSupervisor supervisor(registry,policy,[&](){return now;});assert(supervisor.Start("APP").ok);
    for(int n=0;n<3;++n){
        Record record;bool drained=false;assert(registry.Inspect(supervisor.Snapshot().current,record,drained));
        fake->Emit(record.pid,9);supervisor.Tick();now+=std::chrono::seconds(1);supervisor.Tick();
    }
    assert(supervisor.Snapshot().restarts==2 && supervisor.Snapshot().state=="failed");
}
void GracefulStopDoesNotStopDescendantsBeforeDrain() {
    Fixture f;
    assert(f.registry.RequestStop(f.app));
    assert(f.record(f.app).stopRequested && !f.record(f.app).spec.desired);
    assert(!f.record(f.core).spec.desired && !f.record(f.runner).spec.desired);
    assert(!f.record(f.core).stopRequested && !f.record(f.runner).stopRequested);
    f.registry.Pump();assert(f.fake->kills.empty());
    // APP has drained its requests and explicitly asks core to drain next.
    assert(f.registry.RequestStop(f.core));
    assert(f.record(f.core).stopRequested && !f.record(f.runner).stopRequested);
    f.registry.Pump();assert(f.fake->kills.empty());
    assert(f.registry.RequestStop(f.runner));
    assert(f.record(f.runner).stopRequested);
}

int main() {
    const std::pair<const char *, void (*)()> tests[] = {
        {"invalid session/callback", InvalidSessionAndCallback},
        {"logical ownership", ValidateLogicalOwnership},
        {"core crash/drain/replacement", CoreCrashCascadesBeforeReplacement},
        {"kill request is not exit ACK", KillRequestIsNotExitAcknowledgement},
        {"quit order/no restart", QuitCancelsRestartAndUsesReverseDependencyOrder},
        {"APP crash whole logical tree", AppCrashCascadesWholeLogicalTree},
        {"identity loss", IdentityLossBlocksCleanupAndReplacement},
        {"official kill failure", KillFailureIsNotExit},
        {"owner/start race", OwnerDiesDuringRunnerStart},
        {"early exit/start race", ImmediateExitCannotBeLost},
        {"official start failure/no fallback", OfficialStartFailureDoesNotSpawnFallback},
        {"unknown callbacks", UnknownCallbackCannotAuthorizeSignals},
        {"audit failure", BrokenAuditBlocksNewCreation},
        {"concurrent duplicate callbacks", ConcurrentDuplicateExitCallbacks},
        {"control logical owner permissions", ControlPermissionsFollowLiveLogicalOwners},
        {"cancel during start", StopIntentDuringCreationPreventsPublicationAsRunning},
        {"private Native control plane", PrivateControlCreatesOnlyOwnedOfficialInstances},
        {"partial control client shutdown", PartialControlClientDoesNotBlockStop},
        {"missing callback/independent exit proof", MissingExitCallbackRequiresIndependentAbsenceProof},
        {"delayed callback while identity live", DelayedPIDCallbackCannotRetireLiveGeneration},
        {"stop during replacement covers new generation", CancelDuringReplacementCancelsNewPendingGeneration},
        {"APP recovery/backoff/Quit", APPRecoveryBackoffAndQuitCancellation},
        {"APP recovery budget", APPRecoveryBudgetIsBounded},
        {"graceful stop preserves descendant drain order", GracefulStopDoesNotStopDescendantsBeforeDrain},
    };
    for (auto test : tests) { test.second(); std::printf("PASS %s\n", test.first); }
    return 0;
}
