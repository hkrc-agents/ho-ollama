//go:build harmonyos

package nativeprocess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchContract(t *testing.T) {
	valid := Launch{Schema: 1, Kind: "core", Args: []string{"ollama", "serve"}, Env: []string{"HOME=/private", "TEST=a=b"}, Directory: filepath.Join(t.TempDir(), "call")}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Launch)
	}{
		{"schema", func(l *Launch) { l.Schema = 2 }},
		{"kind", func(l *Launch) { l.Kind = "arbitrary-lib.so" }},
		{"relative", func(l *Launch) { l.Directory = "tmp/call" }},
		{"root", func(l *Launch) { l.Directory = "/" }},
		{"args", func(l *Launch) { l.Args = nil }},
		{"nul", func(l *Launch) { l.Args = []string{"x\x00y"} }},
		{"duplicate-env", func(l *Launch) { l.Env = []string{"HOME=a", "HOME=b"} }},
		{"bad-env", func(l *Launch) { l.Env = []string{"MISSING_EQUALS"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l := valid
			test.change(&l)
			if l.Validate() == nil {
				t.Fatal("invalid launch accepted")
			}
		})
	}
}

func TestLaunchNeverReusesDirectory(t *testing.T) {
	client, _ := New(Config{Address: "127.0.0.1:1", Capability: strings.Repeat("a", 64), Session: strings.Repeat("b", 64), Instance: 1})
	launch := Launch{Schema: 1, Kind: "runner", Args: []string{"llama-server"}, Directory: filepath.Join(t.TempDir(), "run")}
	_, err := client.Launch(context.Background(), Runner, launch)
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatal(err)
	}
	if _, err = os.Stat(launch.Directory); err != nil {
		t.Fatal("ambiguous create lost its reserved directory", err)
	}
	if _, err = client.Launch(context.Background(), Runner, launch); !os.IsExist(err) {
		t.Fatal("retry reused invocation directory", err)
	}
	launch.Directory = filepath.Join(t.TempDir(), "wrong-role")
	if _, err = client.Launch(context.Background(), Core, launch); err == nil {
		t.Fatal("role/kind mismatch accepted")
	}
	if _, err = os.Stat(launch.Directory); !os.IsNotExist(err) {
		t.Fatal("invalid launch made directory")
	}
}

func TestReturnReceiptBinding(t *testing.T) {
	// Field encoding is shared with the native SO dispatcher. This test does
	// not assert that absent/forged receipts are successful program exits.
	var receipt ReturnReceipt
	if err := json.Unmarshal([]byte(`{"hap_session":"s","instance":3,"pid":12,"code":7}`), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Instance != 3 || receipt.PID != 12 || receipt.Code != 7 {
		t.Fatal(receipt)
	}
}
