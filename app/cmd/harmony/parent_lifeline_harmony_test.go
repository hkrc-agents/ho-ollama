//go:build harmonyos

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLostParentDoesNotRequireAckFileAfterServiceDrain(t *testing.T) {
	previous := parentLeaseLost.Load()
	defer parentLeaseLost.Store(previous)
	missing := t.TempDir() + "/removed"
	identity := serverIdentity{PID: 123, StartTicks: "456"} // In-memory receipt fixture, not a real actor.
	parentLeaseLost.Store(false)
	if err := harmonyWriteShutdownReceipt(missing, identity); err == nil {
		t.Fatal("normal Quit accepted missing ACK")
	}
	parentLeaseLost.Store(true)
	if err := harmonyWriteShutdownReceipt(missing, identity); err != nil {
		t.Fatal("dead host held drained APP alive", err)
	}
	if err := harmonyWriteShutdownReceipt(missing, serverIdentity{}); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestParentLifeline(t *testing.T) {
	for _, mode := range []string{"eof", "protocol-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			reports := make(chan error, 1)
			ctx, cancel := watchParentLifeline(context.Background(), r, func(e error) { reports <- e })
			defer cancel()
			select {
			case <-ctx.Done():
				t.Fatal("cancelled while parent alive")
			default:
			}
			switch mode {
			case "eof":
				w.Close()
			case "protocol-error":
				if _, err := w.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("watch did not stop")
			}
			if mode != "cancel" {
				select {
				case err := <-reports:
					if (err != nil) != (mode == "protocol-error") {
						t.Fatalf("unexpected report %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("missing report")
				}
			}
		})
	}
}
