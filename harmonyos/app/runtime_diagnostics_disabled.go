//go:build harmonyos && pico_native_services && !pico_native_diagnostic

package main

import nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"

func harmonyManagedStartupCheck(nativeprocess.Launch) error { return nil }
