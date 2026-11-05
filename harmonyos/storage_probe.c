/* Private E4 probe ELF. fd 3 is an exclusively-created probe directory passed
 * by the APP, never an arbitrary command-line path. No model settings/data. */
#define _GNU_SOURCE
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/types.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#ifdef PICO_NATIVE_MANAGED_PROBE
static int probe_directory_fd = -1;
void PicoStorageProbeSetDirectoryFd(int fd) { probe_directory_fd = fd; }
#define PROBE_DIRECTORY_FD probe_directory_fd
#else
#define PROBE_DIRECTORY_FD 3
#endif

struct operation { const char *name; int error; int optional; };
static struct operation ops[32];
static int count;
static void record(const char *name, int error, int optional) {
    if (count < 32) ops[count++] = (struct operation){name, error, optional};
}
static int unsupported(int e) { return e == ENOSYS || e == EOPNOTSUPP || e == EINVAL; }
static int same(int dir, const char *name, const struct stat *owned) {
    struct stat now;
    if (fstatat(dir, name, &now, AT_SYMLINK_NOFOLLOW)) return errno;
    return S_ISREG(now.st_mode) && now.st_nlink == 1 && now.st_dev == owned->st_dev &&
        now.st_ino == owned->st_ino ? 0 : ESTALE;
}
static int transfer(int fd, int variant, int writing) {
    unsigned char buffer[8192], expected[8192];
    for (size_t i = 0; i < sizeof(expected); ++i) expected[i] = (unsigned char)((i*31+17+(size_t)variant)%251);
    if (lseek(fd, 0, SEEK_SET) < 0) return errno;
    if (writing) memcpy(buffer, expected, sizeof(buffer));
    else {
        struct stat st;
        if (fstat(fd, &st)) return errno;
        if (!S_ISREG(st.st_mode) || st.st_nlink != 1 || st.st_size != 8192) return EIO;
    }
    size_t done = 0;
    while (done < sizeof(buffer)) {
        ssize_t n = writing ? write(fd, buffer+done, sizeof(buffer)-done) : read(fd, buffer+done, sizeof(buffer)-done);
        if (n < 0) { if (errno == EINTR) continue; return errno; }
        if (!n) return EIO;
        done += (size_t)n;
    }
    return writing || !memcmp(buffer, expected, sizeof(buffer)) ? 0 : EIO;
}
static int create(int dir, const char *name, int *fd, struct stat *st, int *owned) {
    *fd = openat(dir, name, O_CREAT|O_EXCL|O_RDWR|O_NOFOLLOW|O_CLOEXEC, 0600);
    if (*fd < 0) return errno;
    if (fstat(*fd, st)) return errno;
    if (!S_ISREG(st->st_mode) || st->st_nlink != 1) return ESTALE;
    *owned = 1;
    return 0;
}
static int inspect_renamed(int dir, const struct stat *want, int variant) {
    int e = same(dir, "b", want);
    if (e) return e;
    int fd = openat(dir, "b", O_RDONLY|O_NOFOLLOW|O_CLOEXEC);
    if (fd < 0) return errno;
    struct stat st;
    if (fstat(fd, &st)) e = errno;
    else if (st.st_dev != want->st_dev || st.st_ino != want->st_ino) e = ESTALE;
    else e = transfer(fd, variant, 0);
    if (close(fd) && !e) e = errno;
    return e;
}
static void quote(const char *s) {
    putchar('"');
    for (const unsigned char *p = (const unsigned char *)s; *p; ++p) {
        if (*p == '"' || *p == '\\') { putchar('\\'); putchar(*p); }
        else if (*p < 32 || *p > 126) printf("\\u%04x", (unsigned)*p);
        else putchar(*p);
    }
    putchar('"');
}
int main(int argc, char **argv) {
    (void)argv;
    int fd = -1, dir = -1, own_dir = 0, own_a = 0, own_b = 0, e = 0;
    struct stat parent = {0}, directory = {0}, a = {0}, b = {0}, now;
    char domain[256] = {0};
    if (argc != 1) { record("arguments", EINVAL, 0); goto output; }
    /* Broker-managed user filesystems may assign a storage UID distinct from
     * the actor. The APP pins this exclusively-created directory's dev/inode;
     * it separately checks this child's actor UID, PPID and domain. */
    e = fstat(PROBE_DIRECTORY_FD, &parent) ? errno : (!S_ISDIR(parent.st_mode) ? ENOTDIR : 0);
#ifdef PICO_NATIVE_MANAGED_PROBE
    record("reopened_directory", e, 0); if (e) goto output;
#else
    record("inherited_directory", e, 0); if (e) goto output;
#endif
    fd = open("/proc/self/attr/current", O_RDONLY|O_CLOEXEC);
    if (fd < 0) e = errno;
    else {
        ssize_t n;
        do { n = read(fd, domain, sizeof(domain)-1); } while (n < 0 && errno == EINTR);
        e = n <= 0 ? (n < 0 ? errno : EIO) : (n == (ssize_t)sizeof(domain)-1 ? EOVERFLOW : 0);
        close(fd); fd = -1;
        if (!e) { while (n > 0 && (domain[n-1] == '\n' || domain[n-1] == '\r' || domain[n-1] == '\0' || domain[n-1] == ' ')) domain[--n] = 0; if (!n) e = EIO; }
    }
    record("domain", e, 0); if (e) goto output;
    if (mkdirat(PROBE_DIRECTORY_FD, "native", 0700)) { record("mkdir", errno, 0); goto output; }
    own_dir = 1; record("mkdir", 0, 0);
    dir = openat(PROBE_DIRECTORY_FD, "native", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if (dir < 0) { record("open_directory", errno, 0); goto cleanup; }
    if (fstat(dir, &directory)) { record("directory_identity", errno, 0); goto cleanup; }
#define STEP(name, expression) do { e = (expression); record(name, e, 0); if (e) goto cleanup; } while (0)
    STEP("create", create(dir, "a", &fd, &a, &own_a));
    STEP("write", transfer(fd, 0, 1));
    STEP("fsync", fsync(fd) ? errno : 0);
    STEP("read", transfer(fd, 0, 0));
    e = same(dir, "a", &a);
    if (!e) {
        if (!fstatat(dir, "b", &now, AT_SYMLINK_NOFOLLOW)) e = EEXIST;
        else if (errno != ENOENT) e = errno;
    }
    if (!e && renameat(dir, "a", dir, "b")) e = errno;
    if (!e) { b = a; own_b = 1; own_a = 0; e = inspect_renamed(dir, &b, 0); }
    record("rename_new", e, 0); if (e) goto cleanup;
    close(fd); fd = -1;
    STEP("replacement_create", create(dir, "a", &fd, &a, &own_a));
    STEP("replacement_write", transfer(fd, 1, 1));
    STEP("replacement_fsync", fsync(fd) ? errno : 0);
    e = same(dir, "a", &a); if (!e) e = same(dir, "b", &b);
    if (!e && renameat(dir, "a", dir, "b")) e = errno;
    if (!e) { b = a; own_b = 1; own_a = 0; }
    record("rename_replace", e, 0); if (e) goto cleanup;
    STEP("replacement_read", inspect_renamed(dir, &b, 1));
    e = same(dir, "b", &b); if (!e && unlinkat(dir, "b", 0)) e = errno;
    if (!e) own_b = 0;
    record("unlink", e, 0); if (e) goto cleanup;
    record("dirsync", fsync(dir) ? errno : 0, 1);
    struct statfs fs;
    record("statfs", fstatfs(dir, &fs) ? errno : 0, 1);
cleanup:
    if (fd >= 0) close(fd);
    e = 0;
    if (own_a && dir >= 0) { int x = same(dir, "a", &a); if (!x && unlinkat(dir, "a", 0)) x = errno; if (x) e = x; }
    if (own_b && dir >= 0) { int x = same(dir, "b", &b); if (!x && unlinkat(dir, "b", 0)) x = errno; if (x && !e) e = x; }
    if (dir >= 0) close(dir);
    if (own_dir) {
        if (!directory.st_ino) { if (!e) e = ESTALE; }
        else if (fstatat(PROBE_DIRECTORY_FD, "native", &now, AT_SYMLINK_NOFOLLOW)) { if (!e) e = errno; }
        else if (!S_ISDIR(now.st_mode) || now.st_dev != directory.st_dev || now.st_ino != directory.st_ino) { if (!e) e = ESTALE; }
        else if (unlinkat(PROBE_DIRECTORY_FD, "native", AT_REMOVEDIR) && !e) e = errno;
    }
    record("cleanup", e, 0);
output:
    printf("{\"schema\":1,\"pid\":%ld,\"ppid\":%ld,\"uid\":%ld,\"domain\":", (long)getpid(), (long)getppid(), (long)getuid());
    quote(domain);
    printf(",\"parent_dev\":%llu,\"parent_ino\":%llu,\"operations\":[", (unsigned long long)parent.st_dev, (unsigned long long)parent.st_ino);
    int failed = 0;
    for (int i = 0; i < count; ++i) {
        int optional = ops[i].optional && unsupported(ops[i].error);
        if (i) putchar(',');
        printf("{\"name\":\"%s\",\"ok\":%s,\"errno\":%d,\"unsupported\":%s}", ops[i].name, ops[i].error ? "false" : "true", ops[i].error, optional ? "true" : "false");
        if (ops[i].error && !optional) failed = 1;
    }
    puts("]}");
    return failed ? 1 : 0;
}
