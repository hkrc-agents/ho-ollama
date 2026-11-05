//go:build harmonyos

package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCoreParseStat(t *testing.T) {
	fields := append([]string{"S", "12", "34"}, strings.Fields(strings.Repeat("0 ", 16))...)
	fields = append(fields, "987")
	ppid, pgid, ticks, err := coreParseStat("99 (odd ) worker (name)) " + strings.Join(fields, " "))
	if err != nil || ppid != 12 || pgid != 34 || ticks != 987 {
		t.Fatalf("%d %d %d %v", ppid, pgid, ticks, err)
	}
	for _, bad := range []string{"", "99 (x) S", "99 (x) Z " + strings.Join(fields[1:], " ")} {
		if _, _, _, err := coreParseStat(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCoreServerHelper(t *testing.T) {
	if os.Getenv("PICO_CORE_TEST_HELPER") != "1" {
		return
	}
	if os.Getenv("PICO_CORE_TEST_STUBBORN") == "1" {
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
	}
	if err := os.WriteFile(os.Getenv("PICO_CORE_TEST_READY"), []byte("ready"), 0600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func coreTestChild(t *testing.T, stubborn bool) (*exec.Cmd, CoreIdentity) {
	t.Helper()
	old := pidFile
	pidFile = filepath.Join(t.TempDir(), "core.pid")
	t.Cleanup(func() { pidFile = old })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(t.TempDir(), "ollama")
	if err := os.Symlink(exe, entry); err != nil {
		t.Fatal(err)
	}
	cmd := commandContext(context.Background(), entry, "-test.run=^TestCoreServerHelper$", "serve")
	ready := filepath.Join(t.TempDir(), "ready")
	cmd.Env = append(os.Environ(), "PICO_CORE_TEST_HELPER=1", "PICO_CORE_TEST_READY="+ready)
	if stubborn {
		cmd.Env = append(cmd.Env, "PICO_CORE_TEST_STUBBORN=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	id, err := CoreRecordChild(pidFile+".identity.json", cmd.Process.Pid, exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(id.PID)), 0600); err != nil {
		t.Fatal(err)
	}
	return cmd, id
}

func TestCoreIdentityAndListenerOwnership(t *testing.T) {
	_, id := coreTestChild(t, false)
	if !CoreIdentityMatches(id) || !ollamaServeProcess(id.PID) {
		t.Fatal("fresh child not recognized")
	}
	wrong := id
	wrong.StartTicks++
	if CoreIdentityMatches(wrong) {
		t.Fatal("PID reuse accepted")
	}
	wrong = id
	wrong.Executable += ".other"
	if CoreIdentityMatches(wrong) {
		t.Fatal("different executable accepted")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if CoreListenerOwned(id, l.Addr().String()) {
		t.Fatal("foreign listener accepted")
	}
	self, err := CoreProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !CoreListenerOwned(self, l.Addr().String()) {
		t.Fatal("owned listening inode not recognized")
	}
	read, err := CoreReadIdentity(pidFile + ".identity.json")
	if err != nil || read != id {
		t.Fatalf("sidecar: %+v %v", read, err)
	}
	data, _ := os.ReadFile(pidFile)
	if string(data) != strconv.Itoa(id.PID) {
		t.Fatal("upstream integer PID changed")
	}
}

func TestCoreRejectedTerminateCannotReachUpstreamKill(t *testing.T) {
	cmd, id := coreTestChild(t, false)
	id.StartTicks++
	if err := CorePersistIdentity(pidFile+".identity.json", id); err != nil {
		t.Fatal(err)
	}
	// Exercise upstream stop, including its unconditional Kill-on-error branch.
	if err := stop(cmd.Process); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("rejected target was killed:", err)
	}
	b, _ := json.Marshal(id)
	if len(b) == 0 {
		t.Fatal("identity missing")
	}
}

func TestCoreRecordedGracefulCancellation(t *testing.T) {
	cmd, _ := coreTestChild(t, false)
	start := time.Now()
	if err := stop(cmd.Process); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if time.Since(start) > 4*time.Second {
		t.Fatal("stop exceeded bound")
	}
}

func TestCoreRecordedGroupEscalation(t *testing.T) {
	cmd, _ := coreTestChild(t, true)
	start := time.Now()
	if err := stop(cmd.Process); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected bounded escalation, got %v", status)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("stop exceeded bound: %v", elapsed)
	}
}

func TestCoreCommandWaitDelayReapsUnrecordedChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := commandContext(ctx, "/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("WaitDelay failed to bound reap")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() && !cmd.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatal("child not reaped")
	}
}
