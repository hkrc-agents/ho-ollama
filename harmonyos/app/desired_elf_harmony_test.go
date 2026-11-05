//go:build harmonyos

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestElfIntentBoundToLiveHapSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PICO_HAP_SESSION", "session-a")
	if err := os.Mkdir(filepath.Join(home, "desktop"), 0700); err != nil {
		t.Fatal(err)
	}
	if wanted, err := harmonyCoreShouldStart(); err != nil || !wanted {
		t.Fatal(wanted, err)
	}
	if err := recordHarmonyServiceIntent("ollama", "stop", serverConfig{}); err != nil {
		t.Fatal(err)
	}
	if wanted, err := harmonyCoreShouldStart(); err != nil || wanted {
		t.Fatal(wanted, err)
	}
	config := serverConfig{Model: "/owned/model.gguf", Port: 18482, Context: 128, Threads: 2, MemoryMiB: 1536}
	if err := recordHarmonyServiceIntent("llama", "start", config); err != nil {
		t.Fatal(err)
	}
	value, err := readElfIntent()
	if err != nil || !value.Standalone || value.Core || value.Config != config {
		t.Fatalf("%+v %v", value, err)
	}
	if err := recordHarmonyServiceIntent("llama", "stop", serverConfig{}); err != nil {
		t.Fatal(err)
	}
	value, err = readElfIntent()
	if err != nil || value.Standalone || value.Config != config {
		t.Fatalf("%+v %v", value, err)
	}
	t.Setenv("PICO_HAP_SESSION", "session-b")
	value, err = readElfIntent()
	if err != nil || !value.Core || value.Standalone {
		t.Fatalf("replayed old session: %+v %v", value, err)
	}
}

func TestElfIntentCorruptionBlocksRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PICO_HAP_SESSION", "session-a")
	if err := os.Mkdir(filepath.Join(home, "desktop"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "desktop", "elf-service-intent.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := harmonyCoreShouldStart(); err == nil {
		t.Fatal("corrupt intent accepted")
	}
}
