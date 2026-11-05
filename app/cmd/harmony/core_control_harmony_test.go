//go:build harmonyos

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/ollama/ollama/app/server"
)

func coreTestControl(t *testing.T) *harmonyCore {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	t.Setenv("OLLAMA_HOST", address)
	c := newHarmonyCore(nil)
	c.pidPath = filepath.Join(t.TempDir(), "core.pid")
	c.executable = "/test/ollama"
	c.poll = time.Millisecond
	c.healthPoll = time.Millisecond
	c.startTimeout = 100 * time.Millisecond
	c.stopTimeout = 100 * time.Millisecond
	c.run = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	c.observe = func(string) (core.CoreIdentity, error) {
		return core.CoreIdentity{PID: 123, PGID: 123, StartTicks: 42, Executable: "/test/ollama"}, nil
	}
	c.ready = func(context.Context, core.CoreIdentity, *url.URL) bool { return true }
	t.Cleanup(func() { _ = c.Stop() })
	return c
}

func TestHarmonyCoreSerializedLifecycle(t *testing.T) {
	c := coreTestControl(t)
	var starts atomic.Int32
	c.run = func(ctx context.Context) error { starts.Add(1); <-ctx.Done(); return ctx.Err() }
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := c.Start(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("duplicate starts: %d", starts.Load())
	}
	s := c.Snapshot()
	if s.State != "ready" || s.PID != 123 || s.StartTicks != 42 {
		t.Fatalf("snapshot: %+v", s)
	}
	b, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(b), `"start_ticks":42`) {
		t.Fatalf("JSON: %s %v", b, err)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().State != "stopped" {
		t.Fatal(c.Snapshot())
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 {
		t.Fatalf("restart count: %d", starts.Load())
	}
}

func TestHarmonyCorePortConflictDoesNotLaunch(t *testing.T) {
	c := coreTestControl(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Setenv("OLLAMA_HOST", l.Addr().String())
	c.run = func(context.Context) error { t.Error("launched despite conflict"); return nil }
	if err := c.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "port unavailable") {
		t.Fatalf("got %v", err)
	}
	if c.Snapshot().State != "failed" {
		t.Fatal(c.Snapshot())
	}
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal("foreign listener changed:", err)
	}
	conn.Close()
}

func TestHarmonyCoreReadinessRequiresOwnedInstance(t *testing.T) {
	c := coreTestControl(t)
	c.ready = func(context.Context, core.CoreIdentity, *url.URL) bool { return false }
	if err := c.Start(t.Context()); err == nil {
		t.Fatal("ready without owned listener")
	}
	if c.Snapshot().State != "failed" {
		t.Fatal(c.Snapshot())
	}
	select {
	case <-c.done:
	default:
		t.Fatal("startup failure did not reap supervisor")
	}
}

func TestHarmonyCoreUnexpectedExit(t *testing.T) {
	c := coreTestControl(t)
	c.run = func(context.Context) error { return errors.New("injected exit") }
	if err := c.Start(t.Context()); err == nil || err.Error() != "injected exit" {
		t.Fatalf("got %v", err)
	}
	if c.Snapshot().State != "failed" {
		t.Fatal(c.Snapshot())
	}
}

func TestHarmonyCoreIdentityLossCancels(t *testing.T) {
	c := coreTestControl(t)
	var lost atomic.Bool
	observe := c.observe
	c.observe = func(exe string) (core.CoreIdentity, error) {
		if lost.Load() {
			return core.CoreIdentity{}, errors.New("start ticks changed")
		}
		return observe(exe)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	lost.Store(true)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("identity loss did not cancel")
	}
	if s := c.Snapshot(); s.State != "failed" || !strings.Contains(s.Error, "instance lost") {
		t.Fatal(s)
	}
}

func TestHarmonyCoreConfirmedExitAllowsUpstreamRestart(t *testing.T) {
	c := coreTestControl(t)
	var phase atomic.Int32
	var runs atomic.Int32
	c.run = func(ctx context.Context) error { runs.Add(1); <-ctx.Done(); return ctx.Err() }
	c.observe = func(string) (core.CoreIdentity, error) {
		if phase.Load() == 1 {
			return core.CoreIdentity{}, os.ErrNotExist
		}
		pid := 123
		if phase.Load() == 2 {
			pid = 124
		}
		return core.CoreIdentity{PID: pid, PGID: pid, StartTicks: 42, Executable: "/test/ollama"}, nil
	}
	c.gone = func(core.CoreIdentity) (bool, error) { return phase.Load() > 0, nil }
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	deadline := time.Now().Add(time.Second)
	for c.Snapshot().State != "starting" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Snapshot().State != "starting" {
		t.Fatal("exit gap cancelled supervisor", c.Snapshot())
	}
	phase.Store(2)
	for c.Snapshot().PID != 124 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := c.Snapshot(); s.State != "ready" || s.PID != 124 || s.Restarts != 1 || runs.Load() != 1 {
		t.Fatal("upstream replacement not accepted, or second supervisor started", s, runs.Load())
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestHarmonyCoreLiveReplacementStillRejected(t *testing.T) {
	c := coreTestControl(t)
	var replace atomic.Bool
	c.observe = func(string) (core.CoreIdentity, error) {
		pid := 123
		if replace.Load() {
			pid = 124
		}
		return core.CoreIdentity{PID: pid, PGID: pid, StartTicks: 42, Executable: "/test/ollama"}, nil
	}
	c.gone = func(core.CoreIdentity) (bool, error) { return false, nil }
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	replace.Store(true)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("live replacement not rejected")
	}
	if c.Snapshot().State != "failed" {
		t.Fatal(c.Snapshot())
	}
}

func TestHarmonyCoreBoundedStopBlocksReplacement(t *testing.T) {
	c := coreTestControl(t)
	release := make(chan struct{})
	c.run = func(context.Context) error { <-release; return nil }
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(); err == nil {
		t.Fatal("unbounded/stuck supervisor reported stopped")
	}
	if err := c.Start(t.Context()); err == nil {
		t.Fatal("replacement while supervisor active")
	}
	close(release)
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("supervisor not released")
	}
}

func TestHarmonyCoreParentCancellation(t *testing.T) {
	c := coreTestControl(t)
	ctx, cancel := context.WithCancel(t.Context())
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("lifetime context ignored")
	}
	if c.Snapshot().State != "stopped" {
		t.Fatal(c.Snapshot())
	}
}

func TestHarmonyCoreExclusiveSupervisor(t *testing.T) {
	c := coreTestControl(t)
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	other := coreTestControl(t)
	other.pidPath = c.pidPath
	if err := other.Start(t.Context()); err == nil {
		t.Fatal("two supervisors acquired the same PID record")
	}
}

func TestHarmonyCoreAtomicRestart(t *testing.T) {
	c := coreTestControl(t)
	var starts, active, peak atomic.Int32
	c.run = func(ctx context.Context) error {
		starts.Add(1)
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		defer active.Add(-1)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := c.Restart(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 5 || peak.Load() != 1 || c.Snapshot().State != "ready" {
		t.Fatalf("starts=%d peak=%d snapshot=%+v", starts.Load(), peak.Load(), c.Snapshot())
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Restart(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled restart: %v", err)
	}
	if c.Snapshot().State != "ready" {
		t.Fatal("cancelled restart stopped a healthy core")
	}
}
