//go:build harmonyos

package main

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitSupervised(t *testing.T, s *harmonyServer, want func(serverSnapshot) bool) serverSnapshot {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := s.snapshot()
		if want(snapshot) {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("supervision state not reached", s.snapshot())
	return serverSnapshot{}
}

func TestStandaloneCrashRestartsOwnedChild(t *testing.T) {
	s, c := fixtureServer(t)
	s.restartDelay = 5 * time.Millisecond
	if err := s.start(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	old := s.snapshot().Identity.PID
	if err := s.signalChild(s.snapshot().Identity, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	next := waitSupervised(t, s, func(v serverSnapshot) bool { return v.State == "ready" && v.Identity.PID != old })
	if next.Restarts != 1 || !next.DesiredRunning {
		t.Fatal(next)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if now := s.snapshot(); now.State != "stopped" || now.DesiredRunning || now.Identity != nil {
		t.Fatal("manual stop restarted", now)
	}
}

func TestStandaloneStopCancelsPendingRestart(t *testing.T) {
	s, c := fixtureServer(t)
	s.restartDelay = 100 * time.Millisecond
	if err := s.start(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := s.signalChild(s.snapshot().Identity, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitSupervised(t, s, func(v serverSnapshot) bool { return v.State == "failed" })
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if v := s.snapshot(); v.State != "stopped" || v.Identity != nil {
		t.Fatal("stop raced restart", v)
	}
}

func TestStandaloneCrashLoopBounded(t *testing.T) {
	s, c := fixtureServer(t)
	s.restartDelay = 5 * time.Millisecond
	s.restartLimit = 2
	if err := s.start(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		old := s.snapshot().Identity.PID
		if err := s.signalChild(s.snapshot().Identity, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if n < 2 {
			waitSupervised(t, s, func(v serverSnapshot) bool { return v.State == "ready" && v.Identity.PID != old })
		}
	}
	waitSupervised(t, s, func(v serverSnapshot) bool { return strings.Contains(v.Error, "restart limit") })
	if v := s.snapshot(); v.Restarts != 2 || !v.DesiredRunning {
		t.Fatal(v)
	}
	if err := s.start(t.Context(), c); err != nil {
		t.Fatal("manual recovery rejected", err)
	}
	if s.snapshot().Restarts != 0 {
		t.Fatal("manual start did not reset budget")
	}
}

func TestStandaloneLifetimeCancelPreventsRestart(t *testing.T) {
	s, c := fixtureServer(t)
	s.restartDelay = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	s.lifetime = ctx
	if err := s.start(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.signalChild(s.snapshot().Identity, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitSupervised(t, s, func(v serverSnapshot) bool { return v.State == "failed" })
	time.Sleep(150 * time.Millisecond)
	if s.snapshot().Restarts != 0 {
		t.Fatal("host cancellation restarted child")
	}
}
