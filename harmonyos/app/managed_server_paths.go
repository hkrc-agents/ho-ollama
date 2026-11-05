//go:build harmonyos && pico_native_services

package server

import "path/filepath"

// Existing executable supervisor is compiled but never selected by the native
// core controller. HOME-derived paths still serve log/inventory UI helpers.
func HarmonyManagedConfigurePaths(home string) {
	pidFile = filepath.Join(home, ".ollama", "desktop-server.pid")
	serverLogPath = filepath.Join(home, ".ollama", "logs", "server.log")
}
