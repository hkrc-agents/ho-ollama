//go:build harmonyos

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func downloadCall(handler http.Handler, method, path, body, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func downloadList(t *testing.T, handler http.Handler) []*harmonyDownload {
	t.Helper()
	w := downloadCall(handler, "GET", "/api/harmony/downloads", "", "token=secret")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Downloads []*harmonyDownload `json:"downloads"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Downloads
}

func storedDownloads(t *testing.T, directory string) harmonyDownloadFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "downloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file harmonyDownloadFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestDownloadAuthenticationAndValidation(t *testing.T) {
	var calls atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer core.Close()
	t.Setenv("OLLAMA_HOST", core.URL)
	directory := t.TempDir()
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	for _, method := range []string{"GET", "DELETE"} {
		for _, cookie := range []string{"", "token=wrong"} {
			w := downloadCall(handler, method, "/api/harmony/downloads", `{"model":"test"}`, cookie)
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
		}
	}
	if w := downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=wrong"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{`, `{}`, `{"model":"bad name"}`, `{"model":"test"} {}`} {
		for _, path := range []string{"/api/pull", "/api/harmony/downloads"} {
			method := "POST"
			if path != "/api/pull" {
				method = "DELETE"
			}
			if w := downloadCall(handler, method, path, body, "token=secret"); w.Code != 400 {
				t.Fatalf("%s %s: %d", path, body, w.Code)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached core")
	}
	if tasks := downloadList(t, handler); len(tasks) != 0 {
		t.Fatal(tasks)
	}
	if _, err := os.Stat(filepath.Join(directory, "downloads.json")); !os.IsNotExist(err) {
		t.Fatal("invalid request wrote records")
	}
}

func TestDownloadTerminalStatesAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, tail, state, message string
		status                     int
	}{
		{"success", "{\"status\":\"success\"}\n", "completed", "", 200},
		{"core-error", "{\"error\":\"digest mismatch\"}\n", "failed", "digest mismatch", 200},
		{"EOF", "", "failed", "without success", 200},
		{"malformed", "{oops}\n", "failed", "invalid core", 200},
		{"HTTP-error", "{\"error\":\"registry denied\"}\n", "failed", "registry denied", 403},
		{"HTTP-success-body", "{\"status\":\"success\"}\n", "failed", "core HTTP 500", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			progress := "{\"status\":\"pulling\",\"digest\":\"sha256:a\",\"total\":100,\"completed\":10}\n" +
				"{\"status\":\"pulling\",\"digest\":\"sha256:b\",\"total\":50,\"completed\":20}\n" +
				"{\"status\":\"pulling\",\"digest\":\"sha256:a\",\"completed\":90}\n"
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/pull" || r.Method != "POST" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected core request/credentials")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"model":"test:tag","stream":true}` {
					t.Errorf("body changed: %s", body)
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.Header().Set("Set-Cookie", "core=private")
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					io.WriteString(w, progress)
				}
				io.WriteString(w, tc.tail)
			}))
			defer core.Close()
			t.Setenv("OLLAMA_HOST", core.URL)
			handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
			r := httptest.NewRequest("POST", "/api/pull", strings.NewReader(`{"model":"test:tag","stream":true}`))
			r.Header.Set("Cookie", "token=secret; another=private")
			r.Header.Set("Authorization", "Bearer app-credential")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Header().Get("Set-Cookie") != "" {
				t.Fatal("core cookie exposed")
			}
			if tc.name == "success" && (w.Body.String() != progress+tc.tail || !w.Flushed) {
				t.Fatalf("stream changed: %s", w.Body.String())
			}
			tasks := downloadList(t, handler)
			if len(tasks) != 1 || tasks[0].Status != tc.state || !strings.Contains(tasks[0].Error, tc.message) {
				t.Fatalf("tasks: %+v", tasks)
			}
			task := tasks[0]
			if task.Model != "test:tag" || task.CreatedAt.IsZero() || task.UpdatedAt.Before(task.CreatedAt) {
				t.Fatalf("metadata: %+v", task)
			}
			if tc.status == 200 && (task.Digests["sha256:a"] != (harmonyDigestProgress{100, 90}) || task.Digests["sha256:b"] != (harmonyDigestProgress{50, 20})) {
				t.Fatal(task.Digests)
			}
			file := storedDownloads(t, directory)
			if file.Version != 1 || len(file.Downloads) != 1 || file.Downloads[0].Status != tc.state {
				t.Fatal(file)
			}
			info, err := os.Stat(filepath.Join(directory, "downloads.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("permissions: %v %v", info, err)
			}
			restarted := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
			if got := downloadList(t, restarted); len(got) != 1 || got[0].Status != tc.state {
				t.Fatal(got)
			}
		})
	}
}

func TestDownloadCancelConcurrencyAndResume(t *testing.T) {
	directory := t.TempDir()
	progressSent := make(chan struct{})
	var calls atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		if calls.Add(1) == 1 {
			io.WriteString(w, "{\"digest\":\"sha256:a\",\"total\":100,\"completed\":37}\n")
			w.(http.Flusher).Flush()
			close(progressSent)
			<-r.Context().Done()
		} else {
			io.WriteString(w, "{\"digest\":\"sha256:a\",\"total\":100,\"completed\":100}\n{\"status\":\"success\"}\n")
		}
	}))
	defer core.Close()
	t.Setenv("OLLAMA_HOST", core.URL)
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	app := httptest.NewServer(handler)
	defer app.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", app.URL+"/api/pull", strings.NewReader(`{"model":"test"}`))
	r.Header.Set("Cookie", "token=secret")
	response, err := app.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line := make([]byte, len("{\"digest\":\"sha256:a\",\"total\":100,\"completed\":37}\n"))
	if _, err := io.ReadFull(response.Body, line); err != nil {
		t.Fatal(err)
	}
	<-progressSent
	tasks := downloadList(t, handler)
	if len(tasks) != 1 || tasks[0].Status != "downloading" || tasks[0].Digests["sha256:a"].Completed != 37 {
		t.Fatal(tasks)
	}
	createdAt := tasks[0].CreatedAt
	for _, method := range []string{"POST", "DELETE"} {
		path := "/api/pull"
		if method == "DELETE" {
			path = "/api/harmony/downloads"
		}
		w := downloadCall(handler, method, path, `{"model":"registry.ollama.ai/library/test:latest"}`, "token=secret")
		if w.Code != 409 {
			t.Fatalf("active %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		tasks = downloadList(t, handler)
		if tasks[0].Status == "paused" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled task was not paused")
		}
		time.Sleep(time.Millisecond)
	}
	if tasks[0].Error != "" || tasks[0].Digests["sha256:a"].Completed != 37 {
		t.Fatal(tasks[0])
	}
	if storedDownloads(t, directory).Downloads[0].Status != "paused" {
		t.Fatal("cancel not persisted")
	}
	restarted := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	w := downloadCall(restarted, "POST", "/api/pull", `{"model":"test:latest"}`, "token=secret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatal(w.Body.String())
	}
	tasks = downloadList(t, restarted)
	if len(tasks) != 1 || tasks[0].Status != "completed" || !tasks[0].CreatedAt.Equal(createdAt) || tasks[0].Digests["sha256:a"].Completed != 100 || calls.Load() != 2 {
		t.Fatal(tasks)
	}
}

func TestDownloadRestartAndForgetOnlyRecord(t *testing.T) {
	directory := t.TempDir()
	s := openHarmonyDownloads(directory)
	key, _, err := s.begin("test")
	if err != nil {
		t.Fatal(err)
	}
	completed := int64(19)
	if err := s.progress(key, harmonyPullEvent{Digest: "sha256:a", Completed: &completed}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	err = s.saveLocked() // crash snapshot; no finish
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(directory, "blob-do-not-delete")
	if err := os.WriteFile(blob, []byte("owned blob"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	tasks := downloadList(t, handler)
	if len(tasks) != 1 || tasks[0].Status != "paused" || tasks[0].Digests["sha256:a"].Completed != 19 {
		t.Fatal(tasks)
	}
	if storedDownloads(t, directory).Downloads[0].Status != "paused" {
		t.Fatal("recovery not persisted")
	}
	w := downloadCall(handler, "DELETE", "/api/harmony/downloads", `{"model":"test:latest"}`, "token=secret")
	if w.Code != 204 {
		t.Fatalf("forget: %d %s", w.Code, w.Body.String())
	}
	if len(downloadList(t, handler)) != 0 || len(storedDownloads(t, directory).Downloads) != 0 {
		t.Fatal("record retained")
	}
	if data, err := os.ReadFile(blob); err != nil || string(data) != "owned blob" {
		t.Fatal("blob changed")
	}
	if w := downloadCall(handler, "DELETE", "/api/harmony/downloads", `{"model":"test"}`, "token=secret"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if got := downloadList(t, withModelManagementStore(http.NotFoundHandler(), "secret", directory)); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestDownloadRetryChangedManifestDoesNotMixOldLayers(t *testing.T) {
	s := openHarmonyDownloads(t.TempDir())
	key, _, err := s.begin("test:latest")
	if err != nil {
		t.Fatal(err)
	}
	total, partial, complete := int64(100), int64(37), int64(100)
	if err := s.progress(key, harmonyPullEvent{Digest: "sha256:old", Total: &total, Completed: &partial}); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(key, "paused", ""); err != nil {
		t.Fatal(err)
	}
	created := s.tasks[key].CreatedAt
	if s.tasks[key].Digests["sha256:old"].Completed != 37 {
		t.Fatal("paused progress lost")
	}
	if _, _, err = s.begin("test:latest"); err != nil {
		t.Fatal(err)
	}
	if len(s.tasks[key].Digests) != 0 || !s.tasks[key].CreatedAt.Equal(created) {
		t.Fatal("retry inherited an unverified manifest summary")
	}
	if err := s.progress(key, harmonyPullEvent{Digest: "sha256:new", Total: &total, Completed: &complete}); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(key, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if len(s.tasks[key].Digests) != 1 || s.tasks[key].Digests["sha256:new"].Completed != 100 {
		t.Fatal("old/new layer progress mixed")
	}
}

func TestDownloadPersistenceFailures(t *testing.T) {
	t.Run("initialization", func(t *testing.T) {
		for _, content := range []string{"{broken", `{"version":99}`, `{"version":1,"downloads":[null]}`} {
			directory := t.TempDir()
			path := filepath.Join(directory, "downloads.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
			for _, call := range []struct{ method, path string }{{"GET", "/api/harmony/downloads"}, {"DELETE", "/api/harmony/downloads"}, {"POST", "/api/pull"}} {
				w := downloadCall(handler, call.method, call.path, `{"model":"test"}`, "token=secret")
				if w.Code != 503 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if data, _ := os.ReadFile(path); string(data) != content {
				t.Fatal("corrupt records overwritten")
			}
		}
	})
	t.Run("start", func(t *testing.T) {
		directory := t.TempDir()
		handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
		if err := os.Mkdir(filepath.Join(directory, "downloads.json"), 0700); err != nil {
			t.Fatal(err)
		}
		w := downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=secret")
		if w.Code != 503 || strings.Contains(w.Body.String(), `"success"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	})
	t.Run("final", func(t *testing.T) {
		directory := t.TempDir()
		core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Force rename failure after the start record is successfully written.
			path := filepath.Join(directory, "downloads.json")
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Error(err)
			}
			io.WriteString(w, "{\"status\":\"success\"}\n")
		}))
		defer core.Close()
		t.Setenv("OLLAMA_HOST", core.URL)
		handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
		w := downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=secret")
		if w.Code != 503 || strings.Contains(w.Body.String(), `"status":"success"`) || !strings.Contains(w.Body.String(), "persist") {
			t.Fatal(w.Code, w.Body.String())
		}
		if w := downloadCall(handler, "GET", "/api/harmony/downloads", "", "token=secret"); w.Code != 503 {
			t.Fatal("false durable list", w.Code)
		}
		files, err := filepath.Glob(filepath.Join(directory, ".downloads-*.tmp"))
		if err != nil || len(files) != 0 {
			t.Fatal("temporary files leaked", files, err)
		}
	})
	t.Run("forget", func(t *testing.T) {
		directory := t.TempDir()
		s := openHarmonyDownloads(directory)
		key, _, err := s.begin("test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.finish(key, "failed", "test error"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(directory, "downloads.json"), filepath.Join(directory, "saved.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(directory, "downloads.json"), 0700); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.forget(w, httptest.NewRequest("DELETE", "/api/harmony/downloads", strings.NewReader(`{"model":"test"}`)))
		if w.Code != 503 || s.tasks[key] == nil {
			t.Fatal("failed forget lost record")
		}
	})
}

