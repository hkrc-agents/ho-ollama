//go:build !harmonyos

package server

import (
	"log/slog"
	"os"
)

const platformStrictModelDirectory = false

func platformModelsDirectory(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		slog.Warn("models path not accessible, using default", "path", path, "err", err)
		return "", nil
	}
	return path, nil
}
