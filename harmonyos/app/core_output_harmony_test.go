//go:build harmonyos

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const coreTestBindLine = "time=2026-10-21T02:04:59.369Z level=INFO source=routes.go:2288 msg=\"Listening on 127.0.0.1:11434 (version 0.0.0)\"\n"

func TestCoreBindParserFramingAndBounds(t *testing.T) {
	var addresses []string
	p := &coreBindParser{bind: func(s string) { addresses = append(addresses, s) }}
	for _, ch := range []byte(coreTestBindLine) {
		p.Write([]byte{ch})
	}
	if len(addresses) != 1 || addresses[0] != "127.0.0.1:11434" {
		t.Fatal(addresses)
	}
	addresses = nil
	for _, bad := range []string{
		"prompt " + coreTestBindLine,
		strings.Replace(coreTestBindLine, "level=INFO", "level=ERROR", 1),
		strings.Replace(coreTestBindLine, "source=routes.go", "source=other.go", 1),
		strings.Replace(coreTestBindLine, "11434", "notaport", 1),
		strings.Repeat("x", coreBindLineLimit+1) + coreTestBindLine,
	} {
		p.Write([]byte(bad))
	}
	if len(addresses) != 0 || len(p.line) > coreBindLineLimit {
		t.Fatal("accepted untrusted/oversized line", addresses)
	}
	p.Write([]byte(coreTestBindLine[:len(coreTestBindLine)-1]))
	if len(addresses) != 0 {
		t.Fatal("accepted unterminated line")
	}
	p.Write([]byte("\n"))
	if len(addresses) != 1 {
		t.Fatal("parser did not recover")
	}
}

