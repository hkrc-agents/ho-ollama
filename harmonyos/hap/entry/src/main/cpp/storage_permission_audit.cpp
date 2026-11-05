#include "storage_permission_audit.h"

#include <filemanagement/fileshare/oh_file_share.h>
#include <accesstoken/ability_access_control.h>
#include <dlfcn.h>
#include <fcntl.h>
#include <unistd.h>

#include <cerrno>
#include <cstdlib>
#include <cstring>
#include <sstream>
#include <vector>

namespace {
constexpr size_t kMaxJSON = 32768;
constexpr size_t kTextLimit = 192;
constexpr unsigned int kMaxPolicyErrors = 16;
constexpr unsigned int kMode = READ_MODE | WRITE_MODE;
static_assert(kMode == 3, "Public NDK read/write mode changed");

std::string Quote(const char *text, size_t limit = kTextLimit)
{
    std::string out = "\"";
    constexpr char hex[] = "0123456789abcdef";
    if (text) {
        const size_t size = strnlen(text, limit);
        for (size_t i = 0; i < size; ++i) {
            const auto c = static_cast<unsigned char>(text[i]);
            if (c == '"' || c == '\\') { out += '\\'; out += static_cast<char>(c); }
            else if (c < 0x20 || c >= 0x7f) {
                out += "\\u00"; out += hex[c >> 4]; out += hex[c & 15];
            } else { out += static_cast<char>(c); }
        }
    }
    out += '"';
    return out;
}

struct Library {
    void *handle = nullptr;
    std::string error;
    explicit Library(const char *name)
    {
        handle = dlopen(name, RTLD_NOW | RTLD_LOCAL);
        if (!handle) { const char *e = dlerror(); error = e ? std::string(e, strnlen(e, kTextLimit)) : "dlopen failed"; }
    }
    ~Library() { if (handle) dlclose(handle); }
    Library(const Library &) = delete;
    Library &operator=(const Library &) = delete;
    void *Find(const char *name) const { return handle ? dlsym(handle, name) : nullptr; }
};

void Provenance(std::ostream &out, const char *name, void *symbol)
{
    Dl_info info{};
    const bool located = symbol && dladdr(symbol, &info) != 0;
    out << Quote(name) << ":{\"available\":" << (symbol ? 1 : 0)
        << ",\"dladdr_ok\":" << (located ? 1 : 0)
        << ",\"library\":" << Quote(located ? info.dli_fname : nullptr)
        << ",\"symbol\":" << Quote(located ? info.dli_sname : nullptr) << '}';
}

struct CheckResult {
    int called = 0;
    int rc = -1;
    unsigned int count = 0;
    int pointerPresent = 0;
    int value = -1;
    bool CheckedTrue() const { return called && rc == 0 && count == 1 && pointerPresent && value == 1; }
};

CheckResult Check(decltype(&OH_FileShare_CheckPersistentPermission) fn, const FileShare_PolicyInfo &policy)
{
    CheckResult result;
    bool *values = nullptr;
    result.called = 1;
    result.rc = static_cast<int>(fn(&policy, 1, &values, &result.count));
    result.pointerPresent = values ? 1 : 0;
    // An unexpected count is evidence of a contract failure, not permission.
    if (result.rc == 0 && result.count == 1 && values) result.value = values[0] ? 1 : 0;
    // Installed public SDK explicitly specifies free(); never delete[].
    std::free(values);
    return result;
}

void EmitCheck(std::ostream &out, const char *name, const CheckResult &r)
{
    out << Quote(name) << ":{\"called\":" << r.called << ",\"rc\":" << r.rc
        << ",\"input_count\":1,\"result_count\":" << r.count
        << ",\"pointer_present\":" << r.pointerPresent << ",\"value\":" << r.value
        << ",\"shape_ok\":" << (r.called && r.count == 1 && r.pointerPresent ? 1 : 0) << '}';
}

struct PolicyResult {
    int called = 0;
    int rc = -1;
    unsigned int count = 0;
    int pointerPresent = 0;
    int released = 0;
    std::vector<int> codes;
};

PolicyResult Apply(decltype(&OH_FileShare_PersistPermission) fn,
                   decltype(&OH_FileShare_ReleasePolicyErrorResult) release,
                   const FileShare_PolicyInfo &policy)
{
    PolicyResult result;
    FileShare_PolicyErrorResult *errors = nullptr;
    result.called = 1;
    result.rc = static_cast<int>(fn(&policy, 1, &errors, &result.count));
    result.pointerPresent = errors ? 1 : 0;
    // One input policy should yield at most one error. Bound inspection even
    // if a system component reports an unexpected count; expose that count.
    if (errors && result.count <= kMaxPolicyErrors) {
        for (unsigned int i = 0; i < result.count; ++i) result.codes.push_back(static_cast<int>(errors[i].code));
    }
    if (errors) { release(errors, result.count); result.released = 1; }
    return result;
}

void EmitPolicy(std::ostream &out, const char *name, const PolicyResult &r)
{
    out << Quote(name) << ":{\"called\":" << r.called << ",\"rc\":" << r.rc
        << ",\"input_count\":1,\"result_count\":" << r.count
        << ",\"pointer_present\":" << r.pointerPresent << ",\"released\":" << r.released
        << ",\"shape_ok\":" << (r.called && r.count <= 1 && (r.count == 0 || r.pointerPresent) ? 1 : 0)
        << ",\"error_codes\":[";
    for (size_t i = 0; i < r.codes.size(); ++i) { if (i) out << ','; out << r.codes[i]; }
    out << "],\"codes_truncated\":" << (r.count > kMaxPolicyErrors ? 1 : 0) << '}';
}

bool ValidURI(const std::string &uri)
{
    if (uri.size() <= 7 || uri.size() > 8192 || uri.compare(0, 7, "file://") != 0) return false;
    for (const unsigned char c : uri) if (c < 0x20 || c == 0x7f) return false;
    return true;
}
} // namespace

