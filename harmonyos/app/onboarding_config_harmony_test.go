//go:build harmonyos

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/internal/onboarding"
)

func TestHarmonyWelcomePreservesConfigAndUsesMarker(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	if got := onboarding.AppDatabasePath(); got != filepath.Join(home, ".ollama", "db.sqlite") {
		t.Fatalf("unexpected HarmonyOS app database path: %q", got)
	}
	if err := save(&config{LastModel: "preserved-model"}); err != nil {
		t.Fatal(err)
	}
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if needed, err := NeedsWelcome(); err != nil || !needed {
		t.Fatalf("fresh welcome: needed=%v err=%v", needed, err)
	}
	if err := CompleteWelcome(); err != nil {
		t.Fatal(err)
	}
	if completed, err := (onboarding.State{}).Completed(); err != nil || !completed {
		t.Fatalf("completion marker: completed=%v err=%v", completed, err)
	}
	if needed, err := NeedsWelcome(); err != nil || needed {
		t.Fatalf("completed welcome: needed=%v err=%v", needed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("welcome rewrote user config: err=%v", err)
	}
	if _, err := os.Stat(onboarding.AppDatabasePath()); !os.IsNotExist(err) {
		t.Fatalf("CLI welcome must not create the APP database: %v", err)
	}
}
