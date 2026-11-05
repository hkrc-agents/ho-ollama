//go:build harmonyos && pico_native_services

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ollama/ollama/cmd"
	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

var entered atomic.Bool

//export PicoServiceRun
func PicoServiceRun(input *C.char) C.int {
	if !entered.CompareAndSwap(false, true) || input == nil {
		return 125
	}
	_, launch, client, err := nativeprocess.DecodeBootstrap(C.GoString(input))
	if err != nil {
		fmt.Fprintln(os.Stderr, "managed core bootstrap rejected")
		return 125
	}
	if launch.Kind != "core" {
		return 125
	}
	if err = launch.ConfigureEnvironment(); err != nil {
		return 125
	}
	if err = nativeprocess.InstallCurrent(client, launch); err != nil {
		return 125
	}
	registration, stop := context.WithTimeout(context.Background(), 15*time.Second)
	err = client.AwaitRegistration(registration)
	stop()
	if err != nil {
		return 125
	}
	ctx, cancel := client.WatchIntent(context.Background())
	defer cancel()
	// Upstream Serve owns graceful HTTP/scheduler shutdown through SIGTERM.
	// Native registry intent, not a cross-process PID signal, triggers SELF.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
	}()
	command := cmd.NewCLI()
	command.SetArgs(launch.Args[1:])
	if err = command.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func main() {}