func TestDownloadProgressWriteThrottle(t *testing.T) {
	directory := t.TempDir()
	s := openHarmonyDownloads(directory)
	key, _, err := s.begin("test")
	if err != nil {
		t.Fatal(err)
	}
	// No sleep-based assertion: hold the write clock in the future, then expire it.
	s.lastWrite = time.Now().Add(time.Hour)
	for completed := int64(1); completed <= 100; completed++ {
		if err := s.progress(key, harmonyPullEvent{Digest: "sha256:a", Completed: &completed}); err != nil {
			t.Fatal(err)
		}
	}
	if len(storedDownloads(t, directory).Downloads[0].Digests) != 0 {
		t.Fatal("progress writes not throttled")
	}
	s.lastWrite = time.Time{}
	completed := int64(101)
	if err := s.progress(key, harmonyPullEvent{Digest: "sha256:a", Completed: &completed}); err != nil {
		t.Fatal(err)
	}
	if got := storedDownloads(t, directory).Downloads[0].Digests["sha256:a"].Completed; got != 101 {
		t.Fatal(got)
	}
	s.lastWrite = time.Now().Add(time.Hour)
	completed = 102
	if err := s.progress(key, harmonyPullEvent{Digest: "sha256:a", Completed: &completed}); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(key, "paused", ""); err != nil {
		t.Fatal(err)
	}
	if got := storedDownloads(t, directory).Downloads[0]; got.Status != "paused" || got.Digests["sha256:a"].Completed != 102 {
		t.Fatal(got)
	}
}