func TestCoreOutputChildHelper(t *testing.T) {
	if os.Getenv("PICO_CORE_OUTPUT_HELPER") != "1" {
		return
	}
	listener, err := net.Listen("tcp4", os.Getenv("PICO_CORE_OUTPUT_ADDRESS"))
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	if os.Getenv("PICO_CORE_OUTPUT_EMIT") == "1" {
		fmt.Fprintf(os.Stderr, "time=2026-10-21T02:04:59.369Z level=INFO source=routes.go:2288 msg=\"Listening on %s (version 0.0.0)\"\n", listener.Addr())
	}
	if err := os.WriteFile(os.Getenv("PICO_CORE_OUTPUT_READY"), []byte("ready"), 0600); err != nil {
		os.Exit(3)
	}
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"version":"test"}`)
	}))
}

func coreTestOutputChild(t *testing.T, emit bool) (CoreIdentity, string) {
	t.Helper()
	return coreTestOutputChildConfig(t, emit, "127.0.0.1", nil)
}

func coreTestOutputChildConfig(t *testing.T, emit bool, bindIP string, env []string) (CoreIdentity, string) {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "ollama")
	if err := os.Symlink(exe, entry); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	logPath := filepath.Join(dir, "old-server.log")
	// A valid historical event in the output logfile must not seed pipe proof.
	if err := os.WriteFile(logPath, []byte(strings.ReplaceAll(coreTestBindLine, "127.0.0.1:11434", address)), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	ready := filepath.Join(dir, "ready")
	cmd := commandContext(context.Background(), entry, "-test.run=^TestCoreOutputChildHelper$", "serve")
	cmd.Env = append(os.Environ(), "PICO_CORE_OUTPUT_HELPER=1", "PICO_CORE_OUTPUT_ADDRESS="+address, "PICO_CORE_OUTPUT_READY="+ready)
	// Set the final environment after installing the output hook, matching
	// upstream Server.cmd ordering. $PORT is just a test-fixture placeholder.
	cmd.Stdout, cmd.Stderr = platformCoreOutput(cmd, log)
	_, boundPort, _ := net.SplitHostPort(address)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		filtered := cmd.Env[:0]
		for _, old := range cmd.Env {
			if !strings.HasPrefix(old, key+"=") {
				filtered = append(filtered, old)
			}
		}
		cmd.Env = append(filtered, strings.ReplaceAll(entry, "$PORT", boundPort))
	}
	if emit {
		cmd.Env = append(cmd.Env, "PICO_CORE_OUTPUT_EMIT=1")
	}
	if cmd.Stdout != cmd.Stderr {
		t.Fatal("streams must share one child pipe writer")
	}
	if _, inherited := cmd.Stdout.(*os.File); inherited {
		t.Fatal("child inherited logfile rather than a pipe")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper startup timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	id, err := CoreProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if emit {
		for !corePipeBindOwned(id, address) {
			if time.Now().After(deadline) {
				t.Fatal("actual child pipe event not captured")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return id, address
}

func TestCoreExplicitWildcardAliasPolicy(t *testing.T) {
	for _, tt := range []struct {
		name         string
		env          []string
		bound, alias string
	}{
		{"expose-default", []string{"OLLAMA_HOST=0.0.0.0"}, "0.0.0.0:11434", "127.0.0.1:11434"},
		{"explicit-port", []string{"OLLAMA_HOST=0.0.0.0:18080"}, "0.0.0.0:18080", "127.0.0.1:18080"},
		{"http-default", []string{"OLLAMA_HOST=http://0.0.0.0"}, "0.0.0.0:80", "127.0.0.1:80"},
		{"ipv6", []string{"OLLAMA_HOST=[::]:18080"}, "[::]:18080", "[::1]:18080"},
		{"no-explicit-env", nil, "0.0.0.0:11434", ""},
		{"loopback-request", []string{"OLLAMA_HOST=127.0.0.1:11434"}, "0.0.0.0:11434", ""},
		{"wrong-port", []string{"OLLAMA_HOST=0.0.0.0:18080"}, "0.0.0.0:11434", ""},
		{"wrong-family", []string{"OLLAMA_HOST=0.0.0.0:11434"}, "[::]:11434", ""},
		{"duplicate-env", []string{"OLLAMA_HOST=0.0.0.0", "OLLAMA_HOST=127.0.0.1"}, "0.0.0.0:11434", ""},
		{"invalid-port", []string{"OLLAMA_HOST=0.0.0.0:bad"}, "0.0.0.0:11434", ""},
		{"remote-name", []string{"OLLAMA_HOST=example.org"}, "0.0.0.0:11434", ""},
		{"path", []string{"OLLAMA_HOST=http://0.0.0.0/path"}, "0.0.0.0:80", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			alias, ok := coreWildcardLoopback(tt.env, tt.bound)
			if alias != tt.alias || ok != (tt.alias != "") {
				t.Fatalf("alias=%q ok=%t", alias, ok)
			}
		})
	}
}

func TestCoreActualWildcardPipeAlias(t *testing.T) {
	// Avoid a duplicate inherited OLLAMA_HOST in this subprocess fixture.
	t.Setenv("OLLAMA_HOST", "0.0.0.0:1")
	// Build a single explicit key: the helper's extra environment replaces it.
	id, bound := coreTestOutputChildConfig(t, true, "0.0.0.0", []string{"OLLAMA_HOST=0.0.0.0:$PORT"})
	_, port, _ := net.SplitHostPort(bound)
	alias := net.JoinHostPort("127.0.0.1", port)
	denied := func(string) ([]byte, error) { return nil, os.ErrPermission }
	if got := coreListenerProof(id, alias, denied); got != "child-pipe-bind" {
		t.Fatalf("explicit wildcard alias proof=%q", got)
	}
	for _, wrong := range []string{net.JoinHostPort("127.0.0.2", port), net.JoinHostPort("::1", port), net.JoinHostPort("127.0.0.1", "1")} {
		if corePipeBindOwned(id, wrong) {
			t.Fatal("wildcard alias widened:", wrong)
		}
	}
	wrong := id
	wrong.StartTicks++
	if corePipeBindOwned(wrong, alias) {
		t.Fatal("stale wildcard identity accepted")
	}
	response, err := http.Get("http://" + alias + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("loopback health failed")
	}
}

func TestCorePipeProofRequiresFreshChildEvent(t *testing.T) {
	id, address := coreTestOutputChild(t, false)
	denied := func(string) ([]byte, error) { return nil, os.ErrPermission }
	if corePipeBindOwned(id, address) || coreListenerProof(id, address, denied) != "" {
		t.Fatal("historical logfile/HTTP-only accepted")
	}
	response, err := http.Get("http://" + address + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || coreListenerProof(id, address, denied) != "" {
		t.Fatal("HTTP success became ownership evidence")
	}
}

func TestCorePipeProofIdentityAddressAndDenial(t *testing.T) {
	id, address := coreTestOutputChild(t, true)
	denied := func(string) ([]byte, error) { return nil, os.ErrPermission }
	if got := coreListenerProof(id, address, denied); got != "child-pipe-bind" {
		t.Fatalf("proof=%q", got)
	}
	readable := func(string) ([]byte, error) { return []byte("header only\n"), nil }
	if coreListenerProof(id, address, readable) != "" {
		t.Fatal("fallback used despite readable proc-net")
	}
	absent := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if coreListenerProof(id, address, absent) != "" {
		t.Fatal("non-permission failure authorized fallback")
	}
	for _, wrong := range []CoreIdentity{
		{PID: id.PID, UID: id.UID, PPID: id.PPID, PGID: id.PGID, StartTicks: id.StartTicks + 1, Executable: id.Executable},
		{PID: os.Getpid(), UID: id.UID, PPID: id.PPID, PGID: id.PGID, StartTicks: id.StartTicks, Executable: id.Executable},
		{PID: id.PID, UID: id.UID, PPID: id.PPID, PGID: id.PGID, StartTicks: id.StartTicks, Executable: id.Executable + ".other"},
	} {
		if coreListenerProof(wrong, address, denied) != "" {
			t.Fatal("wrong identity accepted", wrong)
		}
	}
	host, port, _ := net.SplitHostPort(address)
	for _, wrong := range []string{net.JoinHostPort(host, "1"), net.JoinHostPort("127.0.0.2", port)} {
		if coreListenerProof(id, wrong, denied) != "" {
			t.Fatal("wrong address accepted", wrong)
		}
	}
	response, err := http.Get("http://" + address + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("health failed")
	}
}
