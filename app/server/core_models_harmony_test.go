//go:build harmonyos

package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarmonyModelRootNeverFallsBack(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", filepath.Join(root, "missing"), file} {
		if value, err := platformModelsDirectory(path); err == nil || value != "" {
			t.Fatalf("invalid path %q silently accepted: %q %v", path, value, err)
		}
	}
	if value, err := platformModelsDirectory(root); err != nil || value != root {
		t.Fatal(value, err)
	}
}
