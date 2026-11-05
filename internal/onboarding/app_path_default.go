//go:build !harmonyos

package onboarding

func platformAppDatabasePath(home string) string { return "" }
