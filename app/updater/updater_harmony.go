//go:build harmonyos

package updater

// HarmonyOS installs signed HAP updates, not Windows/macOS installers.
// The platform launcher deliberately does not start the desktop auto-updater.
func IsUpdatePending() bool { return false }
