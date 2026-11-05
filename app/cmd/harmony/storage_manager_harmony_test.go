//go:build harmonyos

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/app/store"
)

func storageManagerFixture(t *testing.T) *harmonyStorageManager {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	directory := filepath.Join(root, "desktop")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(root, "models")
	if err := os.Mkdir(models, 0700); err != nil {
		t.Fatal(err)
	}
	db := &store.Store{DBPath: filepath.Join(root, "desktop.sqlite")}
	t.Cleanup(func() { db.Close() })
	if err := db.SetSettings(store.Settings{Models: models}); err != nil {
		t.Fatal(err)
	}
	core := newHarmonyCore(db)
	core.pidPath = filepath.Join(root, "core.pid")
	return newHarmonyStorageManager(directory, "secret", core, newHarmonyServer(directory, "/unused/llama-server"), t.Context())
}

func storageCall(m *harmonyStorageManager, next http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Cookie", "token=secret")
	w := httptest.NewRecorder()
	m.wrap(next).ServeHTTP(w, r)
	return w
}

func TestStorageModesSurviveManagerRestart(t *testing.T) {
	m := storageManagerFixture(t)
	custom := t.TempDir()
	for _, mode := range []string{"private", "custom"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(os.Getenv("HOME"), "models")
			if mode == "custom" {
				root = custom
			}
			if err := m.setRoot(root); err != nil {
				t.Fatal(err)
			}
			// A fresh manager reads the saved setting, not a transient UI selection.
			restarted := newHarmonyStorageManager(m.directory, m.token, newHarmonyCore(m.core.database), m.server, t.Context())
			w := storageCall(restarted, http.NotFoundHandler(), "GET", "/api/harmony/storage/status", "")
			var status struct {
				Mode           string `json:"mode"`
				Root           string `json:"root"`
				ConfiguredRoot string `json:"configured_root"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 || status.Mode != mode || status.Root != root || status.ConfiguredRoot != root {
				t.Fatalf("status=%s error=%v", w.Body.String(), err)
			}
		})
	}
	if err := os.Remove(custom); err != nil { // Only this test's empty directory.
		t.Fatal(err)
	}
	w := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/storage/status", "")
	var status map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["mode"] != "custom" || status["configured_root"] != custom || status["root"] != "" || status["error"] == "" {
		t.Fatalf("lost unavailable custom selection: %s", w.Body.String())
	}
}

func TestStorageDefaultRootIgnoresDesktopAndCustomEnvironment(t *testing.T) {
	m := storageManagerFixture(t)
	t.Setenv("OLLAMA_MODELS", "/not-the-private-library")
	w := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/storage/status", "")
	var status map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["mode"] != "private" || status["default_root"] != filepath.Join(os.Getenv("HOME"), "models") || status["default_root"] == filepath.Join(m.directory, "models") {
		t.Fatal(w.Body.String())
	}
	for _, invalid := range []string{"", "/", "relative"} {
		t.Setenv("HOME", invalid)
		if root, err := privateStorageRoot(); err == nil || root != "" {
			t.Fatalf("HOME=%q root=%q error=%v", invalid, root, err)
		}
	}
}

func TestStorageReturnPrivateFromUnavailableCustom(t *testing.T) {
	m := storageManagerFixture(t)
	custom := filepath.Join(t.TempDir(), "ollama-models-"+strings.Repeat("a", 32))
	if err := m.setRoot(custom); err != nil {
		t.Fatal(err)
	}
	grant := storageSelection{Schema: 1, ID: "storage-active-host-fixture", Path: filepath.Dir(custom), URI: "file://" + filepath.Dir(custom), OperationMode: 31, Persisted: true, Activated: true, Time: 1}
	m.last = &migrationRecord{State: "completed", NewRoot: custom, NewSelection: &grant}
	root, err := privateStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "private-model")
	if err := os.WriteFile(keep, []byte("keep private bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	w := storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/migrate", `{"mode":"private","confirm":true}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		running := m.cancel != nil
		m.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private switch did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if selected, err := m.core.selectedModelRoot(); err != nil || selected != root {
		t.Fatal(selected, err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep private bytes" {
		t.Fatal("private data changed", err)
	}
	if err := m.Recover(); err != nil || m.last.State != "completed" || m.last.Mode != "private" {
		t.Fatal(m.last, err)
	}
	if m.last.OldSelection == nil || !reflect.DeepEqual(*m.last.OldSelection, grant) || m.last.NewSelection != nil {
		t.Fatal("private return did not retain the old grant for interrupted recovery", m.last)
	}
	w = storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/discard", `{"id":"`+m.last.ID+`","confirm":true}`)
	if w.Code != 409 {
		t.Fatal("private library became discardable", w.Code)
	}
}

func TestStoragePrivateJournalRecovery(t *testing.T) {
	for _, state := range []string{"preparing", "switching", "starting", "completed"} {
		t.Run(state, func(t *testing.T) {
			m := storageManagerFixture(t)
			old := t.TempDir()
			root, err := privateStorageRoot()
			if err != nil {
				t.Fatal(err)
			}
			r := migrationRecord{Version: 1, ID: strings.Repeat("c", 32), Mode: "private", State: state, OldRoot: old, NewRoot: root, Parent: filepath.Dir(root), ServerConfig: m.server.snapshot().Config}
			if err := storageAtomicJSON(filepath.Join(m.directory, "storage-migration.json"), r); err != nil {
				t.Fatal(err)
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			selected, err := m.core.selectedModelRoot()
			want := old
			if state == "completed" {
				want = root
			}
			if err != nil || selected != want {
				t.Fatal(state, selected, err)
			}
			for _, path := range []string{old, root} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("recovery removed a retained library", err)
				}
			}
		})
	}
}

func TestStoragePrivateJournalRejectsArbitraryTarget(t *testing.T) {
	m := storageManagerFixture(t)
	r := migrationRecord{Version: 1, ID: strings.Repeat("c", 32), Mode: "private", State: "completed", OldRoot: t.TempDir(), NewRoot: t.TempDir()}
	r.Parent = filepath.Dir(r.NewRoot)
	if err := storageAtomicJSON(filepath.Join(m.directory, "storage-migration.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(); err == nil || m.blocked == "" {
		t.Fatal("arbitrary private target accepted")
	}
}

func TestStorageActiveGrantErrorIndependentOfStagedSelection(t *testing.T) {
	m := storageManagerFixture(t)
	custom := t.TempDir()
	if err := m.setRoot(custom); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{"schema": 1, "new_root": custom, "old_root": "/old", "new_error": "active grant revoked", "old_error": "", "error": ""}
	if err := storageAtomicJSON(filepath.Join(filepath.Dir(m.directory), "model-directory-active-restore.json"), receipt); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"custom", "private"} {
		if mode == "private" {
			root, err := privateStorageRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := m.setRoot(root); err != nil {
				t.Fatal(err)
			}
		}
		w := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/storage/status", "")
		var status map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if mode == "custom" && status["authorization_error"] != "active grant revoked" {
			t.Fatal(w.Body.String())
		}
		if mode == "custom" && (status["root"] != "" || status["configured_root"] != custom) {
			t.Fatal("revoked root was presented as active or saved selection was lost", w.Body.String())
		}
		if mode == "private" && status["authorization_error"] != "" {
			t.Fatal("obsolete custom grant blocked private mode", w.Body.String())
		}
	}
}

func TestStoragePrivateSwitchRejectsUnsafeRequests(t *testing.T) {
	m := storageManagerFixture(t)
	for _, body := range []string{`{"mode":"private","confirm":false}`, `{"mode":"other","confirm":true}`, `{"mode":"private","selection_id":"fake","confirm":true}`, `{"mode":"private","path":"/arbitrary","confirm":true}`} {
		if w := storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/migrate", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err := m.setRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m.gate.RLock()
	w := storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/migrate", `{"mode":"private","confirm":true}`)
	m.gate.RUnlock()
	if w.Code != 409 {
		t.Fatal("private switch bypassed operation gate", w.Code)
	}
}

func TestStorageGateAndPrivateAuth(t *testing.T) {
	m := storageManagerFixture(t)
	m.gate.Lock()
	for _, path := range []string{"/api/pull", "/api/harmony/gguf", "/api/generate", "/api/harmony/services"} {
		w := storageCall(m, http.NotFoundHandler(), "POST", path, `{}`)
		if w.Code != 423 {
			t.Fatal(path, w.Code)
		}
	}
	w := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/storage/status", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "/api/harmony/storage/status", nil)
	w = httptest.NewRecorder()
	m.wrap(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	m.gate.Unlock()
}

func TestStorageDeleteCannotRaceActiveModelRequests(t *testing.T) {
	for _, path := range []string{"/api/pull", "/api/harmony/gguf", "/api/generate", "/api/chat", "/api/harmony/services", "/api/v1/chat"} {
		t.Run(path, func(t *testing.T) {
			m := storageManagerFixture(t)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				storageCall(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }), "POST", path, `{}`)
			}()
			<-entered
			w := storageCall(m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("delete escaped active request gate") }), "DELETE", "/api/delete", `{"model":"test:latest"}`)
			close(release)
			<-done
			if w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestStorageSettingsCannotBypassMigration(t *testing.T) {
	m := storageManagerFixture(t)
	root, _ := m.core.selectedModelRoot()
	for _, body := range []string{`{"Models":"/another"}`, `{"models":""}`, `{"Models":17}`} {
		w := storageCall(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("root change reached upstream") }), "POST", "/api/v1/settings", body)
		if w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := storageCall(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var values map[string]any
		json.NewDecoder(r.Body).Decode(&values)
		if values["Models"] != root || values["Expose"] != false {
			t.Fatal(values)
		}
	}), "POST", "/api/v1/settings", `{"Expose":false}`)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	after, _ := m.core.selectedModelRoot()
	if after != root {
		t.Fatal("root changed")
	}
}

