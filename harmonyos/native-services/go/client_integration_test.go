//go:build harmonyos

package nativeprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This is actual Go-to-C++ TCP interoperability with a model platform backend.
// It is NOT evidence of HarmonyOS API creation or model inference.
func TestCPPControlInteroperability(t *testing.T) {
	path := os.Getenv("PICO_NATIVE_CONTROL_HOST_FIXTURE")
	if path == "" {
		t.Skip("explicit host-only C++ fixture not supplied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// Cleanup runs LIFO: let the fixture consume quit and exit before cancelling
	// the CommandContext; a function defer would kill it before t.Cleanup.
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fmt.Fprintln(input, "quit")
		input.Close()
		if err := command.Wait(); err != nil {
			t.Error("C++ fixture cleanup", err)
		}
	})
	var config Config
	if err = json.NewDecoder(output).Decode(&config); err != nil {
		t.Fatal(err)
	}
	app, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	core, err := app.Start(ctx, Core, json.RawMessage(`{"argv":["serve"]}`))
	if err != nil {
		t.Fatal(err)
	}
	coreConfig := config
	coreConfig.Instance = core
	coreClient, err := New(coreConfig)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := coreClient.Start(ctx, Runner, json.RawMessage(`{"argv":["--model","host-only.gguf"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coreClient.Status(ctx, config.Instance); err == nil {
		t.Fatal("core managed its APP owner")
	}
	if _, err = fmt.Fprintf(input, "crash %d\n", core); err != nil {
		t.Fatal(err)
	}
	status, err := app.WaitTree(ctx, core)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Record.ExitSignalKnown || status.Record.ExitSignal != 11 {
		t.Fatal("lost official mock exit metadata", status)
	}
	oldRunner, err := app.Status(ctx, runner)
	if err != nil || !oldRunner.Drained {
		t.Fatal("old runner not drained", oldRunner, err)
	}
	replacement, err := app.Replace(ctx, core)
	if err != nil || replacement == core {
		t.Fatal(replacement, err)
	}
	if _, err = app.Replace(ctx, core); err == nil {
		t.Fatal("old generation reused twice")
	}
	if err = app.CancelIntent(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Status(ctx, replacement); err != nil {
		t.Fatal("cannot observe draining instance", err)
	}
	if err = app.AbortTree(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = app.WaitStopped(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Replace(ctx, replacement); err == nil {
		t.Fatal("cancelled intent restarted")
	}
}
