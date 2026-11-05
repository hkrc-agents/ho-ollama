//go:build harmonyos && !pico_native_services

package main

import (
	"context"
	"reflect"
	"testing"
)

func TestExecutableRuntimeLayoutUnchanged(t *testing.T) {
	paths, err := harmonyRuntimeLayout("/example/bin/ollama-app")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ollama-app": "/example/bin/ollama-app", "ollama": "/example/bin/ollama", "llama-server": "/example/lib/ollama/llama-server",
		"pico-service-launcher": "/example/bin/pico-service-launcher", "pico-storage-probe": "/example/bin/pico-storage-probe"}
	if paths.BinRoot != "/example/bin" || paths.Server != want["llama-server"] || !reflect.DeepEqual(paths.Components, want) {
		t.Fatal(paths)
	}
	ctx, cancel := harmonyAppContext(context.Background())
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("default lifetime cancellation lost")
	}
}
