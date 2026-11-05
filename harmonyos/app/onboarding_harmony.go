//go:build harmonyos

package onboarding

import "path/filepath"

func platformAppDatabasePath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".ollama", "db.sqlite")
}