func TestDownloadCoreDisconnectAndRetry(t *testing.T) {
	directory := t.TempDir()
	var calls atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		if calls.Add(1) == 1 {
			io.WriteString(w, "{\"digest\":\"sha256:a\",\"total\":100,\"completed\":31}\n")
			w.(http.Flusher).Flush()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close() // incomplete HTTP chunked body, not a clean EOF
			return
		}
		io.WriteString(w, "{\"status\":\"success\"}\n")
	}))
	defer core.Close()
	t.Setenv("OLLAMA_HOST", core.URL)
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	w := downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=secret")
	if !strings.Contains(w.Body.String(), "core stream interrupted") || strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatal(w.Body.String())
	}
	tasks := downloadList(t, handler)
	if len(tasks) != 1 || tasks[0].Status != "failed" || tasks[0].Digests["sha256:a"].Completed != 31 {
		t.Fatal(tasks)
	}
	if storedDownloads(t, directory).Downloads[0].Status != "failed" {
		t.Fatal("interruption not persisted")
	}
	w = downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=secret")
	if w.Code != 200 || downloadList(t, handler)[0].Status != "completed" || calls.Load() != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDownloadCoreUnavailable(t *testing.T) {
	core := httptest.NewServer(http.NotFoundHandler())
	core.Close()
	t.Setenv("OLLAMA_HOST", core.URL)
	directory := t.TempDir()
	handler := withModelManagementStore(http.NotFoundHandler(), "secret", directory)
	w := downloadCall(handler, "POST", "/api/pull", `{"model":"test"}`, "token=secret")
	if w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	if task := downloadList(t, handler)[0]; task.Status != "failed" || task.Error != "core request failed" {
		t.Fatal(task)
	}
	if storedDownloads(t, directory).Downloads[0].Status != "failed" {
		t.Fatal("transport failure not persisted")
	}
}

