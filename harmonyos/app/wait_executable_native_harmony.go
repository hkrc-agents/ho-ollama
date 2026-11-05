//go:build harmonyos && pico_native_services

package main

import "os/exec"

// Archived opt-in SO path never selects the executable service manager.
func waitExecutableService(cmd *exec.Cmd) error { return cmd.Wait() }
func executableDrainBlocked(error) bool         { return false }
func harmonyWriteShutdownReceipt(directory string, identity serverIdentity) error {
	return writeHarmonyShutdownReceipt(directory, identity)
}
