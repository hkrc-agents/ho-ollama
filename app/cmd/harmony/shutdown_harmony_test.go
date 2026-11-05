//go:build harmonyos

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownRequiresBothManagersAndReceipt(t *testing.T) {
	for _, failed := range []string{"", "standalone", "core", "receipt"} {
		t.Run(failed, func(t *testing.T) {
			var calls []string
			operation := func(name string) func() error {
				return func() error {
					calls = append(calls, name)
					if failed == name {
						return errors.New(name)
					}
					return nil
				}
			}
			err := completeHarmonyShutdown(operation("standalone"), operation("core"), operation("receipt"))
			if (err != nil) != (failed != "") {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(calls) < 2 || calls[0] != "standalone" || calls[1] != "core" {
				t.Fatal(calls)
			}
			if (len(calls) == 3) != (failed == "" || failed == "receipt") {
				t.Fatal(calls)
			}
		})
	}
}

func TestShutdownWaitsForDetachedStorageJobs(t *testing.T) {
	var gate sync.RWMutex
	var finished atomic.Bool
	gate.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(gate.Unlock) }
	defer unlock()
	done := make(chan error, 1)
	go func() {
		done <- finishHarmonyStorageShutdown(&gate, func() error {
			if !finished.Load() {
				return errors.New("storage rollback still active")
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		t.Fatal("cleanup ran before detached migration completed", err)
	case <-time.After(20 * time.Millisecond):
	}
	finished.Store(true)
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestShutdownWaitsForCancelledHTTPWorkersBeforeReceipt(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, cancelled, released, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		<-released // Simulates a worker still being reaped after cancellation.
	}))
	server.Config.BaseContext = func(net.Listener) context.Context { return life }
	server.Start()
	defer func() { cancel(); release(); server.Close() }()
	go func() {
		defer close(clientDone)
		if response, err := http.Get(server.URL); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("request did not inherit host cancellation")
	}
	ack := false
	receipt := func() error { ack = true; return nil }
	if err := finishHarmonyHTTPShutdown(server.Config, 20*time.Millisecond, receipt); err == nil || ack {
		t.Fatal("unreaped HTTP worker acknowledged as stopped", err)
	}
	release()
	if err := finishHarmonyHTTPShutdown(server.Config, time.Second, receipt); err != nil || !ack {
		t.Fatal(err)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client did not complete")
	}
}

func TestShutdownRetryDoesNotAcknowledgeFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "shutdown-complete")
	failed := true
	receipt := func() error {
		return writeHarmonyShutdownReceipt(directory, serverIdentity{PID: 42, StartTicks: "123"})
	}
	standalone := func() error {
		if failed {
			return errors.New("ownership unknown")
		}
		return nil
	}
	core := func() error { return nil }
	if completeHarmonyShutdown(standalone, core, receipt) == nil {
		t.Fatal("failure accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failure produced receipt", err)
	}
	failed = false
	if err := completeHarmonyShutdown(standalone, core, receipt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "42 123\n" {
		t.Fatal(string(b), err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if err := writeHarmonyShutdownReceipt(filepath.Join(directory, "absent"), serverIdentity{PID: 42, StartTicks: "123"}); err == nil {
		t.Fatal("write failure ignored")
	}
	if err := writeHarmonyShutdownReceipt(directory, serverIdentity{}); err == nil {
		t.Fatal("invalid identity accepted")
	}
}
