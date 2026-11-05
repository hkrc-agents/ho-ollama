//go:build harmonyos && pico_native_services

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	core "github.com/ollama/ollama/app/server"
	"github.com/ollama/ollama/app/store"
	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

var managedAppEntered atomic.Bool

//export PicoServiceRun
func PicoServiceRun(input *C.char) C.int {
	if input == nil || !managedAppEntered.CompareAndSwap(false, true) {
		return 125
	}
	_, launch, client, err := nativeprocess.DecodeBootstrap(C.GoString(input))
	if err != nil || (launch.Kind != "app" && launch.Kind != "model-store") {
		return 125
	}
	if err = launch.ConfigureEnvironment(); err != nil {
		return 125
	}
	if err = nativeprocess.InstallCurrent(client, launch); err != nil {
		return 125
	}
	check, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = client.AwaitRegistration(check)
	cancel()
	if err != nil {
		return 125
	}
	if launch.Kind == "app" {
		if err = harmonyManagedStartupCheck(launch); err != nil {
			fmt.Fprintln(os.Stderr, "managed startup check:", err)
			return 125
		}
	}
	// Imported packages snapshot HOME during init, before the exported entry.
	// Repair only explicit platform paths before constructing a store/server.
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) {
		return 125
	}
	store.HarmonyManagedConfigurePaths(home)
	core.HarmonyManagedConfigurePaths(home)
	if launch.Kind == "model-store" {
		if len(launch.Args) != 3 {
			return 125
		}
		if err = harmonyStoreWorker(launch.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	main()
	// Normal main returns only after precise HTTP/storage/both-service ACKs.
	return 0
}

func harmonyRuntimeLayout(_ string) (harmonyRuntimePaths, error) {
	root := os.Getenv("PICO_NATIVE_LIB_DIR")
	bin := os.Getenv("PICO_COMPONENT_BIN_DIR")
	if !filepath.IsAbs(root) || !filepath.IsAbs(bin) {
		return harmonyRuntimePaths{}, errors.New("explicit installed SO/bin roots required")
	}
	paths := map[string]string{"ollama-app": filepath.Join(root, "libpico_ollama_app.so"), "ollama-core": filepath.Join(root, "libpico_ollama_core.so"),
		"llama-server": filepath.Join(root, "libpico_llama_server.so"), "native-entry": filepath.Join(root, "libpico_go_child.so"), "ollama": filepath.Join(bin, "ollama")}
	return harmonyRuntimePaths{BinRoot: bin, Server: paths["llama-server"], Components: paths}, nil
}

func harmonySelfShutdownIdentity(exe string) (serverIdentity, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return serverIdentity{}, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return serverIdentity{}, errors.New("invalid self stat")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 {
		return serverIdentity{}, errors.New("short self stat")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || ticks == 0 {
		return serverIdentity{}, errors.New("invalid self start ticks")
	}
	return serverIdentity{PID: os.Getpid(), UID: os.Getuid(), StartTicks: fields[19], Executable: exe}, nil
}

func harmonyAppContext(parent context.Context) (context.Context, context.CancelFunc) {
	client, _, err := nativeprocess.Current()
	if err != nil {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	return client.WatchIntent(parent)
}
