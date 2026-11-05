//go:build harmonyos

package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var pidFile = filepath.Join(os.Getenv("HOME"), ".ollama", "desktop-server.pid")
var serverLogPath = filepath.Join(os.Getenv("HOME"), ".ollama", "logs", "server.log")

// Diagnostic throttling only: one warning per instance and failure category.
var coreListenerDiagnostics sync.Map

var ErrCoreExited = errors.New("core exited")

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// Keep core and its runners in their own group, separate from ollama-app.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	// Server.cmd replaces Cancel with stop(cmd.Process), which reaches terminate
	// below. Keep this safe default for direct callers too.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return terminate(cmd.Process)
	}
	return cmd
}

// CoreIdentity is a private sidecar to desktop-server.pid; never change the
// upstream integer PID format. StartTicks distinguishes PID reuse within a boot.
type CoreIdentity struct {
	PID        int    `json:"pid"`
	UID        uint32 `json:"uid"`
	PPID       int    `json:"ppid"`
	PGID       int    `json:"pgid"`
	StartTicks uint64 `json:"start_ticks"`
	Executable string `json:"executable"`
}

func CoreProcessIdentity(pid int) (CoreIdentity, error) {
	var id CoreIdentity
	if pid <= 1 {
		return id, errors.New("invalid core PID")
	}
	path := filepath.Join("/proc", strconv.Itoa(pid))
	info, err := os.Stat(path)
	if err != nil {
		return id, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return id, errors.New("core UID mismatch")
	}
	data, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return id, err
	}
	ppid, pgid, ticks, err := coreParseStat(string(data))
	if err != nil {
		return id, err
	}
	exe, err := os.Readlink(filepath.Join(path, "exe"))
	if err != nil {
		return id, err
	}
	return CoreIdentity{pid, stat.Uid, ppid, pgid, ticks, exe}, nil
}

func coreParseStat(data string) (ppid, pgid int, ticks uint64, err error) {
	// comm can contain spaces and parentheses; fields after the last ')' start
	// at stat field 3. starttime is field 22, hence index 19 here.
	i := strings.LastIndex(data, ")")
	if i < 0 {
		return 0, 0, 0, errors.New("invalid proc stat")
	}
	f := strings.Fields(data[i+1:])
	if len(f) < 20 {
		return 0, 0, 0, errors.New("invalid proc stat")
	}
	if f[0] == "Z" || f[0] == "X" {
		return 0, 0, 0, ErrCoreExited
	}
	ppid, err = strconv.Atoi(f[1])
	if err != nil {
		return
	}
	pgid, err = strconv.Atoi(f[2])
	if err != nil {
		return
	}
	ticks, err = strconv.ParseUint(f[19], 10, 64)
	if err == nil && ticks == 0 {
		err = errors.New("invalid start ticks")
	}
	return
}

func CoreIdentityMatches(id CoreIdentity) bool {
	live, err := CoreProcessIdentity(id.PID)
	return err == nil && id == live
}

func CoreReadIdentity(path string) (CoreIdentity, error) {
	var id CoreIdentity
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &id)
	}
	if err == nil && (id.PID <= 1 || id.StartTicks == 0 || id.Executable == "") {
		err = errors.New("invalid core identity")
	}
	return id, err
}

