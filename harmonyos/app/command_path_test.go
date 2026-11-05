//go:build harmonyos

package main

import "testing"

func TestHarmonyCommandPath(t *testing.T) {
	for _, test := range []struct{ name, bin, inherited, want string }{
		{"empty", "/package/bin", "", "/package/bin:/system/bin:/bin:/usr/bin"},
		{"observed API23", "/package/bin", "/data/app/bin:/data/service/hnp/bin:/data/app/bin:/data/service/hnp/bin:", "/package/bin:/data/app/bin:/data/service/hnp/bin:/system/bin:/bin:/usr/bin"},
		{"existing order", "/package/bin", "/usr/bin:/custom/bin:/bin:/system/bin", "/package/bin:/usr/bin:/custom/bin:/bin:/system/bin"},
		{"no cwd search", "/package/bin", ":.:relative:./bin::/safe/bin:", "/package/bin:/safe/bin:/system/bin:/bin:/usr/bin"},
		{"deduplicate", "/package/bin/", "/package/bin:/safe/../safe/bin:/safe/bin", "/package/bin:/safe/bin:/system/bin:/bin:/usr/bin"},
		{"unicode", "/应用/bin", "/用户/tools", "/应用/bin:/用户/tools:/system/bin:/bin:/usr/bin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := harmonyCommandPath(test.bin, test.inherited); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
