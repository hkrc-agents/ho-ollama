//go:build harmonyos

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ollama/ollama/envconfig"
)

func (c *harmonyCore) selectedModelRoot() (string, error) {
	root := envconfig.Models()
	if c.database != nil {
		settings, err := c.database.Settings()
		if err != nil {
			return "", err
		}
		root = settings.Models
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("model root must be absolute")
	}
	root = filepath.Clean(root)
	if err := checkModelRootAuthorization(root); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("selected model root unavailable; no fallback: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("selected model root is not a directory")
	}
	return root, nil
}

// The HAP restores policies before starting the Go APP. Actual filesystem
// readability (including a temporary grant) does not establish persistence.
// A receipt is a negative gate, never a substitute for OS authorization or I/O.
func checkModelRootAuthorization(root string) error {
	privateRoot, err := privateStorageRoot()
	if err != nil {
		return err
	}
	if root == privateRoot {
		return nil
	}
	path := filepath.Join(filepath.Dir(privateRoot), "model-directory-active-restore.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		// Legacy installations have no receipt. A same-session migration may
		// also select a new root absent from the startup receipt.
		return nil
	}
	if err != nil {
		return fmt.Errorf("model authorization restore receipt unavailable; no fallback: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16384 {
		return errors.New("invalid model authorization restore receipt; no fallback")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return errors.New("model authorization restore receipt unreadable; no fallback")
	}
	var receipt struct {
		Schema   int     `json:"schema"`
		OldRoot  *string `json:"old_root"`
		NewRoot  *string `json:"new_root"`
		OldError *string `json:"old_error"`
		NewError *string `json:"new_error"`
		Error    *string `json:"error"`
	}
	if json.Unmarshal(data, &receipt) != nil || receipt.Schema != 1 ||
		receipt.OldRoot == nil || receipt.NewRoot == nil || receipt.OldError == nil || receipt.NewError == nil || receipt.Error == nil {
		return errors.New("invalid model authorization restore receipt; no fallback")
	}
	for _, path := range []string{*receipt.OldRoot, *receipt.NewRoot} {
		if path != "" && (!filepath.IsAbs(path) || strings.ContainsRune(path, 0)) {
			return errors.New("invalid model authorization restore root; no fallback")
		}
	}
	if (*receipt.OldError != "" && *receipt.OldRoot == "") || (*receipt.NewError != "" && *receipt.NewRoot == "") {
		return errors.New("unbound model authorization restore error; no fallback")
	}
	if *receipt.Error != "" ||
		(*receipt.OldRoot != "" && filepath.Clean(*receipt.OldRoot) == root && *receipt.OldError != "") ||
		(*receipt.NewRoot != "" && filepath.Clean(*receipt.NewRoot) == root && *receipt.NewError != "") {
		return errors.New("saved model library authorization could not be restored; reauthorize and restart; no fallback")
	}
	return nil
}

func (c *harmonyCore) activeModelRoot() (string, error) {
	root, err := c.selectedModelRoot()
	if err != nil {
		return "", err
	}
	s := c.Snapshot()
	if s.State == "ready" && s.ModelRoot != "" && s.ModelRoot != root {
		return "", errors.New("selected model root differs from running core; restart or recover migration first")
	}
	return root, nil
}
