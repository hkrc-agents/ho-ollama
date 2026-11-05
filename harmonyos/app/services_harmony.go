//go:build harmonyos

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Platform host lifecycle only. The installed upstream llama-server still owns
// model loading, tokenization, batching and the public, unauthenticated HTTP API.
type serverConfig struct {
	Model     string `json:"model"`
	Port      int    `json:"port"`
	Threads   int    `json:"threads"`
	Context   int    `json:"context"`
	MemoryMiB int    `json:"memory_mib"`
}
type serverIdentity struct {
	PID        int    `json:"pid"`
	UID        int    `json:"uid"`
	StartTicks string `json:"start_ticks"`
	Executable string `json:"executable"`
}
type serverSnapshot struct {
	State          string          `json:"state"`
	Error          string          `json:"error,omitempty"`
	Config         serverConfig    `json:"config"`
	Identity       *serverIdentity `json:"identity,omitempty"`
	ReadinessProof string          `json:"readiness_proof,omitempty"`
	DesiredRunning bool            `json:"desired_running"`
	Restarts       int             `json:"restarts"`
}

// Version-pinned upstream stdout event, read only from this exec.Cmd's pipes.
// Never parse an existing log file or accept a caller-supplied readiness string.
type serverBindReceipt struct {
	mu         sync.Mutex
	pending    string
	executable string
	port       int
	pid        func() int
	sawBind    bool
	dropping   bool
}

// The shipped b11351 entry point is named llama_server (the executable wrapper
// renames main) and enables the pinned common-log timestamp/INFO prefix.
var serverLogTimestamp = regexp.MustCompile(`^[0-9]+\.[0-9]{2}\.[0-9]{3}\.[0-9]{3}$`)

func pinnedServerBindLine(line string, port int) bool {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) == 7 && f[1] == "I" && serverLogTimestamp.MatchString(f[0]) {
		f = f[2:]
	}
	return len(f) == 5 && f[0] == "srv" && (f[1] == "main:" || f[1] == "llama_server:") && f[2] == "listening" && f[3] == "on" && f[4] == fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (b *serverBindReceipt) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Bounded parsing only: no identity lookup, filesystem access or channel
	// waits on exec's copy goroutine. Drop an oversized physical line through
	// its newline, so its suffix can never masquerade as a fresh bind event.
	for remaining := p; len(remaining) > 0; {
		end := -1
		for n, c := range remaining {
			if c == '\n' {
				end = n
				break
			}
		}
		part := remaining
		if end >= 0 {
			part = remaining[:end]
		}
		if !b.dropping {
			if len(b.pending)+len(part) > 8192 {
				b.pending = ""
				b.dropping = true
			} else {
				b.pending += string(part)
			}
		}
		if end < 0 {
			break
		}
		if !b.dropping {
			if pinnedServerBindLine(b.pending, b.port) {
				b.sawBind = true
			}
		}
		b.pending, b.dropping = "", false
		remaining = remaining[end+1:]
	}
	return len(p), nil
}
func (b *serverBindReceipt) proves(i *serverIdentity) bool {
	b.mu.Lock()
	sawBind := b.sawBind
	b.mu.Unlock()
	// The expected identity belongs to this fresh exec.Cmd; recheck its full
	// identity on the readiness path, never while copying the child's output.
	return sawBind && i != nil && i.Executable == b.executable && i.PID == b.pid() && sameServer(i)
}

type harmonyServer struct {
	backend               harmonyServerBackend // non-nil only in the explicit official-SO build
	op                    sync.Mutex
	mu                    sync.Mutex
	signal                sync.Mutex // Excludes reaping between identity checks and signals.
	directory, executable string
	current               serverSnapshot
	done                  chan struct{}
	cancel                context.CancelFunc
	listenerOwned         func(*serverIdentity, int) bool
	child                 *exec.Cmd // signal protects this unreaped direct child.
	observe               func(*serverIdentity) (bool, error)
	waitExit              func(int) (bool, error)
	recoveryErr           error
	waitErr               error // mu protects exit-observation/pipe-drain errors.
	lifetime              context.Context
	watchCancel           context.CancelFunc
	watchEpoch            uint64
	restartDelay          time.Duration
	restartLimit          int
}