func TestDownloadNonStreamingAndUnterminatedProgress(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-success", true: "non-streaming-success"}[success], func(t *testing.T) {
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if success {
					io.WriteString(w, `{"status":"success"}`)
				} else {
					io.WriteString(w, `{"digest":"sha256:a","completed":12}`)
				}
			}))
			defer core.Close()
			t.Setenv("OLLAMA_HOST", core.URL)
			handler := withModelManagementStore(http.NotFoundHandler(), "secret", t.TempDir())
			w := downloadCall(handler, "POST", "/api/pull", `{"name":"test","stream":false}`, "token=secret")
			decoder := json.NewDecoder(w.Body)
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if success {
				if event["status"] != "success" || downloadList(t, handler)[0].Status != "completed" {
					t.Fatal(event)
				}
			} else {
				if err := decoder.Decode(&event); err != nil || event["error"] == nil {
					t.Fatal("invalid NDJSON error", event, err)
				}
				if downloadList(t, handler)[0].Status != "failed" {
					t.Fatal("missing success counted as completion")
				}
			}
		})
	}
}

// Inject one bounded syscall outcome while operating on real private temp files.
// This never fills a disk, changes process credentials, or touches model blobs.
type harmonyJournalFault struct {
	stage        string
	nth          int
	calls        int
	injected     bool
	partialBytes int
	temps        []string
}