func TestStorageRecoverInterruptedSelection(t *testing.T) {
	for _, phase := range []string{"copying", "switching", "starting"} {
		t.Run(phase, func(t *testing.T) {
			m := storageManagerFixture(t)
			old, _ := m.core.selectedModelRoot()
			parent := t.TempDir()
			id := strings.Repeat("b", 32)
			target := filepath.Join(parent, "ollama-models-"+id)
			os.Mkdir(target, 0700)
			r := migrationRecord{Version: 1, ID: id, OldRoot: old, NewRoot: target, Parent: parent, State: phase, ServerConfig: m.server.snapshot().Config}
			if err := m.setRoot(target); err != nil {
				t.Fatal(err)
			}
			if err := storageAtomicJSON(filepath.Join(m.directory, "storage-migration.json"), r); err != nil {
				t.Fatal(err)
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			selected, _ := m.core.selectedModelRoot()
			if selected != old || m.last.State != "rolled_back" {
				t.Fatal(selected, m.last.State)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatal("recovery removed target without consent")
			}
		})
	}
}

func TestStorageCorruptJournalFailsClosed(t *testing.T) {
	m := storageManagerFixture(t)
	old, _ := m.core.selectedModelRoot()
	os.WriteFile(filepath.Join(m.directory, "storage-migration.json"), []byte("{"), 0600)
	if err := m.Recover(); err == nil || m.blocked == "" {
		t.Fatal("corrupt state ignored")
	}
	after, _ := m.core.selectedModelRoot()
	if old != after {
		t.Fatal("corruption selected a different root")
	}
	w := storageCall(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("write permitted during unknown recovery") }), "POST", "/api/pull", `{}`)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestStorageUnconfirmedSelectionCannotMigrate(t *testing.T) {
	m := storageManagerFixture(t)
	old, _ := m.core.selectedModelRoot()
	w := storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/migrate", `{"selection_id":"invented","confirm":true}`)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := m.core.selectedModelRoot()
	if after != old {
		t.Fatal("unqualified root was switched")
	}
}

