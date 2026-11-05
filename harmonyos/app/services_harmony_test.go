//go:build harmonyos

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Real child process + socket ownership/readiness/exit tests, not HTTP mocks.
func fixtureServer(t *testing.T) (*harmonyServer, serverConfig) {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "lib", "ollama", "llama-server")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	self := os.Getenv("PICO_SERVICE_HELPER_BINARY")
	if self == "" {
		t.Fatal("PICO_SERVICE_HELPER_BINARY must identify the separately built non-race fixture")
	}
	b, e := os.ReadFile(self)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(exe, b, 0700); e != nil {
		t.Fatal(e)
	}
	launcher := filepath.Join(root, "bin", "pico-service-launcher")
	os.MkdirAll(filepath.Dir(launcher), 0700)
	// Source is carried next to this test by prepare; an absolute override supports
	// independently cross-building the same fixture for the target SDK.
	source := os.Getenv("PICO_SERVICE_LAUNCHER_SOURCE")
	if source == "" {
		t.Fatal("PICO_SERVICE_LAUNCHER_SOURCE must identify the actual launcher source")
	}
	if output, e := exec.Command("cc", "-O2", "-Wall", "-Wextra", "-Werror", source, "-o", launcher).CombinedOutput(); e != nil {
		t.Fatalf("launcher: %v %s", e, output)
	}
	model := filepath.Join(root, "model.gguf")
	os.WriteFile(model, []byte("GGUFfixture"), 0600)
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s := newHarmonyServer(root, exe)
	t.Cleanup(func() {
		if t.Failed() {
			if b, e := os.ReadFile(filepath.Join(root, "llama-server.log")); e == nil {
				t.Logf("child fixture log: %s", b)
			}
		}
		if e := s.stop(); e != nil {
			t.Error(e)
		}
	})
	return s, serverConfig{Model: model, Port: port, Threads: 2, Context: 512, MemoryMiB: 1536}
}
func TestStandaloneLifecycle(t *testing.T) {
	s, c := fixtureServer(t)
	if e := s.start(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	first := s.snapshot()
	if first.State != "ready" || !serverOwnsPort(first.Identity, c.Port) {
		t.Fatal(first)
	}
	if e := s.start(context.Background(), c); e != nil || s.snapshot().Identity.PID != first.Identity.PID {
		t.Fatal("duplicate start", e)
	}
	c.Threads = 3
	if e := s.start(context.Background(), c); e == nil {
		t.Fatal("changed running config accepted")
	}
	c.Threads = 2
	if e := s.restart(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if s.snapshot().Identity.PID == first.Identity.PID || sameServer(first.Identity) {
		t.Fatal("restart did not replace/reap instance")
	}
	if e := s.stop(); e != nil {
		t.Fatal(e)
	}
	if e := s.stop(); e != nil {
		t.Fatal(e)
	}
	if s.snapshot().State != "stopped" {
		t.Fatal(s.snapshot())
	}
	b, e := os.ReadFile(filepath.Join(s.directory, "llama-server.log"))
	if e != nil || !strings.Contains(string(b), "RLIMIT_AS=disabled") {
		t.Fatal("disabled-policy receipt absent", e)
	}
}

func TestStandalonePipeReadinessWithoutProcNet(t *testing.T) {
	s, c := fixtureServer(t)
	s.listenerOwned = func(*serverIdentity, int) bool { return false }
	if e := s.start(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if s.snapshot().ReadinessProof != "owned_child_bind_pipe_and_http" {
		t.Fatal(s.snapshot())
	}
	i := s.snapshot().Identity
	b := &serverBindReceipt{executable: s.executable, port: c.Port, pid: func() int { return i.PID }}
	b.Write([]byte(fmt.Sprintf("srv main: listening on http://127.0.0.1:%d\n", c.Port+1)))
	if b.proves(i) {
		t.Fatal("wrong port accepted")
	}
	b.Write([]byte("srv main: listening on http://127.0.0.1:"))
	b.Write([]byte(strconv.Itoa(c.Port) + "\n"))
	if !b.proves(i) {
		t.Fatal("split current-child bind event rejected")
	}
	stale := *i
	stale.StartTicks += "0"
	if b.proves(&stale) {
		t.Fatal("stale instance accepted")
	}
}

func TestPinnedPackagedServerBindLogFormat(t *testing.T) {
	for _, line := range []string{
		"srv main: listening on http://127.0.0.1:18082",
		"0.04.296.137 I srv  llama_server: listening on http://127.0.0.1:18082",
	} {
		if !pinnedServerBindLine(line, 18082) {
			t.Fatalf("valid pinned event rejected: %q", line)
		}
	}
	for _, line := range []string{
		"0.04.296.137 W srv  llama_server: listening on http://127.0.0.1:18082",
		"badtime I srv  llama_server: listening on http://127.0.0.1:18082",
		"0.04.296.137 I srv  load_model: listening on http://127.0.0.1:18082",
		"0.04.296.137 I srv  llama_server: listening on http://127.0.0.1:18083",
		"echo srv main: listening on http://127.0.0.1:18082",
		"srv main: listening on http://127.0.0.1:18082 unexpected",
	} {
		if pinnedServerBindLine(line, 18082) {
			t.Fatalf("invalid event accepted: %q", line)
		}
	}
}

func TestStandaloneHTTPWithoutBindProofRejected(t *testing.T) {
	s, c := fixtureServer(t)
	s.listenerOwned = func(*serverIdentity, int) bool { return false }
	t.Setenv("PICO_HELPER_NO_BIND_EVENT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := s.start(ctx, c); e == nil {
		t.Fatal("bare HTTP became readiness proof")
	}
	if sameServer(s.snapshot().Identity) {
		t.Fatal("failed readiness left process alive")
	}
}
func TestStandaloneConflictAndInvalidModel(t *testing.T) {
	s, c := fixtureServer(t)
	l, e := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", c.Port))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if e := s.start(context.Background(), c); e == nil {
		t.Fatal("occupied port accepted")
	}
	if s.snapshot().Identity != nil {
		t.Fatal("created process for occupied port")
	}
	l.Close()
	c.Model += "missing"
	if e := s.start(context.Background(), c); e == nil {
		t.Fatal("missing file accepted")
	}
}
func TestStandaloneStaleRecordCannotAuthorizeAnotherProcess(t *testing.T) {
	s, c := fixtureServer(t)
	if e := s.start(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	i := *s.snapshot().Identity
	i.StartTicks += "9"
	if sameServer(&i) || serverOwnsPort(&i, c.Port) {
		t.Fatal("stale start ticks accepted")
	}
	i = *s.snapshot().Identity
	i.Executable += "other"
	if sameServer(&i) {
		t.Fatal("wrong executable accepted")
	}
	i = *s.snapshot().Identity
	i.UID++
	if sameServer(&i) {
		t.Fatal("wrong UID accepted")
	}
}
func TestStandaloneConcurrentStart(t *testing.T) {
	s, c := fixtureServer(t)
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- s.start(context.Background(), c) }()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	if s.snapshot().State != "ready" {
		t.Fatal(s.snapshot())
	}
}
func TestStandaloneExitAndRecovery(t *testing.T) {
	s, c := fixtureServer(t)
	if e := s.start(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	i := s.snapshot().Identity
	p, _ := os.FindProcess(i.PID)
	p.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for s.snapshot().State != "failed" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.snapshot().State != "failed" {
		t.Fatal("unexpected exit not observed")
	}
	if e := s.start(context.Background(), c); e != nil {
		t.Fatal(e)
	}
}
func TestStandaloneInterruptedStart(t *testing.T) {
	s, c := fixtureServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s.start(ctx, c); e == nil {
		t.Fatal("cancelled start accepted")
	}
	if sameServer(s.snapshot().Identity) {
		t.Fatal("cancel left owned process")
	}
}
func TestServiceAPIPrivateBoundaryAndValidation(t *testing.T) {
	s, _ := fixtureServer(t)
	handler := withHarmonyServices(http.NotFoundHandler(), "secret", s, nil, context.Background())
	for _, body := range []string{`{}`, `{"service":"llama","action":"unknown"}`, `{"service":"llama","action":"stop","extra":true}`, `{} {}`} {
		r := httptest.NewRequest("POST", "/api/harmony/services", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "token", Value: "secret"})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "/api/harmony/services", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
func TestInstalledModelsUseRealManifest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OLLAMA_MODELS", root)
	digest := strings.Repeat("a", 64)
	path := filepath.Join(root, "manifests-v2", "ollama.com", "library", "example", "tag")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(`{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:`+digest+`"}]}`), 0600)
	items := installedServerModels()
	if len(items) != 1 || !strings.HasSuffix(items[0].Path, "sha256-"+digest) {
		t.Fatal(items)
	}
	var c serverConfig
	if e := json.Unmarshal([]byte(`{"port":18082,"threads":2,"context":512,"memory_mib":1536}`), &c); e != nil || strconv.Itoa(c.Port) != "18082" {
		t.Fatal(e)
	}
}

func TestStandaloneInvalidRestartPreservesReadyInstance(t *testing.T) {
	s, c := fixtureServer(t)
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	first := s.snapshot()
	bad := c
	bad.Model += ".missing"
	if err := s.restart(context.Background(), bad); err == nil {
		t.Fatal("invalid restart accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.restart(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	now := s.snapshot()
	if now.State != "ready" || *now.Identity != *first.Identity || !sameServer(first.Identity) {
		t.Fatal("invalid/cancelled restart disturbed healthy instance", now)
	}
}

func TestBindReceiptDropsOversizedLineThroughNewlineWithoutIdentityLookup(t *testing.T) {
	b := &serverBindReceipt{port: 18082, pid: func() int { panic("identity lookup on copy path") }}
	line := "srv main: listening on http://127.0.0.1:18082\n"
	// Splitting just after the bound must not turn the following suffix into
	// a new physical line. Write must never invoke even the PID lookup seam.
	if n, err := b.Write([]byte(strings.Repeat("x", 8193))); n != 8193 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := b.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if b.sawBind || b.dropping || b.pending != "" {
		t.Fatal("oversized line suffix accepted", b)
	}
	if _, err := b.Write([]byte(strings.Repeat("x", 8193) + line)); err != nil {
		t.Fatal(err)
	}
	if b.sawBind {
		t.Fatal("single-write oversized line suffix accepted")
	}
	if _, err := b.Write([]byte(line[:12])); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte(line[12:])); err != nil {
		t.Fatal(err)
	}
	if !b.sawBind {
		t.Fatal("valid next physical line rejected")
	}
}

func TestStandaloneCancelledStartHasNoSideEffects(t *testing.T) {
	s, c := fixtureServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.start(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, name := range []string{"llama-server-config.json", "llama-server-instance.json", "llama-server.log"} {
		if _, err := os.Stat(filepath.Join(s.directory, name)); !os.IsNotExist(err) {
			t.Fatalf("created %s: %v", name, err)
		}
	}
}

func TestStandaloneFailedSpawnKeepsPersistedConfigConsistent(t *testing.T) {
	s, c := fixtureServer(t)
	launcher := filepath.Join(filepath.Dir(s.executable), "..", "..", "bin", "pico-service-launcher")
	if err := os.Rename(launcher, launcher+".disabled"); err != nil {
		t.Fatal(err)
	}
	c.Threads = 7
	if err := s.start(context.Background(), c); err == nil {
		t.Fatal("missing launcher accepted")
	}
	b, err := os.ReadFile(filepath.Join(s.directory, "llama-server-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved serverConfig
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved != c || s.snapshot().Config != saved || s.snapshot().State != "failed" {
		t.Fatal("snapshot differs from persisted requested config", saved, s.snapshot())
	}
}

func TestStandaloneUnknownOwnershipPreservesRecordAndBlocksReplacement(t *testing.T) {
	s, c := fixtureServer(t)
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	i := *s.snapshot().Identity
	path := filepath.Join(s.directory, "llama-server-instance.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observe := s.observe
	s.observe = func(*serverIdentity) (bool, error) { return false, syscall.EACCES }
	defer func() { s.observe = observe }()
	if err := s.stop(); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
	if err := s.start(context.Background(), c); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || s.snapshot().Identity == nil || *s.snapshot().Identity != i || !sameServer(&i) {
		t.Fatal("unknown ownership discarded identity or killed child", err, s.snapshot())
	}
}

func TestStandaloneFailedStartIncludesCleanupError(t *testing.T) {
	s, _ := fixtureServer(t)
	observe := s.observe
	s.observe = func(*serverIdentity) (bool, error) { return false, syscall.EACCES }
	defer func() { s.observe = observe }()
	err := s.failedStart(context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, syscall.EACCES) ||
		!strings.Contains(s.snapshot().Error, "startup cleanup failed") {
		t.Fatal("cleanup failure hidden", err, s.snapshot())
	}
}

func TestStandaloneRecoveredLiveInstanceIsNeverSignalled(t *testing.T) {
	s, c := fixtureServer(t)
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	i := *s.snapshot().Identity
	recovered := newHarmonyServer(s.directory, s.executable)
	if err := recovered.stop(); err == nil || !strings.Contains(err.Error(), "non-child") {
		t.Fatal(err)
	}
	if err := recovered.start(context.Background(), c); err == nil {
		t.Fatal("replaced recovered live instance")
	}
	if !sameServer(&i) || recovered.snapshot().Identity == nil || recovered.snapshot().State != "failed" {
		t.Fatal("recovery lost or signalled previous instance", recovered.snapshot())
	}
	if _, err := os.Stat(filepath.Join(s.directory, "llama-server-instance.json")); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneMalformedRecoveryRecordBlocksMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llama-server-instance.json")
	if err := os.WriteFile(path, []byte("{incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newHarmonyServer(dir, "/unused/llama-server")
	if err := s.stop(); err == nil {
		t.Fatal("discarded unreadable recovery record")
	}
	if err := s.start(context.Background(), serverConfig{}); err == nil {
		t.Fatal("started despite recovery error")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{incomplete" {
		t.Fatal("recovery record modified", err)
	}
}

func TestStandaloneWaitidDoesNotReapLiveChildOrBlockStop(t *testing.T) {
	s, c := fixtureServer(t)
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	i := *s.snapshot().Identity
	if exited, err := serverChildExited(i.PID); err != nil || exited {
		t.Fatal("live exit observation", exited, err)
	}
	// A waiter must release signal while the process is alive, not enter Wait.
	acquired := make(chan struct{})
	go func() { s.signal.Lock(); s.signal.Unlock(); close(acquired) }()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("reaper blocked signalling while child alive")
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, i.PID, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatal("child status not consumed by the sole Wait", err)
	}
}

func TestStandaloneStopAwaitsReaperAcrossTransientExitCmdline(t *testing.T) {
	s, c := fixtureServer(t)
	var allowReap atomic.Bool
	allowReap.Store(true)
	s.waitExit = func(pid int) (bool, error) {
		if !allowReap.Load() {
			return false, nil
		}
		return serverChildExited(pid)
	}
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	done := s.done
	observe := s.observe
	defer func() { s.observe = observe; allowReap.Store(true) }()
	allowReap.Store(false)
	calls, transientReads := 0, 0
	s.observe = func(i *serverIdentity) (bool, error) {
		calls++
		if calls == 2 {
			// Initial ownership and TERM authorization succeed. Hold non-reaping
			// exit observation briefly so an old immediate poll deterministically
			// encounters the simulated empty-cmdline transition before done.
			time.AfterFunc(100*time.Millisecond, func() { allowReap.Store(true) })
		}
		if calls > 2 {
			select {
			case <-done:
			default:
				transientReads++
				return false, errors.New("process executable mismatch: injected exit-time empty cmdline")
			}
		}
		return observe(i)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	if transientReads != 0 || calls != 3 || s.snapshot().State != "stopped" || s.snapshot().Identity != nil {
		t.Fatal("stop observed an unreaped exit transition or skipped final validation", calls, transientReads, s.snapshot())
	}
	if _, err := os.Stat(filepath.Join(s.directory, "llama-server-instance.json")); !os.IsNotExist(err) {
		t.Fatal("completed stop retained record", err)
	}
}

func TestStandaloneStopTimeoutUnknownStillRetainsIdentity(t *testing.T) {
	s, c := fixtureServer(t)
	var allowReap atomic.Bool
	allowReap.Store(true)
	s.waitExit = func(pid int) (bool, error) {
		if !allowReap.Load() {
			return false, nil
		}
		return serverChildExited(pid)
	}
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	done := s.done
	path := filepath.Join(s.directory, "llama-server-instance.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observe := s.observe
	defer func() {
		s.observe = observe
		allowReap.Store(true)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("test child reaper did not finish")
		}
	}()
	allowReap.Store(false)
	calls := 0
	s.observe = func(i *serverIdentity) (bool, error) {
		calls++
		if calls > 2 {
			return false, syscall.EACCES
		}
		return observe(i)
	}
	// The first two observations authorize TERM. At the grace timeout the
	// full identity cannot be read: escalation must fail closed, retaining
	// identity and record even though the direct child has actually exited.
	if err := s.stop(); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) || s.snapshot().Identity == nil || s.snapshot().State != "failed" || calls != 3 {
		t.Fatal("unknown timeout ownership discarded record or skipped validation", err, calls, s.snapshot())
	}
}

func TestStandaloneStopFinalUnknownStillRetainsIdentity(t *testing.T) {
	s, c := fixtureServer(t)
	if err := s.start(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	observe := s.observe
	defer func() { s.observe = observe }()
	calls := 0
	s.observe = func(i *serverIdentity) (bool, error) {
		calls++
		if calls > 2 {
			return false, syscall.EACCES
		}
		return observe(i)
	}
	if err := s.stop(); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
	if s.snapshot().Identity == nil || s.snapshot().State != "failed" {
		t.Fatal("final unknown observation lost ownership", s.snapshot())
	}
	if _, err := os.Stat(filepath.Join(s.directory, "llama-server-instance.json")); err != nil {
		t.Fatal("final unknown observation removed record", err)
	}
}

func TestStandaloneStopCancelsStartupWithoutWaitingForReadinessTimeout(t *testing.T) {
	s, c := fixtureServer(t)
	s.listenerOwned = func(*serverIdentity, int) bool { return false }
	t.Setenv("PICO_HELPER_NO_BIND_EVENT", "1")
	started := make(chan error, 1)
	go func() { started <- s.start(context.Background(), c) }()
	deadline := time.Now().Add(3 * time.Second)
	for s.snapshot().Identity == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.snapshot().Identity == nil {
		t.Fatal("startup did not publish identity")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("stop missed startup cancellation")
	}
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestServiceAPIRejectsShutdownMutation(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	cancel()
	// Nil controllers prove rejection happens before any lifecycle call.
	handler := withHarmonyServices(http.NotFoundHandler(), "secret", nil, nil, root)
	r := httptest.NewRequest("POST", "/api/harmony/services", strings.NewReader(`{"service":"llama","action":"start"}`))
	r.AddCookie(&http.Cookie{Name: "token", Value: "secret"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestServiceAPICancelsStartupFromRootOrRequest(t *testing.T) {
	for _, source := range []string{"root", "request"} {
		t.Run(source, func(t *testing.T) {
			s, c := fixtureServer(t)
			s.listenerOwned = func(*serverIdentity, int) bool { return false }
			t.Setenv("PICO_HELPER_NO_BIND_EVENT", "1")
			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			handler := withHarmonyServices(http.NotFoundHandler(), "secret", s, nil, root)
			body, err := json.Marshal(map[string]any{"service": "llama", "action": "start", "config": c})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/api/harmony/services", strings.NewReader(string(body))).WithContext(requestCtx)
			r.AddCookie(&http.Cookie{Name: "token", Value: "secret"})
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { handler.ServeHTTP(w, r); close(done) }()
			deadline := time.Now().Add(3 * time.Second)
			for s.snapshot().Identity == nil && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if s.snapshot().Identity == nil {
				t.Fatal("startup did not publish identity")
			}
			if source == "root" {
				cancelRoot()
			} else {
				cancelRequest()
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("cancelled request left startup active")
			}
			if w.Code != http.StatusConflict || sameServer(s.snapshot().Identity) {
				t.Fatal(w.Code, s.snapshot())
			}
		})
	}
}