func (f *harmonyJournalFault) hit(stage string) bool {
	if stage != f.stage {
		return false
	}
	f.calls++
	if f.calls == f.nth {
		f.injected = true
		return true
	}
	return false
}

type harmonyJournalFaultFile struct {
	*os.File
	fault     *harmonyJournalFault
	directory bool
}

func (f *harmonyJournalFaultFile) Write(data []byte) (int, error) {
	if f.fault.hit("write_partial") {
		n, err := f.File.Write(data[:len(data)/2])
		f.fault.partialBytes = n
		return n, errors.Join(err, syscall.ENOSPC)
	}
	if f.fault.hit("write_short") {
		n, err := f.File.Write(data[:len(data)/2])
		f.fault.partialBytes = n
		return n, err
	}
	return f.File.Write(data)
}
func (f *harmonyJournalFaultFile) Sync() error {
	stage := "file_sync"
	if f.directory {
		stage = "directory_sync"
	}
	if f.fault.hit(stage) {
		return syscall.ENOSPC
	}
	return f.File.Sync()
}
func (f *harmonyJournalFaultFile) Close() error {
	err := f.File.Close() // Actually close even when reporting an injected error.
	if !f.directory && f.fault.hit("close") {
		return errors.Join(err, syscall.ENOSPC)
	}
	return err
}
func (f *harmonyJournalFault) filesystem() harmonyJournalFS {
	return harmonyJournalFS{
		createTemp: func(dir, pattern string) (harmonyJournalFile, error) {
			if f.hit("create") {
				return nil, syscall.ENOSPC
			}
			file, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			f.temps = append(f.temps, file.Name())
			return &harmonyJournalFaultFile{File: file, fault: f}, nil
		},
		rename: func(old, new string) error {
			if f.hit("rename") {
				return syscall.ENOSPC
			}
			return os.Rename(old, new)
		},
		remove: os.Remove,
		openDirectory: func(path string) (harmonyJournalDirectory, error) {
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			return &harmonyJournalFaultFile{File: file, fault: f, directory: true}, nil
		},
	}
}
func assertHarmonyNoJournalTemps(t *testing.T, directory string, fault *harmonyJournalFault) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(directory, ".downloads-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary journals leaked", files, err)
	}
	for _, path := range fault.temps {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("temporary journal still exists", path, err)
		}
	}
}
func assertHarmonyJournalError(t *testing.T, s *harmonyDownloads, fault *harmonyJournalFault, err error) {
	t.Helper()
	want := error(syscall.ENOSPC)
	if fault.stage == "write_short" {
		want = io.ErrShortWrite
	}
	if !fault.injected || !errors.Is(err, want) || !errors.Is(s.writeErr, want) {
		t.Fatal("lost actual journal error", fault, err, s.writeErr)
	}
	var saveErr *harmonyJournalSaveError
	if !errors.As(err, &saveErr) || saveErr.renamed != (fault.stage == "directory_sync") {
		t.Fatal("incorrect journal commit phase", err)
	}
	wantStage := strings.ReplaceAll(fault.stage, "_", " ")
	if fault.stage == "write_partial" || fault.stage == "write_short" {
		wantStage = "write"
	}
	if fault.stage == "close" {
		wantStage = "file close"
	}
	if saveErr.stage != wantStage {
		t.Fatal("lost failing syscall context", saveErr.stage, wantStage)
	}
	if (fault.stage == "write_partial" || fault.stage == "write_short") && fault.partialBytes <= 0 {
		t.Fatal("partial-write test wrote no real bytes")
	}
	w := httptest.NewRecorder()
	s.list(w, httptest.NewRequest("GET", "/api/harmony/downloads", nil))
	if w.Code != 503 {
		t.Fatal("failed persistence reported durable list", w.Code, w.Body.String())
	}
}

