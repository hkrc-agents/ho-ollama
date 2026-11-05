//go:build harmonyos && !pico_native_services

package main

import (
	"errors"
	core "github.com/ollama/ollama/app/server"
	"os/exec"
)

func waitExecutableService(cmd *exec.Cmd) error { return core.WaitServiceCommand(cmd) }
func executableDrainBlocked(err error) bool     { return errors.Is(err, core.ErrServiceTreeNotDrained) }
