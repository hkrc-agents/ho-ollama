//go:build harmonyos

package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const platformStrictModelDirectory = true

// Do not silently launch against a different library when a selected directory
// loses authorization or disappears. Existing private roots are provisioned by HAP.
func platformModelsDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("model directory must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("selected model directory unavailable (no fallback): %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("selected model directory is not a directory")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err = f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return filepath.Clean(path), nil
}
