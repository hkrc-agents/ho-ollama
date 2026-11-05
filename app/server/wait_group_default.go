//go:build !harmonyos || pico_native_services

package server

import "os/exec"

func platformCoreWait(cmd *exec.Cmd) error { return cmd.Wait() }
func platformCoreMayRestart(error) bool    { return true }
