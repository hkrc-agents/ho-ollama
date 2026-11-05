#pragma once
#include "registry.h"
#include <atomic>
#include <mutex>
#include <set>
#include <thread>

namespace pico::services {

// Private HAP control plane, NOT the public Ollama/OpenAI API. Each managed SO
// receives this per-session capability through its private native bootstrap.
// Ordinary CLI/API requests do not use it and gain no new authentication.
// The registry must outlive this server. No request invokes ArkTS or a shell.
class ControlServer {
public:
    ControlServer(Registry &registry, std::string capabilityHex);
    ~ControlServer();
    std::string Start(); // ephemeral IPv4 loopback address; throws on failure
    void Stop();
private:
    Registry &registry_;
    std::string capability_;
    std::atomic<bool> stopping_{false};
    int listener_ = -1;
    std::mutex socketsMutex_;
    std::set<int> sockets_;
    std::vector<std::thread> workers_;
    void Worker();
    void Handle(int fd);
};
} // namespace pico::services
