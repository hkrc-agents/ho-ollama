//go:build harmonyos

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// HAP inherits HNP command directories, but API23 does not necessarily include
// the system utilities directory. Preserve explicit command precedence and add
// standard target paths; never turn an empty PATH component into a cwd lookup.
// This changes only the HarmonyOS APP-owned service environment, not upstream
// Linux/macOS/Windows behavior or the caller's public HNP environment.
func harmonyCommandPath(binRoot, inherited string) string {
	seen := make(map[string]bool)
	var paths []string
	add := func(path string) {
		if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
			return
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	add(binRoot)
	for _, path := range filepath.SplitList(inherited) {
		add(path)
	}
	for _, path := range []string{"/system/bin", "/bin", "/usr/bin"} {
		add(path)
	}
	return strings.Join(paths, string(os.PathListSeparator))
}