// A missing/replaced/zombie process is confirmed gone. Permission errors and
// malformed proc data are unknown, never authority to discard a live record.
func serverIdentityLive(i *serverIdentity) (bool, error) {
	if i == nil {
		return false, nil
	}
	if i.PID <= 1 || i.UID != os.Getuid() || i.StartTicks == "" || i.Executable == "" {
		return false, errors.New("invalid saved server identity")
	}
	live, err := serverIdentityStatLive(i)
	if err != nil || !live {
		return live, err
	}
	now, err := serverProcess(i.PID, i.Executable)
	if err != nil {
		// Exit may occur between the initial stat and cmdline/UID reads.
		// Confirm disappearance separately rather than turning that normal
		// exit race into an unknown-ownership failure.
		if stillLive, statErr := serverIdentityStatLive(i); statErr == nil && !stillLive {
			return false, nil
		}
		return false, err
	}
	if now != *i {
		return false, errors.New("server ownership cannot be confirmed")
	}
	return true, nil
}

func serverIdentityStatLive(i *serverIdentity) (bool, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(i.PID), "stat"))
	if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return false, errors.New("invalid process stat")
	}
	f := strings.Fields(string(b)[end+1:])
	if len(f) < 20 {
		return false, errors.New("short process stat")
	}
	if _, err := strconv.ParseUint(f[19], 10, 64); err != nil {
		return false, err
	}
	if f[19] != i.StartTicks || f[0] == "Z" || f[0] == "X" {
		return false, nil
	}
	return true, nil
}

// Observe exit without consuming status. Wait must not run until this succeeds:
// a live child's unreaped PID pins the numeric process group during signalling.
func serverChildExited(pid int) (bool, error) {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if err == nil {
		return info.Signo != 0, nil
	}
	if errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return false, fmt.Errorf("non-reaping waitid: %w", err)
	}
	// Some targets lack WNOWAIT. Only an explicit zombie/dead stat for this
	// still-unreaped direct child permits Wait; a read failure is not exit proof.
	b, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if readErr != nil {
		return false, fmt.Errorf("waitid unsupported (%v); exit stat unavailable: %w", err, readErr)
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return false, errors.New("waitid unsupported; invalid exit stat")
	}
	f := strings.Fields(string(b)[end+1:])
	if len(f) < 20 {
		return false, errors.New("waitid unsupported; short exit stat")
	}
	return f[0] == "Z" || f[0] == "X", nil
}

func (s *harmonyServer) reap(cmd *exec.Cmd, log *os.File, done chan struct{}) {
	for {
		s.signal.Lock()
		exited, err := s.waitExit(cmd.Process.Pid)
		s.mu.Lock()
		s.waitErr = err
		s.mu.Unlock()
		if err != nil {
			s.setState("failed", err.Error())
		}
		if !exited || err != nil {
			s.signal.Unlock()
			time.Sleep(25 * time.Millisecond)
			continue
		}
		// Child is confirmed exited. Wait now reaps once and drains the existing
		// stdout observer with the configured two-second WaitDelay bound.
		err = waitExecutableService(cmd)
		if executableDrainBlocked(err) {
			s.mu.Lock()
			s.waitErr = err
			s.current.State = "failed"
			s.current.Error = err.Error()
			s.mu.Unlock()
			s.signal.Unlock()
			time.Sleep(100 * time.Millisecond)
			continue // No exit ACK/restart until the same pinned group drains.
		}
		log.Close()
		s.child = nil
		s.mu.Lock()
		if errors.Is(err, exec.ErrWaitDelay) {
			s.waitErr = err
		}
		if s.done == done && s.current.State != "draining" && s.current.State != "stopping" {
			s.current.State = "failed"
			s.current.Error = fmt.Sprintf("server exited: %v", err)
		}
		close(done)
		s.mu.Unlock()
		s.signal.Unlock()
		return
	}
}

