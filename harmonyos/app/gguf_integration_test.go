//go:build harmonyos && ggufintegration

package main

// Explicit opt-in: real pinned Linux core and read-only caller GGUF, with a
// wholly temporary HOME/model root. Never points at an installed model library.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

func TestGGUFRealCore(t *testing.T) {
	binary, source := os.Getenv("PICO_GGUF_CORE"), os.Getenv("PICO_GGUF_SOURCE")
	if binary == "" || source == "" {
		t.Fatal("explicit isolated core binary and source file required")
	}
	file, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before := sha256.New()
	if _, err := io.Copy(before, file); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + hex.EncodeToString(before.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "serve")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "OLLAMA_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+root, "OLLAMA_MODELS="+filepath.Join(root, "models"), "OLLAMA_HOST="+address)
	log, err := os.Create(filepath.Join(root, "core.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if cmd == nil {
			log.Close()
			return
		}
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(root, "core.log"))
			t.Log(string(data))
		}
	}()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(address + "/api/tags")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated core did not become ready")
	}
	t.Setenv("OLLAMA_HOST", address)
	s := newHarmonyGGUF(filepath.Join(root, "app"))
	r := httptest.NewRequest("POST", "/api/harmony/gguf", file).WithContext(ctx)
	r.ContentLength = info.Size()
	r.Header.Set("X-GGUF-Size", strconv.FormatInt(info.Size(), 10))
	w := httptest.NewRecorder()
	s.importFile(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	list := httptest.NewRecorder()
	s.list(list, httptest.NewRequest("GET", "/", nil))
	var result struct {
		Imports []harmonyImportReceipt `json:"imports"`
	}
	json.Unmarshal(list.Body.Bytes(), &result)
	if len(result.Imports) != 1 || result.Imports[0].Status != "completed" || result.Imports[0].Digest != digest {
		t.Fatal(list.Body.String())
	}
	blob, err := os.Open(filepath.Join(root, "models", "blobs", strings.ReplaceAll(digest, ":", "-")))
	if err != nil {
		t.Fatal(err)
	}
	after := sha256.New()
	n, err := io.Copy(after, blob)
	blob.Close()
	if err != nil || n != info.Size() || hex.EncodeToString(after.Sum(nil)) != strings.TrimPrefix(digest, "sha256:") {
		t.Fatal("core installed blob differs from source")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	after.Reset()
	io.Copy(after, file)
	if hex.EncodeToString(after.Sum(nil)) != strings.TrimPrefix(digest, "sha256:") {
		t.Fatal("original source changed")
	}
	assertNoGGUFStage(t, s)
	// Real byte copy and a second core boot from the copied library. This is a
	// host-only isolated fixture, not a substitute for native directory grants.
	app := os.Getenv("PICO_GGUF_APP")
	if app == "" {
		t.Fatal("explicit Linux APP worker binary required")
	}
	inspect := func(modelRoot string) harmonyModelFile {
		t.Helper()
		output, e := exec.CommandContext(ctx, app, "--model-store-worker", modelRoot, result.Imports[0].Model).Output()
		var descriptor harmonyModelFile
		if e != nil || json.Unmarshal(output, &descriptor) != nil || descriptor.Digest != digest || descriptor.Size != info.Size() {
			t.Fatalf("real worker: %s %v", output, e)
		}
		return descriptor
	}
	inspect(filepath.Join(root, "models"))
	parent := filepath.Join(root, "copy-parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	migration := &migrationRecord{Version: 1, ID: id, OldRoot: filepath.Join(root, "models"), Parent: parent, NewRoot: filepath.Join(parent, "ollama-models-"+id)}
	selected := migration.OldRoot
	hooks := migrationHooks{
		Save: func(r *migrationRecord) error { return storageAtomicJSON(filepath.Join(root, "migration.json"), r) },
		Stop: func() error {
			if cmd == nil {
				return nil
			}
			cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
				cmd = nil
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		SetRoot: func(value string) error { selected = value; return nil }, GetRoot: func() (string, error) { return selected, nil },
		ConfigureServer: func(serverConfig) error { return nil }, CheckSelection: func() error { return nil }, Space: func(string, int64) error { return nil },
		Start: func(startCtx context.Context, r *migrationRecord, forward bool) error {
			cmd = exec.CommandContext(ctx, binary, "serve")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "OLLAMA_MODELS=" + selected, "OLLAMA_HOST=" + address}
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				return err
			}
			done = make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
				response, err := client.Get(address + "/api/tags")
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
						return nil
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("copied-library core not ready")
		},
	}
	if err := runStorageMigration(ctx, migration, hooks); err != nil {
		t.Fatal(err)
	}
	if migration.State != "completed" || selected != migration.NewRoot {
		t.Fatal("migration did not commit")
	}
	old, next := inspect(migration.OldRoot), inspect(migration.NewRoot)
	if old.Manifest != next.Manifest {
		t.Fatal("copied manifest changed")
	}
	reader, e := os.Open(next.Path)
	if e != nil {
		t.Fatal(e)
	}
	verified := sha256.New()
	copied, e := io.Copy(verified, reader)
	reader.Close()
	if e != nil || copied != info.Size() || "sha256:"+hex.EncodeToString(verified.Sum(nil)) != digest {
		t.Fatal("copied real weights checksum differs", e)
	}
	show, e := api.ClientFromEnvironment()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = show.Show(ctx, &api.ShowRequest{Model: result.Imports[0].Model}); e != nil {
		t.Fatal("copied core cannot read model", e)
	}
	t.Log(fmt.Sprintf("real core create confirmed: model=%s bytes=%d digest=%s; source preserved; no inference requested", result.Imports[0].Model, n, digest))
}
