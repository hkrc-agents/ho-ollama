//go:build harmonyos && pico_native_services

package main

func recordHarmonyServiceIntent(string, string, serverConfig) error { return nil }
func harmonyCoreShouldStart() (bool, error)                         { return true, nil }