std::string AuditStoragePermission(const std::string &uri)
{
    std::ostringstream out;
    char domain[kTextLimit + 1]{};
    int domainError = 0;
    int domainTruncated = 0;
    const int fd = open("/proc/self/attr/current", O_RDONLY | O_CLOEXEC);
    if (fd < 0) { domainError = errno; }
    else {
        ssize_t count;
        do { count = read(fd, domain, kTextLimit); } while (count < 0 && errno == EINTR);
        if (count < 0) domainError = errno;
        else {
            domainTruncated = count == static_cast<ssize_t>(kTextLimit) ? 1 : 0;
            while (count > 0 && (domain[count - 1] == '\n' || domain[count - 1] == '\r' || domain[count - 1] == '\0')) domain[--count] = '\0';
        }
        close(fd);
    }
    const bool valid = ValidURI(uri);
    out << "{\"schema\":1,\"mode\":" << kMode << ",\"uid\":" << getuid() << ",\"pid\":" << getpid()
        << ",\"domain\":" << Quote(domain) << ",\"domain_errno\":" << domainError
        << ",\"domain_truncated\":" << domainTruncated << ",\"uri_valid\":" << (valid ? 1 : 0)
        << ",\"uri_length\":" << uri.size();
    if (!valid) { out << ",\"audit_complete\":0,\"input_error\":401}"; return out.str(); }

    Library share("libohfileshare.so");
    Library access("libability_access_control.so");
    void *checkSymbol = share.Find("OH_FileShare_CheckPersistentPermission");
    void *persistSymbol = share.Find("OH_FileShare_PersistPermission");
    void *activateSymbol = share.Find("OH_FileShare_ActivatePermission");
    void *releaseSymbol = share.Find("OH_FileShare_ReleasePolicyErrorResult");
    void *selfSymbol = access.Find("OH_AT_CheckSelfPermission");
    out << ",\"libraries\":{\"fileshare_loaded\":" << (share.handle ? 1 : 0)
        << ",\"fileshare_error\":" << Quote(share.error.c_str())
        << ",\"access_loaded\":" << (access.handle ? 1 : 0)
        << ",\"access_error\":" << Quote(access.error.c_str()) << "},\"symbols\":{";
    Provenance(out, "check", checkSymbol); out << ',';
    Provenance(out, "persist", persistSymbol); out << ',';
    Provenance(out, "activate", activateSymbol); out << ',';
    Provenance(out, "release", releaseSymbol); out << ',';
    Provenance(out, "self_permission", selfSymbol); out << '}';
    const auto self = reinterpret_cast<decltype(&OH_AT_CheckSelfPermission)>(selfSymbol);
    out << ",\"self_permission\":{\"called\":" << (self ? 1 : 0)
        << ",\"granted\":" << (self ? (self("ohos.permission.FILE_ACCESS_PERSIST") ? 1 : 0) : -1) << '}';

    CheckResult before, after, final;
    PolicyResult persist, activate;
    const bool available = checkSymbol && persistSymbol && activateSymbol && releaseSymbol;
    if (available) {
        const auto check = reinterpret_cast<decltype(&OH_FileShare_CheckPersistentPermission)>(checkSymbol);
        const auto persistFn = reinterpret_cast<decltype(&OH_FileShare_PersistPermission)>(persistSymbol);
        const auto activateFn = reinterpret_cast<decltype(&OH_FileShare_ActivatePermission)>(activateSymbol);
        const auto release = reinterpret_cast<decltype(&OH_FileShare_ReleasePolicyErrorResult)>(releaseSymbol);
        // Own mutable input storage matching the public char* contract. No URI
        // or policy-error messages are copied to diagnostic output.
        std::vector<char> text(uri.begin(), uri.end()); text.push_back('\0');
        FileShare_PolicyInfo policy{text.data(), static_cast<unsigned int>(uri.size()), kMode};
        before = Check(check, policy);
        persist = Apply(persistFn, release, policy);
        after = Check(check, policy);
        if (after.CheckedTrue()) activate = Apply(activateFn, release, policy);
        final = Check(check, policy);
    }
    out << ','; EmitCheck(out, "check_before", before);
    out << ','; EmitPolicy(out, "persist", persist);
    out << ','; EmitCheck(out, "check_after", after);
    out << ",\"activation_eligible\":" << (after.CheckedTrue() ? 1 : 0) << ',';
    EmitPolicy(out, "activate", activate);
    out << ','; EmitCheck(out, "check_final", final);
    // Complete means the available audit sequence ran, NOT that persistence,
    // activation or actual filesystem access passed. Preserve caller metadata.
    out << ",\"audit_complete\":" << (available ? 1 : 0) << '}';
    const std::string result = out.str();
    return result.size() <= kMaxJSON ? result : "{\"schema\":1,\"audit_complete\":0,\"output_limit\":1}";
}
