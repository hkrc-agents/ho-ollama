//go:build harmonyos

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// Both managers must finish, even when the first fails. A receipt is authority
// only for this exact APP instance and only after both confirmed cleanup.
func completeHarmonyShutdown(standalone, core, receipt func() error) error {
	if err := errors.Join(standalone(), core()); err != nil {
		return err
	}
	return receipt()
}

// A timed-out drain is not completion. The host retries without acknowledging
// exit, and request-scoped exec.Cmd workers must finish before handlers return.
func finishHarmonyHTTPShutdown(server *http.Server, timeout time.Duration, receipt func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return err
	}
	return receipt()
}

// Accepted migrations can outlive their HTTP 202 response. Wait for their
// cancellation/rollback and any service restoration before the final stops;
// otherwise a late rollback could restart a service after its shutdown ACK.
func finishHarmonyStorageShutdown(gate *sync.RWMutex, finish func() error) error {
	gate.Lock()
	defer gate.Unlock()
	return finish()
}

func writeHarmonyShutdownReceipt(directory string, identity serverIdentity) error {
	if identity.PID <= 1 || identity.StartTicks == "" {
		return errors.New("missing APP shutdown identity")
	}
	return atomicPrivateBytes(filepath.Join(directory, "shutdown-complete"),
		[]byte(fmt.Sprintf("%d %s\n", identity.PID, identity.StartTicks)))
}
