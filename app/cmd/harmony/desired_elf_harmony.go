//go:build harmonyos

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var elfIntentMutex sync.Mutex

type elfServiceIntent struct {
	Session    string       `json:"session"`
	Core       bool         `json:"core"`
	Standalone bool         `json:"standalone"`
	Config     serverConfig `json:"config"`
}

func readElfIntent() (elfServiceIntent, error) {
	session := os.Getenv("PICO_HAP_SESSION")
	value := elfServiceIntent{Session: session, Core: true}
	if session == "" {
		return value, nil
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "desktop", "elf-service-intent.json"))
	if os.IsNotExist(err) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	var saved elfServiceIntent
	if err = json.Unmarshal(data, &saved); err != nil {
		return value, err
	}
	if saved.Session == session {
		return saved, nil
	}
	return value, nil // No process adoption or desired-state replay across HAP sessions.
}
func recordHarmonyServiceIntent(service, action string, config serverConfig) error {
	if os.Getenv("PICO_HAP_SESSION") == "" {
		return nil
	}
	elfIntentMutex.Lock()
	defer elfIntentMutex.Unlock()
	value, err := readElfIntent()
	if err != nil {
		return err
	}
	wanted := action != "stop"
	switch service {
	case "ollama":
		value.Core = wanted
	case "llama":
		value.Standalone = wanted
		if wanted {
			value.Config = config
		}
	default:
		return errors.New("unknown service intent")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Join(os.Getenv("HOME"), "desktop")
	f, err := os.CreateTemp(dir, ".elf-intent-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "elf-service-intent.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func harmonyCoreShouldStart() (bool, error) {
	elfIntentMutex.Lock()
	defer elfIntentMutex.Unlock()
	value, err := readElfIntent()
	return value.Core, err
}
func harmonyRestoreStandalone(ctx context.Context, s *harmonyServer) error {
	elfIntentMutex.Lock()
	value, err := readElfIntent()
	elfIntentMutex.Unlock()
	if err != nil {
		return err
	}
	if value.Standalone {
		return s.start(ctx, value.Config)
	}
	return nil
}
