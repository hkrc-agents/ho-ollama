//go:build harmonyos && !pico_native_services

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var parentLeaseLost atomic.Bool

func harmonyWriteShutdownReceipt(directory string, identity serverIdentity) error {
	err := writeHarmonyShutdownReceipt(directory, identity)
	if err != nil && parentLeaseLost.Load() && identity.PID > 1 && identity.StartTicks != "" {
		// Called only after HTTP/storage and both service managers completed.
		// A dead host cannot consume ACKs; disk-full/uninstall must not keep
		// an otherwise drained APP alive forever. Normal Quit remains strict.
		slog.Warn("HAP lease lost; drained APP exiting without persisted ACK", "error", err)
		return nil
	}
	return err
}

// The HAP alone retains the write endpoint. Read ownership transfers here;
// CloseOnExec prevents model workers/services from keeping this endpoint.
// This is cooperative shutdown, never a claim of kernel tree termination.
func watchParentLifeline(parent context.Context, file *os.File, report func(error)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		defer file.Close()
		var b [1]byte
		n, err := file.Read(b[:])
		if errors.Is(err, io.EOF) {
			report(nil)
		} else if ctx.Err() == nil {
			report(fmt.Errorf("parent lifeline protocol: bytes=%d error=%v", n, err))
		}
	}()
	go func() { <-ctx.Done(); file.Close() }()
	return ctx, cancel
}

func executableAppContext(parent context.Context) (context.Context, context.CancelFunc) {
	value, present := os.LookupEnv("PICO_PARENT_LIFELINE_FD")
	if !present {
		// Compatibility for the previously installed host and standalone tests.
		return context.WithCancel(parent)
	}
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		panic("invalid PICO_PARENT_LIFELINE_FD")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		panic(fmt.Errorf("APP subreaper required: %w", err))
	}
	var subreaper int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&subreaper)), 0, 0, 0); err != nil || subreaper != 1 {
		panic(fmt.Errorf("APP subreaper readback: value=%d error=%v", subreaper, err))
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		panic("parent lifeline must be an inherited pipe")
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		panic(fmt.Errorf("parent lifeline nonblocking mode: %w", err))
	}
	os.Unsetenv("PICO_PARENT_LIFELINE_FD")
	file := os.NewFile(uintptr(fd), "hap-lifeline")
	return watchParentLifeline(parent, file, func(err error) {
		if err == nil {
			parentLeaseLost.Store(true)
		}
		if err != nil {
			slog.Error("HAP lifeline failed; draining APP", "error", err)
		} else {
			slog.Info("HAP lifeline closed; draining APP")
		}
	})
}