func TestStorageExportDigestAndBoundaries(t *testing.T) {
	for _, kind := range []string{"valid", "corrupt", "outside_root", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m := storageManagerFixture(t)
			root, _ := m.core.selectedModelRoot()
			os.Mkdir(filepath.Join(root, "blobs"), 0700)
			data := append([]byte("GGUF"), bytes.Repeat([]byte{9}, 256<<10)...)
			sum := sha256.Sum256(data)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			path := filepath.Join(root, "blobs", strings.ReplaceAll(digest, ":", "-"))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			file := harmonyModelFile{Model: "test", Digest: digest, Manifest: strings.Repeat("f", 64), Path: path, Size: int64(len(data))}
			switch kind {
			case "corrupt":
				os.WriteFile(path, bytes.Repeat([]byte{1}, len(data)), 0600)
			case "outside_root":
				file.Path = filepath.Join(t.TempDir(), "outside")
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				os.WriteFile(outside, data, 0600)
				os.Remove(path)
				os.Symlink(outside, path)
			}
			m.inspect = func(context.Context, string, string) (harmonyModelFile, error) { return file, nil }
			metadata := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/gguf/metadata?model=test", "")
			if strings.Contains(metadata.Body.String(), path) {
				t.Fatal("internal path exposed in metadata")
			}
			w := storageCall(m, http.NotFoundHandler(), "GET", "/api/harmony/gguf/export?model=test", "")
			if kind == "valid" {
				if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("X-GGUF-SHA256") != hex.EncodeToString(sum[:]) {
					t.Fatal("export changed bytes")
				}
			} else if w.Code != 409 {
				t.Fatal("unsafe export passed", kind, w.Code)
			}
		})
	}
}

