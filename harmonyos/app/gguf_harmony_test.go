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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func ggufRequest(body []byte) *http.Request {
	r := httptest.NewRequest("POST", "/api/harmony/gguf", bytes.NewReader(body))
	r.Header.Set("X-GGUF-Size", strconv.Itoa(len(body)))
	return r
}
func ggufFixture() []byte { return append([]byte("GGUF"), bytes.Repeat([]byte{7}, 160<<10)...) }
func ggufStore(t *testing.T) *harmonyGGUF {
	t.Helper()
	s := newHarmonyGGUF(t.TempDir())
	s.space = func(string, int64) error { return nil }
	return s
}
func assertNoGGUFStage(t *testing.T, s *harmonyGGUF) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.directory, "upload-*.part"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("staging retained: %v %v", matches, err)
	}
}

func TestGGUFOfficialCoreProtocol(t *testing.T) {
	for _, outcome := range []string{"success", "error", "eof", "http-error", "marker-failure"} {
		t.Run(outcome, func(t *testing.T) {
			s := ggufStore(t)
			if outcome == "marker-failure" {
				s.writeReceipt = func(path string, data []byte) error {
					if strings.HasSuffix(path, ".complete") {
						return syscall.ENOSPC
					}
					return harmonyImportWrite(path, data)
				}
			}
			data := ggufFixture()
			hash := sha256.Sum256(data)
			digest := "sha256:" + hex.EncodeToString(hash[:])
			var calls atomic.Int32
			var created string
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Cookie") != "" {
					t.Error("APP credential leaked")
				}
				if strings.HasPrefix(r.URL.Path, "/api/blobs/") {
					body, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, data) || r.URL.Path != "/api/blobs/"+digest {
						t.Error("upload byte/digest mismatch")
					}
					w.WriteHeader(201)
					return
				}
				if r.URL.Path != "/api/create" {
					t.Error(r.URL.Path)
					w.WriteHeader(404)
					return
				}
				var req struct {
					Model string            `json:"model"`
					Files map[string]string `json:"files"`
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil || req.Files["model.gguf"] != digest {
					t.Error("not official files contract")
				}
				created = req.Model
				if !strings.HasPrefix(created, "harmony-import-") {
					t.Error("not fresh generated name")
				}
				// Intent must be readable before a potentially committing core call.
				files, _ := filepath.Glob(filepath.Join(s.directory, "*.json"))
				if len(files) != 1 {
					t.Error("missing pre-create intent")
				}
				switch outcome {
				case "success", "marker-failure":
					io.WriteString(w, "{\"status\":\"parsing\"}\n{\"status\":\"success\"}\n")
				case "error":
					io.WriteString(w, "{\"error\":\"bad GGUF\"}\n")
				case "http-error":
					http.Error(w, "failed after possible commit", 500)
				default:
					io.WriteString(w, "{\"status\":\"parsing\"}\n")
				}
			}))
			defer core.Close()
			t.Setenv("OLLAMA_HOST", core.URL)
			w := httptest.NewRecorder()
			s.importFile(w, ggufRequest(data))
			if calls.Load() != 2 || created == "" {
				t.Fatalf("calls %d: %s", calls.Load(), w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"status":"success"`) != (outcome == "success") {
				t.Fatal(w.Body.String())
			}
			// Re-open a new manager: unconfirmed is deliberately not called rollback.
			reopened := newHarmonyGGUF(filepath.Dir(s.directory))
			list := httptest.NewRecorder()
			reopened.list(list, httptest.NewRequest("GET", "/", nil))
			var result struct {
				Imports []harmonyImportReceipt `json:"imports"`
			}
			json.Unmarshal(list.Body.Bytes(), &result)
			if list.Code != 200 || len(result.Imports) != 1 || result.Imports[0].Model != created {
				t.Fatal(list.Body.String())
			}
			want := "unconfirmed"
			if outcome == "success" {
				want = "completed"
			}
			if result.Imports[0].Status != want {
				t.Fatal(result)
			}
			assertNoGGUFStage(t, s)
		})
	}
}

func TestGGUFRejectBeforeCore(t *testing.T) {
	for _, mode := range []string{"magic", "size", "oversize", "truncated", "cancelled", "space", "busy", "intent"} {
		t.Run(mode, func(t *testing.T) {
			s := ggufStore(t)
			var calls atomic.Int32
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("invalid upload reached core") }))
			defer core.Close()
			t.Setenv("OLLAMA_HOST", core.URL)
			data := ggufFixture()
			r := ggufRequest(data)
			switch mode {
			case "magic":
				data[0] = 'X'
			case "size":
				r.Header.Set("X-GGUF-Size", "not-size")
			case "oversize":
				r.Header.Set("X-GGUF-Size", strconv.FormatInt(harmonyGGUFMax+1, 10))
			case "truncated":
				r.Body = io.NopCloser(bytes.NewReader(data[:20]))
			case "cancelled":
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			case "space":
				s.space = func(string, int64) error { return errors.New("no space") }
			case "busy":
				s.mu.Lock()
				defer s.mu.Unlock()
			case "intent":
				s.writeReceipt = func(string, []byte) error { return errors.New("record filesystem unavailable") }
			}
			w := httptest.NewRecorder()
			s.importFile(w, r)
			if w.Code < 400 || calls.Load() != 0 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			assertNoGGUFStage(t, s)
		})
	}
}

func TestGGUFBlobFailureNeverCreatesOrReportsSuccess(t *testing.T) {
	for _, mode := range []string{"enospc", "partial_disconnect", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := ggufStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var creates atomic.Int32
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/create" {
					creates.Add(1)
					return
				}
				if !strings.HasPrefix(r.URL.Path, "/api/blobs/") {
					t.Error(r.URL.Path)
					return
				}
				io.CopyN(io.Discard, r.Body, 17)
				switch mode {
				case "enospc":
					http.Error(w, "no space left on device", 507)
				case "cancel":
					cancel()
					http.Error(w, "cancelled", 499)
				case "partial_disconnect":
					connection, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					connection.Close()
				}
			}))
			defer core.Close()
			t.Setenv("OLLAMA_HOST", core.URL)
			r := ggufRequest(ggufFixture()).WithContext(ctx)
			w := httptest.NewRecorder()
			s.importFile(w, r)
			if creates.Load() != 0 || strings.Contains(w.Body.String(), `"status":"success"`) || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatal(creates.Load(), w.Body.String())
			}
			records, _, e := s.records()
			if e != nil || len(records) != 1 || records[0].Status != "unconfirmed" {
				t.Fatal(records, e)
			}
			assertNoGGUFStage(t, s)
		})
	}
}

func TestGGUFPrivateRouteAuth(t *testing.T) {
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", t.TempDir())
	for _, path := range []string{"/api/harmony/gguf", "/api/harmony/imports"} {
		method := "POST"
		if strings.HasSuffix(path, "imports") {
			method = "GET"
		}
		for _, cookie := range []string{"", "token=wrong"} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Cookie", cookie)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
		}
	}
}

func TestGGUFReceiptExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intent.json")
	if err := harmonyImportWrite(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := harmonyImportWrite(path, []byte("overwrite")); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "original" {
		t.Fatal("overwrote intent")
	}
}
