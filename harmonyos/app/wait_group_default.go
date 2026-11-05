//go:build !harmonyos

package server

import "os/exec"

func platformCoreWait(cmd *exec.Cmd) error { return cmd.Wait() }
func platformCoreMayRestart(error) bool    { return true }
