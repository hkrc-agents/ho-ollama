#ifndef PICO_STORAGE_PERMISSION_AUDIT_H
#define PICO_STORAGE_PERMISSION_AUDIT_H

#include <string>

// Caller supplies only the previously selected native URI. This diagnostic
// neither updates selection metadata nor qualifies filesystem/model access.
std::string AuditStoragePermission(const std::string &uri);

#endif
