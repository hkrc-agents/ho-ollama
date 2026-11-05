#pragma once
#ifndef __OHOS__
#error "The official Native child backend must only be built for HarmonyOS"
#endif
#include "registry.h"

namespace pico::services {

// Entry points are supplied by trusted packaging, not by a remote request.
// Only one backend owns the official callback in a HAP process.
std::shared_ptr<Platform> CreateOHOSPlatform(const std::string &auditPath,
                                          const std::map<Role, std::string> &entries,
                                          std::function<std::string(const Record &)> bootstrap = {});
std::string NewSessionID();

} // namespace pico::services
