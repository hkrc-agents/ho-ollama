//go:build harmonyos

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func modelAuthorizationReceipt(t *testing.T, oldRoot, newRoot, oldError, newError, globalError string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"schema": 1, "old_root": oldRoot, "new_root": newRoot,
		"old_error": oldError, "new_error": newError, "error": globalError})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func modelAuthorizationWriteReceipt(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "model-directory-active-restore.json"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestModelRootAuthorizationReadableCustomFailures(t *testing.T) {
	for _, failure := range []string{"old", "new", "global"} {
		t.Run(failure, func(t *testing.T) {
			m := storageManagerFixture(t)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "readable-model"), []byte("readable despite failed persistence"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.setRoot(root + string(filepath.Separator)); err != nil {
				t.Fatal(err)
			}
			oldError, newError, globalError := "", "", ""
			switch failure {
			case "old":
				oldError = "activation failed"
			case "new":
				newError = "persistent policy revoked"
			case "global":
				globalError = "journal restore failed"
			}
			modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, root, root, oldError, newError, globalError))
			if _, err := os.Stat(root); err != nil {
				t.Fatal("fixture must remain actually readable", err)
			}
			if _, err := os.ReadFile(filepath.Join(root, "readable-model")); err != nil {
				t.Fatal(err)
			}
			m.core.snapshot.State, m.core.snapshot.ModelRoot = "ready", root
			for _, selected := range []func() (string, error){m.core.selectedModelRoot, m.core.activeModelRoot} {
				if value, err := selected(); err == nil || value != "" || !strings.Contains(err.Error(), "authorization") {
					t.Fatal(value, err)
				}
			}
			settings, err := m.core.database.Settings()
			if err != nil || settings.Models != root+string(filepath.Separator) {
				t.Fatal("failure changed saved selection", settings.Models, err)
			}
			if err := m.core.Start(t.Context()); err == nil || m.core.Snapshot().State != "failed" {
				t.Fatal("core startup did not reject failed authorization", m.core.Snapshot(), err)
			}
			if _, err := os.Stat(m.core.pidPath + ".control.lock"); !os.IsNotExist(err) {
				t.Fatal("startup passed authorization gate and reached process setup", err)
			}
		})
	}
}

func TestModelRootAuthorizationCompatibility(t *testing.T) {
	for _, state := range []string{"missing", "initial-private", "other-root-errors", "matching-success"} {
		t.Run(state, func(t *testing.T) {
			m := storageManagerFixture(t)
			root := t.TempDir()
			if err := m.setRoot(root); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "initial-private":
				modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, "", "", "", "", ""))
			case "other-root-errors":
				modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, t.TempDir(), t.TempDir(), "old failed", "new staged failed", ""))
			case "matching-success":
				modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, root, "/other", "", "other failed", ""))
			}
			for _, selected := range []func() (string, error){m.core.selectedModelRoot, m.core.activeModelRoot} {
				if value, err := selected(); err != nil || value != root {
					t.Fatal(state, value, err)
				}
			}
		})
	}
}

func TestModelRootAuthorizationMalformedReceipt(t *testing.T) {
	for _, text := range []string{"{", "null", "{}", `{"schema":2}`, `{"schema":1,"old_root":false}`, `{"schema":1,"old_root":"","new_root":"","old_error":"unbound","new_error":"","error":""}`, `{"schema":1,"old_root":"relative","new_root":"","old_error":"","new_error":"","error":""}`, `{"schema":1,"old_root":"/bad\u0000root","new_root":"","old_error":"","new_error":"","error":""}`, strings.Repeat("x", 16385)} {
		t.Run(text[:min(len(text), 35)], func(t *testing.T) {
			m := storageManagerFixture(t)
			root := t.TempDir()
			if err := m.setRoot(root); err != nil {
				t.Fatal(err)
			}
			modelAuthorizationWriteReceipt(t, text)
			if value, err := m.core.selectedModelRoot(); err == nil || value != "" {
				t.Fatal("malformed custom receipt accepted", value, err)
			}
			privateRoot, err := privateStorageRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := m.setRoot(privateRoot); err != nil {
				t.Fatal(err)
			}
			if value, err := m.core.selectedModelRoot(); err != nil || value != privateRoot {
				t.Fatal("malformed custom receipt blocked private library", value, err)
			}
		})
	}
}

func TestModelRootAuthorizationPrivateIgnoresCustomFailure(t *testing.T) {
	m := storageManagerFixture(t)
	root, err := privateStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, root, root, "old failure", "new failure", "global failure"))
	if value, err := m.core.selectedModelRoot(); err != nil || value != root {
		t.Fatal(value, err)
	}
}

func TestModelRootAuthorizationExplicitReturnPrivate(t *testing.T) {
	m := storageManagerFixture(t)
	custom := t.TempDir()
	if err := m.setRoot(custom); err != nil {
		t.Fatal(err)
	}
	modelAuthorizationWriteReceipt(t, modelAuthorizationReceipt(t, "", custom, "", "revoked custom grant", ""))
	if _, err := m.core.selectedModelRoot(); err == nil {
		t.Fatal("revoked custom library accepted")
	}
	w := storageCall(m, http.NotFoundHandler(), "POST", "/api/harmony/storage/migrate", `{"mode":"private","confirm":true}`)
	if w.Code != http.StatusAccepted {
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
			t.Fatal("private switch timed out")
		}
		time.Sleep(time.Millisecond)
	}
	root, err := privateStorageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if value, err := m.core.selectedModelRoot(); err != nil || value != root {
		t.Fatal(value, err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatal("custom library was removed", err)
	}
	if m.last.State != "completed" {
		t.Fatal(m.last.State)
	}
}