func TestDownloadJournalRealFileFaultsRollbackRetryAndUncertainCommit(t *testing.T) {
	for _, stage := range []string{"create", "write_partial", "write_short", "file_sync", "close", "rename", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			for _, action := range []string{"new-begin", "retry-begin", "forget"} {
				t.Run(action, func(t *testing.T) {
					directory := t.TempDir()
					s := openHarmonyDownloads(directory)
					key, _, err := s.begin("test")
					if err != nil {
						t.Fatal(err)
					}
					if err = s.finish(key, "paused", ""); err != nil {
						t.Fatal(err)
					}
					previous := s.tasks[key]
					before, err := os.ReadFile(s.path)
					if err != nil {
						t.Fatal(err)
					}
					lastWrite := s.lastWrite
					blob := filepath.Join(directory, "blob-do-not-touch")
					if err = os.WriteFile(blob, []byte("model sentinel"), 0600); err != nil {
						t.Fatal(err)
					}
					fault := &harmonyJournalFault{stage: stage, nth: 1}
					s.journalFS = fault.filesystem()
					name := "test"
					if action == "new-begin" {
						name = "other"
					}
					if action == "forget" {
						w := httptest.NewRecorder()
						s.forget(w, httptest.NewRequest("DELETE", "/api/harmony/downloads", strings.NewReader(`{"model":"test"}`)))
						if w.Code != 503 {
							t.Fatal(w.Code, w.Body.String())
						}
						err = s.writeErr
					} else {
						_, status, e := s.begin(name)
						err = e
						if status != 503 {
							t.Fatal(status, e)
						}
					}
					assertHarmonyJournalError(t, s, fault, err)
					if s.tasks[key] != previous || len(s.tasks) != 1 || len(s.active) != 0 || !s.lastWrite.Equal(lastWrite) {
						t.Fatal("failed operation did not restore RAM/lease/clock")
					}
					after, e := os.ReadFile(s.path)
					if e != nil {
						t.Fatal(e)
					}
					if stage != "directory_sync" && !bytes.Equal(before, after) {
						t.Fatal("pre-rename failure changed old journal")
					}
					if stage == "directory_sync" {
						// Rename already happened. Only in-memory state was rolled
						// back; the replacement is visible, durability is unknown.
						if bytes.Equal(before, after) {
							t.Fatal("post-rename test did not expose replacement")
						}
						var visible harmonyDownloadFile
						if e = json.Unmarshal(after, &visible); e != nil {
							t.Fatal(e)
						}
						if action == "forget" && len(visible.Downloads) != 0 {
							t.Fatal("forget replacement not visible")
						}
						if action == "new-begin" && len(visible.Downloads) != 2 {
							t.Fatal("begin replacement not visible")
						}
						if action == "retry-begin" && (len(visible.Downloads) != 1 || visible.Downloads[0].Status != "downloading") {
							t.Fatal("retry replacement not visible")
						}
					}
					assertHarmonyNoJournalTemps(t, directory, fault)
					// One-shot fault is exhausted: explicit retry must commit and
					// clear the error. Reopen checks visible recovery, not power loss.
					if action == "forget" {
						w := httptest.NewRecorder()
						s.forget(w, httptest.NewRequest("DELETE", "/api/harmony/downloads", strings.NewReader(`{"model":"test"}`)))
						if w.Code != 204 {
							t.Fatal(w.Code, w.Body.String())
						}
					} else {
						retry, _, e := s.begin(name)
						if e != nil {
							t.Fatal("retry failed", e)
						}
						if e = s.finish(retry, "paused", ""); e != nil {
							t.Fatal(e)
						}
					}
					if s.writeErr != nil {
						t.Fatal("successful retry retained failure", s.writeErr)
					}
					reopened := openHarmonyDownloads(directory)
					if reopened.initErr != nil || reopened.writeErr != nil {
						t.Fatal("reopen failed", reopened.initErr, reopened.writeErr)
					}
					want := 1
					if action == "forget" {
						want = 0
					}
					if action == "new-begin" {
						want = 2
					}
					if len(reopened.tasks) != want {
						t.Fatal("retry/reopen state mismatch", reopened.tasks)
					}
					if b, e := os.ReadFile(blob); e != nil || string(b) != "model sentinel" {
						t.Fatal("model sentinel changed", e)
					}
					assertHarmonyNoJournalTemps(t, directory, fault)
				})
			}
		})
	}
}

type harmonyJournalFirstFrame struct {
	*httptest.ResponseRecorder
	store *harmonyDownloads
	armed bool
}

func (w *harmonyJournalFirstFrame) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if !w.armed {
		w.armed = true
		w.store.mu.Lock()
		w.store.lastWrite = time.Time{}
		w.store.mu.Unlock()
	}
	return n, err
}

