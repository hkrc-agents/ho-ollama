//go:build harmonyos && pico_native_services && pico_native_diagnostic

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
	"golang.org/x/sys/unix"
)

// Isolated build-only startup check. It exercises the actual Go→HAP→native SO
// storage worker using a private directory, not fabricated Picker authorization.
func harmonyManagedStartupCheck(launch nativeprocess.Launch) error {
	if os.Getenv("PICO_NATIVE_SELF_TEST") != "1" {
		return nil
	}
	directory, err := os.MkdirTemp(launch.Directory, "storage-selftest-")
	if err != nil {
		return err
	}
	fd, err := unix.Open(directory, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		return err
	}
	domain, err := storageDomain()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, stderr, handled, err := harmonyManagedStorageProbe(ctx, fd, info, domain)
	text, encode := json.Marshal(map[string]any{"handled": handled, "native": result, "error": func() string {
		if err != nil {
			return err.Error()
		}
		return ""
	}(), "stderr": stderr, "scope": "private reopen; not persistent Picker authorization"})
	if encode != nil {
		return encode
	}
	write := os.WriteFile(filepath.Join(launch.Directory, "storage-selftest.json"), text, 0600)
	return errors.Join(err, write)
}