func (s *harmonyServer) signalChild(i *serverIdentity, sig syscall.Signal) error {
	s.signal.Lock()
	defer s.signal.Unlock()
	if s.child == nil {
		return nil
	} // Already reaped; never signal a reused PID.
	if i == nil || s.child.Process.Pid != i.PID {
		return errors.New("direct child identity missing")
	}
	live, err := s.observe(i)
	if err != nil {
		return fmt.Errorf("server ownership unknown: %w", err)
	}
	if !live {
		return nil
	}
	if err = syscall.Kill(-i.PID, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func atomicPrivateJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicPrivateBytes(path, b)
}

func atomicPrivateBytes(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".service-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func serverProcess(pid int, executable string) (serverIdentity, error) {
	i := serverIdentity{PID: pid, UID: os.Getuid(), Executable: executable}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	info, err := os.Stat(root)
	if err != nil {
		return i, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return i, errors.New("process UID mismatch")
	}
	pg, err := syscall.Getpgid(pid)
	if err != nil || pg != pid {
		return i, errors.New("process group mismatch")
	}
	cmd, err := os.ReadFile(root + "/cmdline")
	if err != nil {
		return i, err
	}
	if strings.Split(string(cmd), "\x00")[0] != executable {
		return i, errors.New("process executable mismatch")
	}
	b, err := os.ReadFile(root + "/stat")
	if err != nil {
		return i, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return i, errors.New("invalid process stat")
	}
	f := strings.Fields(string(b)[end+1:])
	if len(f) < 20 || f[0] == "Z" {
		return i, errors.New("process exited or invalid stat")
	}
	i.StartTicks = f[19]
	return i, nil
}
func sameServer(i *serverIdentity) bool {
	if i == nil || i.PID <= 1 || i.UID != os.Getuid() || i.StartTicks == "" {
		return false
	}
	now, err := serverProcess(i.PID, i.Executable)
	return err == nil && now == *i
}

// /proc/net/tcp describes the namespace, not a process. Match the LISTEN inode
// to this process's actual open FDs; successful HTTP alone could be another app.
func serverOwnsPort(i *serverIdentity, port int) bool {
	if !sameServer(i) {
		return false
	}
	root := filepath.Join("/proc", strconv.Itoa(i.PID))
	fds, err := os.ReadDir(root + "/fd")
	if err != nil {
		return false
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, e := os.Readlink(root + "/fd/" + fd.Name())
		if e == nil && strings.HasPrefix(link, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	b, err := os.ReadFile(root + "/net/tcp")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 9 && f[1] == fmt.Sprintf("0100007F:%04X", port) && f[3] == "0A" && inodes[f[9]] {
			return sameServer(i)
		}
	}
	return false
}
func validateServerConfig(c serverConfig, executable string) error {
	if c.Port < 1024 || c.Port > 65535 || c.Threads < 1 || c.Threads > 64 || c.Context < 128 || c.Context > 32768 {
		return errors.New("port 1024–65535, threads 1–64, context 128–32768 and memory limit 128–1048576 MiB required")
	}
	if !filepath.IsAbs(c.Model) || strings.ContainsRune(c.Model, 0) {
		return errors.New("absolute, authorized GGUF path required")
	}
	f, err := os.Open(c.Model)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("model must be a regular GGUF file")
	}
	var magic [4]byte
	if _, err = io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return errors.New("invalid GGUF header")
	}
	if st, err = os.Stat(executable); err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
		return errors.New("installed llama-server unavailable")
	}
	return nil
}
func newHarmonyServer(directory, executable string) *harmonyServer {
	s := &harmonyServer{directory: directory, executable: executable, listenerOwned: serverOwnsPort,
		observe: serverIdentityLive, waitExit: serverChildExited,
		lifetime: context.Background(), restartDelay: time.Second, restartLimit: 5,
		current: serverSnapshot{State: "stopped", Config: serverConfig{Port: 18082, Threads: 2, Context: 1024, MemoryMiB: 1536}}}
	if b, e := os.ReadFile(filepath.Join(directory, "llama-server-config.json")); e == nil {
		var c serverConfig
		if json.Unmarshal(b, &c) == nil {
			s.current.Config = c
		}
	}
	configureHarmonyServer(s)
	if s.backend != nil {
		return s // No old-PID adoption across HAP sessions in managed SO mode.
	}
	var i serverIdentity
	if b, e := os.ReadFile(filepath.Join(directory, "llama-server-instance.json")); !os.IsNotExist(e) {
		if e == nil {
			e = json.Unmarshal(b, &i)
		}
		if e == nil && (i.Executable != executable || i.PID <= 1 || i.StartTicks == "" || i.UID != os.Getuid()) {
			e = errors.New("invalid previous server identity")
		}
		if e != nil {
			s.recoveryErr = e
			s.current.State, s.current.Error = "failed", "cannot read previous instance: "+e.Error()
		} else {
			s.current.Identity = &i
			live, observeErr := s.observe(&i)
			if live || observeErr != nil {
				// Crash recovery cannot pin a non-child PID against reuse. Keep
				// its record and require external recovery; never signal it here.
				s.current.State = "failed"
				s.current.Error = "previous instance is live or ownership unknown; automatic non-child stop/recovery is unsupported"
			}
		}
	}
	return s
}
func (s *harmonyServer) snapshot() serverSnapshot { s.mu.Lock(); defer s.mu.Unlock(); return s.current }
func (s *harmonyServer) setState(state, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.State = state
	s.current.Error = message
}
func (s *harmonyServer) start(ctx context.Context, c serverConfig) error {
	s.op.Lock()
	defer s.op.Unlock()
	if err := s.lifetime.Err(); err != nil {
		return err
	}
	if err := s.startLocked(ctx, c); err != nil {
		return err
	}
	s.enableSupervisionLocked()
	return nil
}
func (s *harmonyServer) startLocked(ctx context.Context, c serverConfig) error {
	if s.backend != nil {
		return s.backend.Start(ctx, c)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	startCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	s.mu.Lock()
	s.cancel = cancel // Publish before validation, persistence or spawn.
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()
	old := s.snapshot()
	live, observeErr := s.observe(old.Identity)
	if observeErr != nil {
		s.setState("failed", "server ownership unknown: "+observeErr.Error())
		return observeErr
	}
	if old.State == "ready" && old.Config == c && live {
		return nil
	}
	if live {
		return errors.New("stop the current instance before changing configuration")
	}
	s.mu.Lock()
	previousDone := s.done
	s.mu.Unlock()
	if previousDone != nil {
		select {
		case <-previousDone:
		default:
			return errors.New("previous direct child has not been reaped; stop before replacing")
		}
	}
	if err := validateServerConfig(c, s.executable); err != nil {
		s.setState("failed", err.Error())
		return err
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", c.Port))
	if err != nil {
		s.setState("failed", "port occupied: "+err.Error())
		return err
	}
	listener.Close()
	if err := startCtx.Err(); err != nil {
		return err
	}
	if err = atomicPrivateJSON(filepath.Join(s.directory, "llama-server-config.json"), c); err != nil {
		s.setState("failed", err.Error())
		return err
	}
	// Config means the last validated, persisted request, including failed starts.
	s.mu.Lock()
	s.current.Config = c
	s.current.ReadinessProof = ""
	s.mu.Unlock()
	log, err := os.OpenFile(filepath.Join(s.directory, "llama-server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		s.setState("failed", err.Error())
		return err
	}
	launcher := filepath.Join(filepath.Dir(s.executable), "..", "..", "bin", "pico-service-launcher")
	cmd := exec.Command(launcher, strconv.Itoa(c.MemoryMiB), s.executable, "--model", c.Model, "--host", "127.0.0.1", "--port", strconv.Itoa(c.Port),
		"--ctx-size", strconv.Itoa(c.Context), "--parallel", "1", "--threads", strconv.Itoa(c.Threads), "--n-gpu-layers", "0")
	receipt := &serverBindReceipt{executable: s.executable, port: c.Port, pid: func() int {
		if cmd.Process == nil {
			return -1
		}
		return cmd.Process.Pid
	}}
	output := io.MultiWriter(log, receipt)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := startCtx.Err(); err != nil {
		log.Close()
		return err
	}
	s.setState("starting", "")
	if err = cmd.Start(); err != nil {
		log.Close()
		s.setState("failed", err.Error())
		return err
	}
	done := make(chan struct{})
	s.signal.Lock()
	s.child = cmd
	s.signal.Unlock()
	s.mu.Lock()
	s.current.Identity = nil
	s.done = done
	s.waitErr = nil
	s.mu.Unlock()
	go s.reap(cmd, log, done)
	var identity serverIdentity
	for attempt := 0; attempt < 20; attempt++ {
		if err = startCtx.Err(); err != nil {
			break
		}
		identity, err = serverProcess(cmd.Process.Pid, s.executable)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		// Identity could not be established. Kill only our still-unreaped
		// direct child, not an unvalidated process group, then let reap Wait.
		s.signal.Lock()
		if s.child == cmd {
			err = errors.Join(err, cmd.Process.Kill())
		}
		s.signal.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			err = errors.Join(err, errors.New("identity failure cleanup: waitpid timeout"))
		}
		s.setState("failed", err.Error())
		return err
	}
	s.mu.Lock()
	s.current.Identity = &identity
	s.mu.Unlock()
	if err = atomicPrivateJSON(filepath.Join(s.directory, "llama-server-instance.json"), identity); err != nil {
		return s.failedStart(err)
	}
	client := &http.Client{Timeout: time.Second}
	for {
		if err := startCtx.Err(); err != nil {
			return s.failedStart(err)
		}
		s.mu.Lock()
		waitErr := s.waitErr
		s.mu.Unlock()
		if waitErr != nil {
			return s.failedStart(waitErr)
		}
		proof := ""
		if s.listenerOwned(&identity, c.Port) {
			proof = "proc_socket_inode_and_http"
		} else if receipt.proves(&identity) {
			proof = "owned_child_bind_pipe_and_http"
		}
		if proof != "" {
			request, _ := http.NewRequestWithContext(startCtx, "GET", fmt.Sprintf("http://127.0.0.1:%d/health", c.Port), nil)
			response, e := client.Do(request)
			if e == nil {
				response.Body.Close()
				if response.StatusCode == 200 && sameServer(&identity) && startCtx.Err() == nil {
					s.mu.Lock()
					if s.waitErr != nil {
						waitErr := s.waitErr
						s.mu.Unlock()
						return s.failedStart(waitErr)
					}
					select {
					case <-done:
						s.mu.Unlock()
						return errors.New("server exited before ready")
					default:
					}
					s.current.ReadinessProof = proof
					s.current.State, s.current.Error = "ready", ""
					s.mu.Unlock()
					return nil
				}
			}
		}
		select {
		case <-done:
			return errors.New(s.snapshot().Error)
		case <-startCtx.Done():
			return s.failedStart(startCtx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}
func (s *harmonyServer) failedStart(cause error) error {
	cleanup := s.stopLocked()
	if cleanup != nil {
		cause = errors.Join(cause, fmt.Errorf("startup cleanup failed: %w", cleanup))
	}
	s.setState("failed", cause.Error())
	return cause
}

// Recheck cancellation while waiting for op, so stop cannot miss the brief
// interval between a starter acquiring op and publishing its cancel function.
func (s *harmonyServer) lockAfterCancel() {
	for {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		if s.op.TryLock() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (s *harmonyServer) restart(ctx context.Context, c serverConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateServerConfig(c, s.executable); err != nil {
		return err
	}
	s.disableSupervision()
	s.lockAfterCancel()
	defer s.op.Unlock()
	// A start already holding op could publish its monitor after the first
	// cancellation. Cancel again under op before replacing/stopping the child.
	s.disableSupervision()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.stopLocked(); err != nil {
		return err
	}
	if err := s.startLocked(ctx, c); err != nil {
		return err
	}
	s.enableSupervisionLocked()
	return nil
}
func (s *harmonyServer) stop() error {
	s.disableSupervision()
	s.lockAfterCancel()
	defer s.op.Unlock()
	s.disableSupervision()
	return s.stopLocked()
}
func (s *harmonyServer) stopLocked() error {
	if s.backend != nil {
		return s.backend.Stop()
	}
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	snapshot := s.snapshot()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	live, err := s.observe(snapshot.Identity)
	if err != nil {
		err = fmt.Errorf("server ownership unknown; identity retained: %w", err)
		s.setState("failed", err.Error())
		return err
	}
	if live {
		s.signal.Lock()
		direct := s.child != nil && s.child.Process.Pid == snapshot.Identity.PID
		s.signal.Unlock()
		if !direct && done == nil {
			err := errors.New("previous live non-child instance: safe automatic stop/crash recovery unsupported; identity retained")
			s.setState("failed", err.Error())
			return err
		}
		s.setState("draining", "")
		if err := s.signalChild(snapshot.Identity, syscall.SIGTERM); err != nil {
			s.setState("failed", err.Error())
			return err
		}
		if done == nil {
			err := errors.New("direct child completion missing; identity retained")
			s.setState("failed", err.Error())
			return err
		}
		// After a validated direct-child TERM, exit can transiently clear
		// cmdline before proc stat reports Z. Await the sole reaper instead of
		// interpreting that transition as an ownership change. No additional
		// signal is authorized by missing/unreadable proc data.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// signalChild revalidates the full identity under signal before KILL.
			// Unknown ownership retains the record and returns without signalling.
			if err := s.signalChild(snapshot.Identity, syscall.SIGKILL); err != nil {
				s.setState("failed", err.Error())
				return err
			}
		}
		s.setState("stopping", "")
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			s.setState("failed", "waitpid timeout")
			return errors.New("waitpid timeout")
		}
	}
	s.mu.Lock()
	waitErr := s.waitErr
	s.mu.Unlock()
	if waitErr != nil {
		s.setState("failed", waitErr.Error())
		return waitErr
	}
	live, err = s.observe(snapshot.Identity)
	if err != nil {
		s.setState("failed", err.Error())
		return err
	}
	if live {
		s.setState("failed", "owned process did not exit")
		return errors.New("owned process did not exit")
	}
	// Do not advertise stopped or lose in-memory identity if record removal fails.
	if err := os.Remove(filepath.Join(s.directory, "llama-server-instance.json")); err != nil && !os.IsNotExist(err) {
		s.setState("failed", err.Error())
		return err
	}
	s.mu.Lock()
	s.current.Identity = nil
	s.current.ReadinessProof = ""
	s.current.State = "stopped"
	s.current.Error = ""
	s.done = nil
	s.mu.Unlock()
	return nil
}

type serverModel struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func installedServerModels() []serverModel {
	root := os.Getenv("OLLAMA_MODELS")
	items := []serverModel{}
	if !filepath.IsAbs(root) {
		return items
	}
	for _, base := range []string{"manifests", "manifests-v2"} {
		filepath.WalkDir(filepath.Join(root, base), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, e := os.ReadFile(path)
			if e != nil || len(b) > 1<<20 {
				return nil
			}
			var m struct {
				Layers []struct {
					MediaType string `json:"mediaType"`
					Digest    string `json:"digest"`
				} `json:"layers"`
			}
			if json.Unmarshal(b, &m) != nil {
				return nil
			}
			for _, layer := range m.Layers {
				if layer.MediaType == "application/vnd.ollama.image.model" && len(layer.Digest) == 71 && strings.HasPrefix(layer.Digest, "sha256:") {
					if _, e = hexDigest(layer.Digest[7:]); e != nil {
						continue
					}
					rel, _ := filepath.Rel(filepath.Join(root, base), path)
					items = append(items, serverModel{filepath.ToSlash(rel), filepath.Join(root, "blobs", strings.ReplaceAll(layer.Digest, ":", "-"))})
				}
			}
			return nil
		})
	}
	return items
}
func hexDigest(value string) (string, error) {
	if len(value) != 64 || strings.Trim(value, "0123456789abcdef") != "" {
		return "", errors.New("invalid digest")
	}
	return value, nil
}

var harmonyServiceActions sync.Mutex

func withHarmonyServices(next http.Handler, token string, server *harmonyServer, core *harmonyCore, root context.Context, inventory ...func() ([]serverModel, error)) http.Handler {
	// Persist user intent and dispatch in the same control-plane order. This
	// does not serialize public inference requests or change the core API.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/harmony/services") {
			next.ServeHTTP(w, r)
			return
		}
		cookie, e := r.Cookie("token")
		if e != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
			http.Error(w, "APP token required", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/harmony/services":
			models := installedServerModels()
			message := ""
			if len(inventory) > 0 {
				var err error
				models, err = inventory[0]()
				if err != nil {
					message = err.Error()
				}
				if models == nil {
					models = []serverModel{}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"llama": server.snapshot(), "ollama": core.Snapshot(), "models": models, "model_error": message})
		case r.Method == "GET" && r.URL.Path == "/api/harmony/services/logs":
			f, e := os.Open(filepath.Join(server.directory, "llama-server.log"))
			if e != nil {
				json.NewEncoder(w).Encode(map[string]string{"log": "No standalone server log yet"})
				return
			}
			defer f.Close()
			st, e := f.Stat()
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			if st.Size() > 65536 {
				f.Seek(st.Size()-65536, io.SeekStart)
			}
			b, _ := io.ReadAll(io.LimitReader(f, 65536))
			json.NewEncoder(w).Encode(map[string]string{"log": string(b)})
		case r.Method == "POST" && r.URL.Path == "/api/harmony/services":
			if root.Err() != nil {
				http.Error(w, "APP is shutting down", http.StatusServiceUnavailable)
				return
			}
			// Startup is cancelled by either the requester or APP shutdown. After
			// readiness, main's service stop owns the running child's lifetime.
			requestCtx, cancel := context.WithCancel(root)
			stopRequest := context.AfterFunc(r.Context(), cancel)
			defer stopRequest()
			defer cancel()
			var request struct {
				Service string       `json:"service"`
				Action  string       `json:"action"`
				Config  serverConfig `json:"config"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
			dec.DisallowUnknownFields()
			if e = dec.Decode(&request); e != nil {
				http.Error(w, "invalid service request", 400)
				return
			}
			var trailing any
			if dec.Decode(&trailing) != io.EOF {
				http.Error(w, "trailing service request", 400)
				return
			}
			if request.Service != "llama" && request.Service != "ollama" {
				http.Error(w, "unknown service", 400)
				return
			}
			if request.Action != "start" && request.Action != "stop" && request.Action != "restart" {
				http.Error(w, "unknown action", 400)
				return
			}
			if root.Err() != nil || r.Context().Err() != nil || requestCtx.Err() != nil {
				http.Error(w, "service request cancelled or APP shutting down", http.StatusServiceUnavailable)
				return
			}
			harmonyServiceActions.Lock()
			defer harmonyServiceActions.Unlock()
			if root.Err() != nil || requestCtx.Err() != nil {
				http.Error(w, "service request cancelled", http.StatusServiceUnavailable)
				return
			}
			if request.Service == "llama" {
				if request.Action != "stop" {
					if err := validateServerConfig(request.Config, server.executable); err != nil {
						http.Error(w, err.Error(), 409)
						return
					}
				}
			}
			if err := recordHarmonyServiceIntent(request.Service, request.Action, request.Config); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			if request.Service == "llama" {
				switch request.Action {
				case "stop":
					e = server.stop()
				case "start":
					e = server.start(requestCtx, request.Config)
				case "restart":
					e = server.restart(requestCtx, request.Config)
				}
			} else {
				switch request.Action {
				case "stop":
					e = core.Stop()
				case "start":
					e = core.Start(root)
				case "restart":
					e = core.Restart(root)
				}
			}
			if e != nil {
				http.Error(w, e.Error(), 409)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	})
}
