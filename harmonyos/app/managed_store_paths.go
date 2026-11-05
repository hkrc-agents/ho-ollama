//go:build harmonyos && pico_native_services

package store

import "path/filepath"

// Once per official SO entry, before a Store is constructed.
func HarmonyManagedConfigurePaths(home string) {
	defaultDBPath = filepath.Join(home, ".ollama", "db.sqlite")
	legacyConfigPath = filepath.Join(home, ".ollama", "config.json")
}
