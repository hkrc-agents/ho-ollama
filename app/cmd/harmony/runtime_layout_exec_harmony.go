//go:build harmonyos

package main

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// Existing executable builds retain their previous paths/identity/context.
// Native service mode provides separate implementations, never runtime fallback.
func harmonyRuntimeLayout(exe string) (harmonyRuntimePaths, error) {
	bin := filepath.Dir(exe)
	server := filepath.Clean(filepath.Join(bin, "..", "lib", "ollama", "llama-server"))
	return harmonyRuntimePaths{BinRoot: bin, Server: server, Components: map[string]string{
		"ollama-app": exe, "ollama": filepath.Join(bin, "ollama"), "llama-server": server,
		"pico-service-launcher": filepath.Join(bin, "pico-service-launcher"), "pico-storage-probe": filepath.Join(bin, "pico-storage-probe"),
	}}, nil
}
func harmonySelfShutdownIdentity(exe string) (serverIdentity, error) {
	return serverProcess(os.Getpid(), exe)
}
func harmonyAppContext(parent context.Context) (context.Context, context.CancelFunc) {
	return executableAppContext(parent)
}
func configureHarmonyCore(c *harmonyCore) *harmonyCore { return c }
func configureHarmonyServer(*harmonyServer)            {}
func harmonyManagedInspect(context.Context, string, string) (harmonyModelFile, bool, error) {
	return harmonyModelFile{}, false, nil
}
func harmonyManagedStorageProbe(context.Context, int, unix.Stat_t, string) (*storageNativeResult, string, bool, error) {
	return nil, "", false, nil
}
