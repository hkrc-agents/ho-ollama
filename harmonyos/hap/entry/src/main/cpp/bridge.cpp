#include <napi/native_api.h>
#include <spawn.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <fcntl.h>
#include <signal.h>
#include <unistd.h>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <fstream>
#include <iterator>
#include <limits>
#include <mutex>
#include <sstream>
#include <string>
#include <thread>
#include <vector>
#ifdef PICO_ELF_SUPERVISION
#include "elf_supervision.h"
#include <map>
#ifndef PICO_ELF_SOURCE_SHA256
#define PICO_ELF_SOURCE_SHA256 "unqualified-direct-build"
#endif
std::map<pid_t,int> appLifelines; // lifecycleMutex protects all accesses.
bool appDrainBlocked=false;
bool appDesired=false;
std::string hapSession;
unsigned appRestarts=0;
uint64_t appIntentEpoch=0;
uint64_t appRunGeneration=0; // Explicit Stop/Start resets intent; crash recovery does not.
std::string appDirectory;
int appCorePort=0;
std::chrono::steady_clock::time_point appStarted;
#endif
#ifdef __OHOS__
#include "storage_permission_audit.h"
#endif

extern char **environ;
namespace {
std::string StartAppLocked(const std::string&,int);
#ifndef PICO_ELF_BINARY
#define PICO_ELF_BINARY "/data/service/hnp/ollama.org/ollama_1.0/bin/ollama"
#endif
#ifndef PICO_ELF_APP_BINARY
#define PICO_ELF_APP_BINARY "/data/service/hnp/ollama.org/ollama_1.0/bin/ollama-app"
#endif
constexpr const char *binary = PICO_ELF_BINARY;
constexpr const char *appBinary = PICO_ELF_APP_BINARY;
// Serializes spawn/identity publication/signalling with direct-child reaping.
std::mutex lifecycleMutex;
#ifdef __OHOS__
std::string tlsCaDirectory; // lifecycleMutex; selected by the HAP's public CertificateManager API.
bool ValidCaDirectory(const std::string &path) {
    if (path.empty() || path == "/" || path.front() != '/') return false;
    for (const unsigned char c : path) if (c < 32 || c == 127) return false;
    struct stat info{};
    return stat(path.c_str(), &info) == 0 && S_ISDIR(info.st_mode);
}
bool TlsEnvironmentName(const std::string &value) {
    return value.rfind("LLAMA_OHOS_CA_DIRECTORY=", 0) == 0;
}
#endif
struct ProcessIdentity {
    pid_t pid = -1;
    uint64_t startTicks = 0;
};
bool Unsigned(const std::string &text, uint64_t &value) {
    value = 0;
    if (text.empty()) return false;
    for (const char c : text) {
        if (c < '0' || c > '9' || value > (UINT64_MAX - (c - '0')) / 10) return false;
        value = value * 10 + (c - '0');
    }
    return true;
}
bool ReadIdentity(std::istream &in, ProcessIdentity &identity) {
    std::string pidText, ticksText, extra;
    uint64_t pid, ticks;
    if (!(in >> pidText >> ticksText) || (in >> extra) ||
        !Unsigned(pidText, pid) || pid <= 1 || pid > static_cast<uint64_t>(std::numeric_limits<pid_t>::max()) ||
        !Unsigned(ticksText, ticks) || ticks == 0) return false;
    identity = {static_cast<pid_t>(pid), ticks};
    return true;
}
bool ParseStartTicks(const std::string &text, pid_t pid, uint64_t &ticks) {
    // comm can contain spaces, newlines and ')' characters. Field 3 follows
    // the LAST closing parenthesis; starttime is field 22 (20 tokens later).
    const auto end = text.rfind(')');
    const auto begin = text.find('(');
    if (begin == std::string::npos || end == std::string::npos || end <= begin) return false;
    std::istringstream prefix(text.substr(0, begin));
    long parsedPid = -1; std::string extra;
    if (!(prefix >> parsedPid) || (prefix >> extra) || parsedPid != pid) return false;
    std::istringstream fields(text.substr(end + 1));
    std::string field;
    for (int i = 3; i <= 22; ++i) {
        if (!(fields >> field)) return false;
        if (i == 3 && (field.size() != 1 || field == "Z" || field == "X" || field == "x")) return false;
    }
    return Unsigned(field, ticks) && ticks != 0;
}
bool ReadProcFile(int proc, const char *name, std::string &text) {
    const int fd = openat(proc, name, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return false;
    text.clear();
    char buffer[4096];
    ssize_t n;
    while ((n = read(fd, buffer, sizeof(buffer))) != 0) {
        if (n < 0) { if (errno == EINTR) continue; close(fd); return false; }
        text.append(buffer, static_cast<size_t>(n));
        if (text.size() > 65536) { close(fd); return false; }
    }
    close(fd);
    return true;
}
bool Snapshot(pid_t pid, const char *executable, bool service, uint64_t &ticks) {
    if (pid <= 1) return false;
    const auto path = "/proc/" + std::to_string(pid);
    const int proc = open(path.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (proc < 0) return false;
    struct stat st{};
    std::string before, cmd, after;
    uint64_t last = 0;
    // All reads use the same proc directory, never a newly reused numeric PID.
    const bool valid = !fstat(proc, &st) && st.st_uid == getuid() && getpgid(pid) == pid &&
        ReadProcFile(proc, "stat", before) && ParseStartTicks(before, pid, ticks) &&
        ReadProcFile(proc, "cmdline", cmd) &&
        cmd.compare(0, std::string(executable).size() + 1, std::string(executable) + '\0') == 0 &&
        (!service || cmd.compare(std::string(executable).size() + 1, 6, std::string("serve\0", 6)) == 0) &&
        ReadProcFile(proc, "stat", after) && ParseStartTicks(after, pid, last) && ticks == last;
    close(proc);
    return valid;
}
pid_t OwnedProcess(const std::string &path, const char *executable, bool service,
                   ProcessIdentity *owned = nullptr) {
    std::ifstream record(path);
    ProcessIdentity identity;
    uint64_t ticks = 0;
    if (!ReadIdentity(record, identity) || !Snapshot(identity.pid, executable, service, ticks) ||
        ticks != identity.startTicks) return -1;
    if (owned) *owned = identity;
    return identity.pid;
}
// Caller holds lifecycleMutex. Only actual posix_spawn results reach this
// reaper: never wait for a PID read from disk, and never waitpid(-1).
void ReapChild(pid_t pid) {
    std::thread([pid]() {
        for (;;) {
            {
                std::lock_guard<std::mutex> lock(lifecycleMutex);
                int status = 0;
                const auto result = waitpid(pid, &status, WNOHANG);
#ifdef PICO_ELF_SUPERVISION
                auto app=appLifelines.find(pid);
                if(result==pid && app!=appLifelines.end()){
                    close(app->second);appLifelines.erase(app);
                    appDrainBlocked=!pico_elf::DrainAdopted();
                    if(appDesired&&!appDrainBlocked){
                        if(std::chrono::steady_clock::now()-appStarted>std::chrono::seconds(30))appRestarts=0;
                        if(appRestarts<3){
                            unsigned attempt=appRestarts++;
                            const auto directory=appDirectory;const int port=appCorePort;const auto epoch=appIntentEpoch;
                            std::thread([directory,port,attempt,epoch]() mutable {
                              for(;;){
                                std::this_thread::sleep_for(std::chrono::seconds(1U<<attempt));
                                std::lock_guard<std::mutex> lock(lifecycleMutex);
                                if(appDesired&&appIntentEpoch==epoch&&!appDrainBlocked&&appLifelines.empty()){
                                    const auto result=StartAppLocked(directory,port);
                                    std::ofstream out(directory+"/elf-recovery.log",std::ios::app);out<<result<<'\n';
                                    if(result.rfind("error:",0)!=0||appRestarts>=3)return;
                                    attempt=appRestarts++;
                                }else return;
                              }
                            }).detach();
                        }
                    }
                }
#endif
                if (result == pid || (result < 0 && errno != EINTR)) return;
            }
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
        }
    }).detach();
}
bool PersistChild(const std::string &path, pid_t pid, const char *executable, bool service) {
    uint64_t ticks = 0;
    bool saved = false;
    bool captured = false;
    // Immediately after spawn, procfs can still expose a transient exec view.
    // The unreaped child's PID remains reserved throughout this bounded retry.
    for (int attempt = 0; attempt < 10; ++attempt) {
        if (Snapshot(pid, executable, service, ticks)) { captured = true; break; }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    if (captured) {
        std::ofstream out(path);
        out << pid << ' ' << ticks << '\n';
        out.close(); // Check flush/close errors as well as insertion errors.
        saved = !out.fail();
    }
    // This is still our unreaped direct child, so its PID cannot be recycled.
    if (!saved) kill(-pid, SIGTERM);
    ReapChild(pid);
    return saved;
}
int StopOwned(const std::string &path, const char *executable, bool service, bool &stopping) {
    stopping = false;
    ProcessIdentity identity;
    const auto pid = OwnedProcess(path, executable, service, &identity);
    if (pid <= 0) return 0;
    // Revalidate immediately before signalling. Reaping our children is locked
    // out; adopted non-children still have the platform's check/kill race.
    uint64_t ticks = 0;
    if (!Snapshot(pid, executable, service, ticks) || ticks != identity.startTicks) return 0;
    if (kill(-pid, SIGTERM)) return errno == ESRCH ? 0 : errno;
    stopping = true;
    return 0;
}
std::string String(napi_env env, napi_value value) {
    size_t size = 0;
    if (napi_get_value_string_utf8(env, value, nullptr, 0, &size) != napi_ok || size > 8192)
        return {};
    std::vector<char> buf(size + 1);
    napi_get_value_string_utf8(env, value, buf.data(), buf.size(), &size);
    return std::string(buf.data(), size);
}
napi_value Result(napi_env env, const std::string &text) {
    napi_value value;
    napi_create_string_utf8(env, text.data(), text.size(), &value);
    return value;
}
bool ValidDir(const std::string &dir) {
    return dir.rfind("/data/storage/", 0) == 0 && dir.find("..") == std::string::npos;
}
pid_t OwnedService(const std::string &dir) {
    return OwnedProcess(dir + "/ollama-service.pid", binary, true);
}
int Spawn(const std::string &dir, const std::string &models, int port,
          std::vector<std::string> args, const std::string &log, pid_t &pid,
          const char *executable = binary) {
#ifdef PICO_ELF_SUPERVISION
    // No alternate direct-child creation while the APP owns this service tree.
    const bool isApp=std::string(executable)==appBinary;
    if(!isApp)return ENOTSUP;
    if(appDrainBlocked||!appLifelines.empty())return EBUSY;
    if(!pico_elf::EnableSubreaper())return EPERM;
    {std::ofstream identity(dir+"/elf-native-build.txt");identity<<PICO_ELF_SOURCE_SHA256<<'\n';identity.close();if(!identity)return EIO;}
    if(hapSession.empty()){
        unsigned char bytes[16];if(getentropy(bytes,sizeof(bytes)))return errno;
        constexpr char hex[]="0123456789abcdef";
        for(auto b:bytes){hapSession+=hex[b>>4];hapSession+=hex[b&15];}
    }
    std::vector<pid_t> before;
    if(!pico_elf::DirectChildren(before)||!before.empty())return EBUSY;
    int life[2];if(pipe2(life,O_CLOEXEC))return errno;
#endif
    int fd = open((dir + "/" + log).c_str(), O_CREAT | O_TRUNC | O_WRONLY | O_CLOEXEC, 0600);
    if (fd < 0) {
        int saved=errno;
#ifdef PICO_ELF_SUPERVISION
        close(life[0]);close(life[1]);
#endif
        return saved;
    }
    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
#ifdef PICO_ELF_SUPERVISION
    // Choose a destination distinct from both pipe ends and the output FD.
    int inherited=196;
    while(inherited==life[0]||inherited==life[1]||inherited==fd)++inherited;
    posix_spawn_file_actions_adddup2(&actions,life[0],inherited);
    posix_spawn_file_actions_addclose(&actions,life[0]);
    posix_spawn_file_actions_addclose(&actions,life[1]);
#endif
    posix_spawn_file_actions_addopen(&actions, STDIN_FILENO, "/dev/null", O_RDONLY, 0);
    posix_spawn_file_actions_adddup2(&actions, fd, STDOUT_FILENO);
    posix_spawn_file_actions_adddup2(&actions, fd, STDERR_FILENO);
    posix_spawn_file_actions_addclose(&actions, fd);
    posix_spawnattr_t attr;
    posix_spawnattr_init(&attr);
    posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETPGROUP);
    posix_spawnattr_setpgroup(&attr, 0);
    std::vector<std::string> vars;
    for (char **item = environ; *item; ++item) {
        std::string v = *item;
#ifdef __OHOS__
        if (!tlsCaDirectory.empty() && TlsEnvironmentName(v)) continue;
#endif
        if (v.rfind("OLLAMA_HOST=", 0) && v.rfind("OLLAMA_MODELS=", 0) &&
            v.rfind("OLLAMA_NUM_PARALLEL=", 0) && v.rfind("OLLAMA_CONTEXT_LENGTH=", 0) &&
            v.rfind("OLLAMA_MAX_LOADED_MODELS=", 0) && v.rfind("LD_LIBRARY_PATH=", 0) &&
            v.rfind("HOME=", 0) && v.rfind("PICO_PARENT_LIFELINE_FD=",0) && v.rfind("PICO_HAP_SESSION=",0)) vars.push_back(v);
    }
    vars.push_back("OLLAMA_HOST=127.0.0.1:" + std::to_string(port));
    vars.push_back("OLLAMA_MODELS=" + models);
    vars.push_back("OLLAMA_NUM_PARALLEL=1");
    vars.push_back("OLLAMA_CONTEXT_LENGTH=1024");
    vars.push_back("OLLAMA_MAX_LOADED_MODELS=1");
    vars.push_back("LD_LIBRARY_PATH="+std::string(appBinary).substr(0,std::string(appBinary).rfind("/bin/"))+"/lib");
    vars.push_back("HOME=" + dir);
#ifdef __OHOS__
    if (!tlsCaDirectory.empty()) vars.push_back("LLAMA_OHOS_CA_DIRECTORY=" + tlsCaDirectory);
#endif
#ifdef PICO_ELF_SUPERVISION
    vars.push_back("PICO_PARENT_LIFELINE_FD="+std::to_string(inherited));
    vars.push_back("PICO_HAP_SESSION="+hapSession+":"+std::to_string(appRunGeneration));
#endif
    std::vector<char *> env, argv;
    for (auto &v : vars) env.push_back(v.data());
    env.push_back(nullptr);
    args.insert(args.begin(), executable);
    for (auto &v : args) argv.push_back(v.data());
    argv.push_back(nullptr);
    int error = posix_spawn(&pid, executable, &actions, &attr, argv.data(), env.data());
    posix_spawnattr_destroy(&attr);
    posix_spawn_file_actions_destroy(&actions);
    close(fd);
#ifdef PICO_ELF_SUPERVISION
    close(life[0]);
    if(error)close(life[1]);else appLifelines.emplace(pid,life[1]);
#endif
    return error;
}
napi_value Start(napi_env env, napi_callback_info info) {
    size_t count = 3;
    napi_value args[3];
    napi_get_cb_info(env, info, &count, args, nullptr, nullptr);
    if (count != 3) return Result(env, "error: start arguments");
    auto dir = String(env, args[0]), models = String(env, args[1]);
    int32_t port = 0;
    napi_get_value_int32(env, args[2], &port);
    if (!ValidDir(dir) || !ValidDir(models) || port < 1024 || port > 65535)
        return Result(env, "error: invalid directory or port");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
    auto old = OwnedService(dir);
    if (old > 0) return Result(env, "running pid=" + std::to_string(old));
    pid_t pid = -1;
    int error = Spawn(dir, models, port, {"serve"}, "ollama-service.log", pid);
    if (error) return Result(env, "error: spawn errno=" + std::to_string(error));
    if (!PersistChild(dir + "/ollama-service.pid", pid, binary, true)) {
        return Result(env, "error: cannot persist service identity");
    }
    return Result(env, "starting pid=" + std::to_string(pid));
}
napi_value Stop(napi_env env, napi_callback_info info) {
    size_t count = 1;
    napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    auto dir = count ? String(env, arg) : "";
    if (!ValidDir(dir)) return Result(env, "error: directory");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
    bool stopping;
#ifdef PICO_ELF_SUPERVISION
    // The new host never creates core directly. Do not signal a legacy PID
    // sidecar from another host generation, even if its birth ticks match.
    return Result(env,OwnedService(dir)>0?"error: legacy service not owned by this host":"stopped");
#endif
    const int error = StopOwned(dir + "/ollama-service.pid", binary, true, stopping);
    if (error) return Result(env, "error: stop errno=" + std::to_string(error));
    return Result(env, stopping ? "stopping" : "stopped");
}
napi_value Status(napi_env env, napi_callback_info info) {
    size_t count = 1;
    napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    auto dir = count ? String(env, arg) : "";
    if (!ValidDir(dir)) return Result(env, "error: directory");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
    auto pid = OwnedService(dir);
    return Result(env, pid > 0 ? "running pid=" + std::to_string(pid) : "stopped");
}
napi_value Logs(napi_env env, napi_callback_info info) {
    size_t count = 1;
    napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    auto dir = count ? String(env, arg) : "";
    if (!ValidDir(dir)) return Result(env, "error: directory");
    std::ifstream in(dir + "/ollama-service.log", std::ios::binary);
    in.seekg(0, std::ios::end);
    std::streamoff size = in.tellg();
    in.seekg(size > 16384 ? size - std::streamoff(16384) : std::streamoff(0));
    return Result(env, std::string(std::istreambuf_iterator<char>(in), {}));
}
napi_value Command(napi_env env, napi_callback_info info) {
#if !defined(PICO_ENABLE_DEBUG_COMMANDS) || !PICO_ENABLE_DEBUG_COMMANDS
    (void)info;
    return Result(env, "error: diagnostic commands disabled in release");
#else
    size_t count = 3;
    napi_value args[3];
    napi_get_cb_info(env, info, &count, args, nullptr, nullptr);
    if (count != 3) return Result(env, "error: command arguments");
    auto dir = String(env, args[0]);
    int32_t port = 0;
    napi_get_value_int32(env, args[2], &port);
    uint32_t length = 0;
    napi_get_array_length(env, args[1], &length);
    if (!ValidDir(dir) || length < 1 || length > 32 || port < 1024 || port > 65535)
        return Result(env, "error: invalid command");
    std::vector<std::string> argv;
    for (uint32_t i = 0; i < length; ++i) {
        napi_value item;
        napi_get_element(env, args[1], i, &item);
        auto value = String(env, item);
        if (value.find('\0') != std::string::npos) return Result(env, "error: argument");
        argv.push_back(value);
    }
    // The upstream executable owns argument parsing. Service startup has a
    // separate lifecycle entry; diagnostic Wants never invoke a shell.
    const std::vector<std::string> allowed = {"--version", "--help", "list", "ps", "show", "run", "pull", "create", "cp", "rm", "stop"};
    bool accepted = false;
    for (const auto &value : allowed) if (argv[0] == value) accepted = true;
    if (!accepted)
        return Result(env, "error: unsupported diagnostic command");
    pid_t pid = -1;
    unlink((dir + "/ollama-command.exit").c_str());
    int error = Spawn(dir, dir + "/models", port, argv, "ollama-command.log", pid);
    if (!error) {
        std::ofstream out(dir + "/ollama-command.pid"); out << pid;
        std::thread([dir, pid]() {
            int status = 0;
            pid_t result;
            do { result = waitpid(pid, &status, 0); } while (result < 0 && errno == EINTR);
            const int code = result < 0 ? -1 : WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
            std::ofstream done(dir + "/ollama-command.exit"); done << code << '\n';
        }).detach();
    }
    return Result(env, "command errno=" + std::to_string(error) + " pid=" + std::to_string(pid));
#endif
}
pid_t OwnedApp(const std::string &dir) {
    return OwnedProcess(dir + "/ollama-app.pid", appBinary, false);
}
std::string StartAppLocked(const std::string& dir,int corePort) {
    const auto old = OwnedApp(dir);
#ifdef PICO_ELF_SUPERVISION
    if(old>0&&appLifelines.find(old)==appLifelines.end())return "error: APP belongs to an earlier host session";
    if(appDrainBlocked)return "error: previous APP tree drain incomplete";
#endif
    if (old > 0) return "running pid=" + std::to_string(old);
    // Create only the default private library. A saved custom model root is
    // still checked by the APP and never silently replaced with this path.
    if(mkdir((dir+"/models").c_str(),0700)&&errno!=EEXIST)return "error: private model directory";
    const auto data = dir + "/desktop";
    if (mkdir(data.c_str(), 0700) && errno != EEXIST) return "error: app data directory";
    unlink((data + "/app-token").c_str());
    if (unlink((data + "/shutdown-complete").c_str()) && errno != ENOENT)
        return "error: stale shutdown receipt";
    pid_t pid = -1;
#ifdef PICO_ELF_PROBE_ONLY
    const int port = corePort+1;
#else
    const int port = corePort == 11435 ? 11437 : 11435;
#endif
    const int error = Spawn(dir, dir + "/models", corePort,
        {"--listen", "127.0.0.1:" + std::to_string(port), "--data", data}, "ollama-app.log", pid, appBinary);
    if (error) return "error: app spawn errno=" + std::to_string(error);
    if (!PersistChild(dir + "/ollama-app.pid", pid, appBinary, false))
        return "error: app pid record";
#ifdef PICO_ELF_SUPERVISION
    appStarted=std::chrono::steady_clock::now();
#endif
    return "starting pid=" + std::to_string(pid);
}
std::string RequestAppStartLocked(const std::string& dir,int corePort) {
#ifdef PICO_ELF_SUPERVISION
    pico_elf::StartIntent(appDesired,appRunGeneration,appIntentEpoch);
    appRestarts=0;appDirectory=dir;appCorePort=corePort;
#endif
    return StartAppLocked(dir,corePort);
}
std::string RequestAppStopLocked(const std::string& dir) {
#ifdef PICO_ELF_SUPERVISION
    pico_elf::StopIntent(appDesired,appIntentEpoch);
    const auto current=OwnedApp(dir);
    if(current>0&&appLifelines.find(current)==appLifelines.end())return "error: APP not owned by current HAP session";
#endif
    bool stopping;
    if (StopOwned(dir + "/ollama-app.pid", appBinary, false, stopping)) return "error: app stop";
    return stopping ? "stopping" : "stopped";
}
napi_value AppStart(napi_env env, napi_callback_info info) {
#ifdef __OHOS__
    size_t count = 3; napi_value args[3];
#else
    size_t count = 2; napi_value args[2];
#endif
    napi_get_cb_info(env, info, &count, args, nullptr, nullptr);
#ifdef __OHOS__
    if (count != 2 && count != 3) return Result(env, "error: app arguments");
    const auto caDirectory = count == 3 ? String(env, args[2]) : std::string();
    if (!caDirectory.empty() && !ValidCaDirectory(caDirectory)) return Result(env, "error: system CA directory");
#else
    if (count != 2) return Result(env, "error: app arguments");
#endif
    const auto dir = String(env, args[0]);
    int32_t corePort = 0; napi_get_value_int32(env, args[1], &corePort);
    if (!ValidDir(dir) || corePort < 1024 || corePort > 65535) return Result(env, "error: app directory/port");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
#ifdef __OHOS__
    if (count == 3) tlsCaDirectory = caDirectory;
#endif
    return Result(env,RequestAppStartLocked(dir,corePort));
}
napi_value AppStop(napi_env env, napi_callback_info info) {
    size_t count = 1; napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    const auto dir = count ? String(env, arg) : "";
    if (!ValidDir(dir)) return Result(env, "error: app directory");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
    return Result(env,RequestAppStopLocked(dir));
}
napi_value AppToken(napi_env env, napi_callback_info info) {
    size_t count = 1; napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    const auto dir = count ? String(env, arg) : "";
#ifdef PICO_ELF_SUPERVISION
    std::unique_lock<std::mutex> lock(lifecycleMutex,std::try_to_lock);
    if(!lock.owns_lock())return Result(env,""); // Never stall the ArkUI observer during drain.
#else
    std::lock_guard<std::mutex> lock(lifecycleMutex);
#endif
    if (!ValidDir(dir) || OwnedApp(dir) <= 0) return Result(env, "");
#ifdef PICO_ELF_SUPERVISION
    if(appLifelines.find(OwnedApp(dir))==appLifelines.end())return Result(env,"");
#endif
    std::ifstream in(dir + "/desktop/app-token");
    std::string token; in >> token;
    if (token.size() != 64 || token.find_first_not_of("0123456789abcdef") != std::string::npos) return Result(env, "");
    return Result(env, token);
}
// A vanished token/failed ownership lookup is NOT proof that managed servers
// stopped. Require the Go managers' ACK for the exact native spawn identity,
// then observe APP proc disappearance (the direct-child reaper owns waitpid).
std::string AppShutdownState(const std::string &dir) {
#ifdef PICO_ELF_SUPERVISION
    if(appDrainBlocked||!appLifelines.empty())return "stopping";
#endif
    std::ifstream record(dir + "/ollama-app.pid");
    ProcessIdentity expected;
    if (!ReadIdentity(record, expected)) return "error: missing APP shutdown identity";
    struct stat st{};
    const auto proc = "/proc/" + std::to_string(expected.pid);
    if (!stat(proc.c_str(), &st)) return "stopping";
    if (errno != ENOENT) return "error: APP exit cannot be observed";
    std::ifstream receipt(dir + "/desktop/shutdown-complete");
    ProcessIdentity acknowledged;
    if (!ReadIdentity(receipt, acknowledged) || acknowledged.pid != expected.pid ||
        acknowledged.startTicks != expected.startTicks)
        return "error: APP exited without matching service shutdown acknowledgement";
    return "stopped";
}
napi_value AppShutdownStatus(napi_env env, napi_callback_info info) {
    size_t count = 1; napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    const auto dir = count ? String(env, arg) : "";
#ifdef PICO_ELF_SUPERVISION
    std::unique_lock<std::mutex> lock(lifecycleMutex,std::try_to_lock);
    if(!lock.owns_lock())return Result(env,"stopping");
#else
    std::lock_guard<std::mutex> lock(lifecycleMutex);
#endif
    return Result(env, ValidDir(dir) ? AppShutdownState(dir) : "error: app directory");
}
napi_value AppDebugCrash(napi_env env,napi_callback_info info) {
#if !PICO_ENABLE_DEBUG_COMMANDS || !defined(PICO_ELF_SUPERVISION)
    (void)info;
    return Result(env,"error: debug APP crash disabled");
#else
    size_t count=1;napi_value arg;napi_get_cb_info(env,info,&count,&arg,nullptr,nullptr);
    const auto dir=count?String(env,arg):"";
    if(!ValidDir(dir))return Result(env,"error: APP directory");
    std::lock_guard<std::mutex> lock(lifecycleMutex);
    const auto pid=OwnedApp(dir);
    if(pid<=1||appLifelines.find(pid)==appLifelines.end())return Result(env,"error: no current direct APP instance");
    if(kill(pid,SIGKILL))return Result(env,"error: APP fault injection denied");
    return Result(env,"debug APP crash requested");
#endif
}
napi_value StoragePermissionAudit(napi_env env, napi_callback_info info) {
    size_t count = 1; napi_value arg;
    napi_get_cb_info(env, info, &count, &arg, nullptr, nullptr);
    if (count != 1) return Result(env, "{\"error\":\"one selected URI required\"}");
#ifdef __OHOS__
    return Result(env, AuditStoragePermission(String(env, arg)));
#else
    return Result(env, "{\"available\":false,\"host_fixture\":true}");
#endif
}
napi_value Init(napi_env env, napi_value exports) {
    napi_property_descriptor props[] = {
        {"start", nullptr, Start, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"stop", nullptr, Stop, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"status", nullptr, Status, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"logs", nullptr, Logs, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"command", nullptr, Command, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"appStart", nullptr, AppStart, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"appStop", nullptr, AppStop, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"appToken", nullptr, AppToken, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"appShutdownStatus", nullptr, AppShutdownStatus, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"appDebugCrash", nullptr, AppDebugCrash, nullptr, nullptr, nullptr, napi_default, nullptr}
        ,{"storagePermissionAudit", nullptr, StoragePermissionAudit, nullptr, nullptr, nullptr, napi_default, nullptr}
    };
    napi_define_properties(env, exports, sizeof(props) / sizeof(props[0]), props);
    return exports;
}
}
#ifndef PICO_ELF_PROBE_ONLY
static napi_module module = {1, 0, nullptr, Init, "ollamabridge", nullptr, {0}};
extern "C" __attribute__((constructor)) void Register() { napi_module_register(&module); }
#endif