func TestDownloadJournalStreamFaultsRemainStorageErrors(t *testing.T) {
	for _, stage := range []string{"write_partial", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			for _, phase := range []string{"progress-before-headers", "progress-after-frame", "terminal-before-headers", "terminal-after-frame"} {
				t.Run(phase, func(t *testing.T) {
					directory := t.TempDir()
					s := openHarmonyDownloads(directory)
					fault := &harmonyJournalFault{stage: stage, nth: 2}
					s.journalFS = fault.filesystem()
					core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/x-ndjson")
						s.mu.Lock()
						if phase == "progress-before-headers" {
							s.lastWrite = time.Time{}
						} else {
							// Deterministic throttle: no wall-clock delay or scheduler load
							// may move the fault before the intended response boundary.
							s.lastWrite = time.Now().Add(time.Hour)
						}
						s.mu.Unlock()
						if phase != "terminal-before-headers" {
							io.WriteString(w, "{\"digest\":\"sha256:a\",\"completed\":12}\n")
						}
						if strings.HasPrefix(phase, "progress-") {
							io.WriteString(w, "{\"digest\":\"sha256:a\",\"completed\":24}\n")
						}
						io.WriteString(w, "{\"status\":\"success\"}\n")
					}))
					defer core.Close()
					t.Setenv("OLLAMA_HOST", core.URL)
					recorder := httptest.NewRecorder()
					var writer http.ResponseWriter = recorder
					if phase == "progress-after-frame" {
						writer = &harmonyJournalFirstFrame{ResponseRecorder: recorder, store: s}
					}
					s.pull(writer, httptest.NewRequest("POST", "/api/pull", strings.NewReader(`{"model":"test"}`)))
					want := 503
					if strings.HasSuffix(phase, "after-frame") {
						want = 200
					}
					if recorder.Code != want || strings.Contains(recorder.Body.String(), `"status":"success"`) || !strings.Contains(recorder.Body.String(), "cannot persist download") {
						t.Fatal("storage failure became core error or forwarded success", recorder.Code, recorder.Body.String())
					}
					if !fault.injected {
						t.Fatal("storage fault was not exercised")
					}
					list := httptest.NewRecorder()
					s.list(list, httptest.NewRequest("GET", "/api/harmony/downloads", nil))
					if strings.HasPrefix(phase, "progress-") {
						if s.writeErr != nil || list.Code != 200 {
							t.Fatal("successful failed-state save hid the row needed for retry", s.writeErr, list.Code)
						}
						var visible struct {
							Downloads []*harmonyDownload `json:"downloads"`
						}
						if err := json.Unmarshal(list.Body.Bytes(), &visible); err != nil || len(visible.Downloads) != 1 || visible.Downloads[0].Status != "failed" {
							t.Fatal("retry list did not expose the failed task", list.Body.String(), err)
						}
						if fault.calls != 3 {
							t.Fatal("test did not exercise successful finishing save after failed progress", fault.calls)
						}
						if tasks := storedDownloads(t, directory).Downloads; len(tasks) != 1 || tasks[0].Status != "failed" || tasks[0].Error != "cannot persist download progress" {
							t.Fatal("storage failure misclassified", tasks)
						}
					} else if !errors.Is(s.writeErr, syscall.ENOSPC) || list.Code != 503 {
						t.Fatal("unpersisted terminal state was exposed as durable", s.writeErr, list.Code)
					}
					if stage == "directory_sync" && strings.HasPrefix(phase, "terminal-") {
						// The completed replacement is visible, but no durable success
						// may be forwarded when its directory sync failed.
						var saveErr *harmonyJournalSaveError
						if !errors.As(s.writeErr, &saveErr) || !saveErr.renamed {
							t.Fatal("terminal commit uncertainty was lost", s.writeErr)
						}
						if tasks := storedDownloads(t, directory).Downloads; len(tasks) != 1 || tasks[0].Status != "completed" {
							t.Fatal("test did not observe the renamed completed journal", tasks)
						}
					}
					assertHarmonyNoJournalTemps(t, directory, fault)
					retry, _, err := s.begin("test")
					if err != nil {
						t.Fatal(err)
					}
					if err = s.finish(retry, "paused", ""); err != nil {
						t.Fatal(err)
					}
					if s.writeErr != nil {
						t.Fatal(s.writeErr)
					}
					if reopened := openHarmonyDownloads(directory); reopened.initErr != nil || reopened.writeErr != nil {
						t.Fatal("retry/reopen failed", reopened.initErr, reopened.writeErr)
					}
				})
			}
		})
	}
}
