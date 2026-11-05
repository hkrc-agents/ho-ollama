//go:build harmonyos

package updater

import "testing"

func TestHarmonyDoesNotOfferDesktopInstaller(t *testing.T) {
	if Installer != "" {
		t.Fatalf("HarmonyOS must not advertise a Windows/macOS installer: %q", Installer)
	}
	previous := UpdateDownloaded
	t.Cleanup(func() { UpdateDownloaded = previous })
	UpdateDownloaded = true
	if IsUpdatePending() {
		t.Fatal("a mocked desktop download must not enable HarmonyOS app replacement")
	}
}