func CorePersistIdentity(path string, id CoreIdentity) error {
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".core-identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func coreRecordedProcess(pid int) bool {
	id, err := CoreReadIdentity(pidFile + ".identity.json")
	if err != nil || id.PID != pid || !CoreIdentityMatches(id) {
		return false
	}
	cmd, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	return err == nil && ollamaServeArgs(strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00"))
}

func ollamaServeProcess(pid int) bool {
	return coreRecordedProcess(pid)
}

func terminate(proc *os.Process) error {
	if !ollamaServeProcess(proc.Pid) {
		// Upstream stop() calls proc.Kill() on ANY terminate error. Return a
		// no-op success, and terminated() below reports this identity gone.
		slog.Warn("refusing to signal unowned core", "pid", proc.Pid)
		return nil
	}
	if err := proc.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Warn("core interrupt failed; bounded command wait will handle its direct child", "pid", proc.Pid, "error", err)
	}
	// Give core the first chance to stop/reap its runners. Escalate only the
	// recorded dedicated group, rechecking start ticks before every signal.
	id, err := CoreReadIdentity(pidFile + ".identity.json")
	if err != nil || id.PID != proc.Pid {
		return nil
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for CoreIdentityMatches(id) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if CoreIdentityMatches(id) && id.PGID == id.PID {
		if err := syscall.Kill(-id.PGID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			slog.Warn("core group terminate", "error", err)
		}
		deadline = time.Now().Add(500 * time.Millisecond)
		for CoreIdentityMatches(id) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if CoreIdentityMatches(id) {
			if err := syscall.Kill(-id.PGID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				slog.Warn("core group kill", "error", err)
			}
		}
	}
	return nil
}

// CoreListenerOwned requires either the listening inode in this instance's fd
// table, or fresh child-pipe bind evidence when proc-net access is denied.
// The caller must additionally check health; HTTP alone never proves ownership.
func CoreListenerOwned(id CoreIdentity, address string) bool {
	return CoreListenerProof(id, address) != ""
}

// CoreListenerProof reports the mechanism, not HTTP/model readiness.
func CoreListenerProof(id CoreIdentity, address string) string {
	return coreListenerProof(id, address, os.ReadFile)
}

// readNet is a unit-test seam for proc-net permission failures. Production
// always uses os.ReadFile; identities/fds and pipe records remain real.
func coreListenerProof(id CoreIdentity, address string, readNet func(string) ([]byte, error)) string {
	diagnostic := func(category string, attrs ...any) {
		key := fmt.Sprintf("%d/%d/%s", id.PID, id.StartTicks, category)
		if _, loaded := coreListenerDiagnostics.LoadOrStore(key, struct{}{}); !loaded {
			fields := []any{"pid", id.PID, "start_ticks", id.StartTicks, "check", category}
			slog.Warn("core listener ownership diagnostic", append(fields, attrs...)...)
		}
	}
	if !CoreIdentityMatches(id) {
		return ""
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return ""
	}
	root := filepath.Join("/proc", strconv.Itoa(id.PID))
	fds, err := os.ReadDir(root + "/fd")
	if err != nil {
		diagnostic("fd_readdir", "path", root+"/fd", "error", err)
		return ""
	}
	inodes := map[string]bool{}
	readlinkErrors := 0
	var firstReadlinkError error
	for _, fd := range fds {
		link, err := os.Readlink(root + "/fd/" + fd.Name())
		if err != nil {
			readlinkErrors++
			if firstReadlinkError == nil {
				firstReadlinkError = err
			}
		}
		if err == nil && strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	if readlinkErrors > 0 {
		diagnostic("fd_readlink", "errors", readlinkErrors, "first_error", firstReadlinkError, "socket_inodes", len(inodes))
	}
	listeners := []string{}
	listenerCount := 0
	procNetDenied := false
	for _, table := range []string{"tcp", "tcp6"} {
		b, err := readNet(root + "/net/" + table)
		if err != nil {
			diagnostic("read_"+table, "path", root+"/net/"+table, "error", err)
			if errors.Is(err, os.ErrPermission) && ((ip.To4() != nil && table == "tcp") || (ip.To4() == nil && table == "tcp6")) {
				procNetDenied = true
			}
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 10 && f[3] == "0A" {
				listenerCount++
				if len(listeners) < 16 {
					// Only local LISTEN address, inode and fd membership; never
					// include remote addresses, UIDs or the complete proc row.
					listeners = append(listeners, fmt.Sprintf("%s local=%s inode=%s owned_fd=%t", table, f[1], f[9], inodes[f[9]]))
				}
			}
			if len(f) < 10 || f[3] != "0A" || !inodes[f[9]] {
				continue
			}
			h, p, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			pn, err := strconv.ParseUint(p, 16, 16)
			if err != nil || pn != n {
				continue
			}
			local, err := hex.DecodeString(h)
			if err != nil || (len(local) != 4 && len(local) != 16) {
				continue
			}
			for i := 0; i < len(local); i += 4 {
				v := binary.BigEndian.Uint32(local[i : i+4])
				binary.NativeEndian.PutUint32(local[i:i+4], v)
			}
			addr := net.IP(local)
			if (addr.Equal(ip) || addr.IsUnspecified()) && CoreIdentityMatches(id) {
				return "proc-fd-inode"
			}
		}
	}
	if procNetDenied && corePipeBindOwned(id, address) {
		return "child-pipe-bind"
	}
	category := "no_matched_listener"
	if listenerCount == 0 {
		category = "no_listen_entries"
	}
	diagnostic(category, "address", address, "socket_inodes", len(inodes), "listen_count", listenerCount, "local_listen", listeners)
	return ""
}

// CoreRecordChild binds a sidecar only to a freshly spawned direct child with
// the exact expected executable and a dedicated process group.
func CoreRecordChild(path string, pid int, executable string) (CoreIdentity, error) {
	id, err := CoreProcessIdentity(pid)
	if err != nil {
		return id, err
	}
	if id.PPID != os.Getpid() || id.PGID != pid || id.Executable != executable {
		return id, fmt.Errorf("core child identity mismatch: pid %d", pid)
	}
	if !CoreIdentityMatches(id) {
		return id, errors.New("core changed while recording")
	}
	return id, CorePersistIdentity(path, id)
}

func terminated(pid int) (bool, error) {
	// Also prevents stale PID files from authorizing signals to unrelated processes.
	return !ollamaServeProcess(pid), nil
}

func reapServers() error {
	// Unlike desktop pgrep, never kill arbitrary same-user servers on port conflict.
	return errors.New("port conflict: stop the explicitly owned service first")
}