func TestStorageDeleteRejectsLoadedModels(t *testing.T) {
	for _, kind := range []string{"standalone", "core_loaded", "different_loaded", "core_idle", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			m := storageManagerFixture(t)
			if kind == "standalone" {
				m.server.current.State = "ready"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/ps" {
					t.Fatal(r.URL.Path)
				}
				if kind == "core_loaded" {
					io.WriteString(w, `{"models":[{"name":"test:latest"}]}`)
				} else if kind == "different_loaded" {
					io.WriteString(w, `{"models":[{"name":"other:latest"}]}`)
				} else if kind == "unknown" {
					w.WriteHeader(503)
				} else {
					io.WriteString(w, `{"models":[]}`)
				}
			}))
			defer upstream.Close()
			t.Setenv("OLLAMA_HOST", upstream.URL)
			called := false
			w := storageCall(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), "DELETE", "/api/delete", `{"model":"test"}`)
			if (kind == "core_idle" || kind == "different_loaded") != called {
				t.Fatal("delete load guard wrong", kind, w.Code)
			}
			if !called && w.Code != 409 {
				t.Fatal(w.Code)
			}
		})
	}
}

func TestStorageActiveRootDoesNotFollowUnappliedSettings(t *testing.T) {
	m := storageManagerFixture(t)
	original, _ := m.core.selectedModelRoot()
	m.core.snapshot.State = "ready"
	m.core.snapshot.ModelRoot = original
	other := t.TempDir()
	if err := m.setRoot(other); err != nil {
		t.Fatal(err)
	}
	if _, err := m.core.activeModelRoot(); err == nil {
		t.Fatal("claimed new directory was already active")
	}
	os.Remove(other) // only this test's freshly allocated empty directory
	if _, err := m.core.selectedModelRoot(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
