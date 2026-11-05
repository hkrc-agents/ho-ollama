//go:build harmonyos

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// HOST FIXTURE ONLY: these booleans simulate a native authorization record.
// They do not obtain a platform grant or establish persisted device access.
func storageFixture(t *testing.T) (string, storageSelection) {
	t.Helper()
	files := t.TempDir()
	data := filepath.Join(files, "desktop")
	folder := filepath.Join(t.TempDir(), "selected")
	if e := os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(folder, 0700); e != nil {
		t.Fatal(e)
	}
	s := storageSelection{Schema: 1, ID: "host-fixture-only", URI: "file://host-fixture-only", Path: folder,
		OperationMode: 31, Persisted: true, Activated: true, Time: 1792454400000}
	storageWriteSelection(t, data, s)
	return data, s
}
func storageWriteSelection(t *testing.T, data string, s storageSelection) {
	t.Helper()
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(filepath.Dir(data), "model-directory-selection.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func storageRequest(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "token", Value: token})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestStorageSelectionPrivateAndExactNativeRecord(t *testing.T) {
	data, s := storageFixture(t)
	h := storageProbeHandler(http.NotFoundHandler(), "secret", data, context.Background(), storageProbeRunner{}, nil)
	for _, method := range []string{"GET", "POST"} {
		path := "/api/harmony/storage/selection"
		if method == "POST" {
			path = "/api/harmony/storage/probe"
		}
		if w := storageRequest(h, method, path, `{}`, ""); w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := storageRequest(h, "GET", "/api/harmony/storage/selection", "", "secret")
	var got storageSelection
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ID != s.ID || got.Path != s.Path {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"selection_id":"other"}`, `{"selection_id":"host-fixture-only","path":"/tmp"}`, `{} {}`, `{}`} {
		w := storageRequest(h, "POST", "/api/harmony/storage/probe", body, "secret")
		if w.Code != 400 && w.Code != 409 {
			t.Fatal("unsafe request accepted", w.Code, w.Body.String())
		}
	}
	if entries, e := os.ReadDir(s.Path); e != nil || len(entries) != 0 {
		t.Fatal("rejected request modified selected folder", e, entries)
	}
}
func TestStorageSelectionRejectsInactiveOrFailedAuthorization(t *testing.T) {
	_, s := storageFixture(t)
	for _, mutate := range []func(*storageSelection){
		func(s *storageSelection) { s.Schema = 2 }, func(s *storageSelection) { s.Persisted = false },
		func(s *storageSelection) { s.Activated = false }, func(s *storageSelection) { s.Error = "activation denied" },
		func(s *storageSelection) { s.Path = "relative" }, func(s *storageSelection) { s.URI = "" },
		func(s *storageSelection) { s.OperationMode = 7 }, func(s *storageSelection) { s.Time = 0 },
	} {
		bad := s
		mutate(&bad)
		if e := validStorageSelection(bad, s.ID); e == nil {
			t.Fatal("invalid authorization accepted", bad)
		}
	}
}

func TestStorageReadWritePolicyStillRequiresFullFilesystemEvidence(t *testing.T) {
	_, selection := storageFixture(t)
	selection.OperationMode = 3
	if err := validStorageSelection(selection, selection.ID); err != nil {
		t.Fatal(err)
	}
	if err := storageEvidence([]storageOperation{{Name: "mkdir", OK: true}}); err == nil {
		t.Fatal("read/write policy must not qualify incomplete filesystem evidence")
	}
	for _, mode := range []int{0, 1, 2, 7, 15, 32} {
		selection.OperationMode = mode
		if err := validStorageSelection(selection, selection.ID); err == nil {
			t.Fatalf("unexpected policy mode admitted: %d", mode)
		}
	}
}
func TestStorageGoActualIOAndScopedCleanup(t *testing.T) {
	data, s := storageFixture(t)
	// Force native launch failure: real Go I/O still runs and evidence must fail.
	sentinel := filepath.Join(s.Path, "user-model.gguf")
	if e := os.WriteFile(sentinel, []byte("untouched-user-data"), 0600); e != nil {
		t.Fatal(e)
	}
	runner := storageProbeRunner{helper: "/missing/pico-storage-probe", launcher: "/missing/pico-service-launcher"}
	h := storageProbeHandler(http.NotFoundHandler(), "secret", data, context.Background(), runner, nil)
	w := storageRequest(h, "POST", "/api/harmony/storage/probe", `{"selection_id":"host-fixture-only"}`, "secret")
	var result storageProbeResult
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Passed || result.Error == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	if e := storageEvidence(result.Operations); e != nil {
		t.Fatal("Go regular file operations failed", e, result.Operations)
	}
	if b, e := os.ReadFile(sentinel); e != nil || string(b) != "untouched-user-data" {
		t.Fatal("user data changed", e)
	}
	entries, e := os.ReadDir(s.Path)
	if e != nil || len(entries) != 1 || entries[0].Name() != "user-model.gguf" {
		t.Fatal("probe cleanup incomplete", e, entries)
	}
	b, e := os.ReadFile(filepath.Join(data, "storage-probe-result.json"))
	if e != nil || len(b) > 65536 {
		t.Fatal("bounded result not persisted", e)
	}
	var saved storageProbeResult
	if e = json.Unmarshal(b, &saved); e != nil || saved.Passed || saved.SelectionID != s.ID {
		t.Fatal("failed evidence became success", e)
	}
}
func TestStorageExistingFileNeverAdoptedForCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	if e := os.WriteFile(path, []byte("unowned"), 0600); e != nil {
		t.Fatal(e)
	}
	fd, e := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	ops, owned := storageGoIO(fd)
	if len(owned) != 0 || len(ops) != 1 || ops[0].Errno != int(syscall.EEXIST) {
		t.Fatal("adopted an existing file", ops, owned)
	}
	if b, e := os.ReadFile(path); e != nil || string(b) != "unowned" {
		t.Fatal("unowned file modified", e)
	}
}
func TestStorageEvidenceMissingDuplicateAndUnsupported(t *testing.T) {
	good := []storageOperation{}
	for _, name := range storageRequired {
		good = append(good, storageOperation{Name: name, OK: true})
	}
	if e := storageEvidence(good); e != nil {
		t.Fatal(e)
	}
	if e := storageEvidence(good[:len(good)-1]); e == nil {
		t.Fatal("missing cleanup accepted")
	}
	if e := storageEvidence(append(append([]storageOperation{}, good...), good[0])); e == nil {
		t.Fatal("duplicate operation accepted")
	}
	for i, o := range good {
		if o.Name == "dirsync" || o.Name == "statfs" {
			good[i] = storageOperation{Name: o.Name, Errno: int(syscall.EOPNOTSUPP), Unsupported: true}
		}
	}
	if e := storageEvidence(good); e != nil {
		t.Fatal("unsupported optional operation rejected", e)
	}
	bad := append([]storageOperation{}, good...)
	bad[0] = storageOperation{Name: bad[0].Name, Errno: int(syscall.EOPNOTSUPP), Unsupported: true}
	if e := storageEvidence(bad); e == nil {
		t.Fatal("unsupported mandatory operation accepted")
	}
	bad = append([]storageOperation{}, good...)
	bad[1].Errno = int(syscall.EIO)
	if e := storageEvidence(bad); e == nil {
		t.Fatal("contradictory success accepted")
	}
}
func TestStorageProbeOutputBoundAndShutdown(t *testing.T) {
	var b storageBoundedBuffer
	n, e := b.Write([]byte(strings.Repeat("x", 32768)))
	if n != 32768 || e != nil || !b.overflow || b.Len() != 16384 {
		t.Fatal(n, e, b.Len())
	}
	data, s := storageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := storageProbeHandler(http.NotFoundHandler(), "secret", data, ctx, storageProbeRunner{}, nil)
	w := storageRequest(h, "POST", "/api/harmony/storage/probe", `{"selection_id":"host-fixture-only"}`, "secret")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if entries, e := os.ReadDir(s.Path); e != nil || len(entries) != 0 {
		t.Fatal("shutdown probe wrote files", e)
	}
}
func TestStorageSelectionRecordBound(t *testing.T) {
	data, _ := storageFixture(t)
	if e := os.WriteFile(filepath.Join(filepath.Dir(data), "model-directory-selection.json"), []byte(strings.Repeat(" ", 16385)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readStorageSelection(data); e == nil {
		t.Fatal("oversized selection accepted")
	}
}

// An external host runner may provide the actual separately compiled C ELF and
// launcher. This test does not compile them, fake native evidence, or claim a
// device authorization pass. Missing fixtures remain an explicit skip.
func TestStorageActualNativeELFHostFixture(t *testing.T) {
	helper := os.Getenv("PICO_STORAGE_PROBE_BINARY")
	launcher := os.Getenv("PICO_STORAGE_PROBE_LAUNCHER")
	if helper == "" || launcher == "" {
		t.Skip("actual storage probe ELF and launcher host fixtures not provided")
	}
	_, s := storageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := (storageProbeRunner{helper: helper, launcher: launcher}).run(ctx, s)
	if !result.Passed || result.Native == nil {
		t.Fatal("host fixture probe failed (not device evidence)", result)
	}
	if e := storageEvidence(result.Operations); e != nil {
		t.Fatal(e)
	}
	if e := storageEvidence(result.Native.Operations); e != nil {
		t.Fatal(e)
	}
	if entries, e := os.ReadDir(s.Path); e != nil || len(entries) != 0 {
		t.Fatal("native cleanup incomplete", e, entries)
	}
}
func TestStorageNativeCancelledContextCannotPass(t *testing.T) {
	dir := t.TempDir()
	fd, e := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, e = (storageProbeRunner{helper: "/missing/helper", launcher: "/missing/launcher"}).native(ctx, fd, st, "host")
	if !errors.Is(e, context.Canceled) {
		t.Fatal("cancelled native probe accepted", e)
	}
}

func TestStorageNativeIdentityAndMissingControlsFailClosed(t *testing.T) {
	parent := unix.Stat_t{Ino: 42}
	good := storageNativeResult{Schema: 1, PID: 1234, PPID: os.Getpid(), UID: os.Getuid(), Domain: "host-fixture-only", ParentDev: uint64(parent.Dev), ParentIno: 42}
	for _, name := range append(append([]string{}, storageRequired...), "domain", "inherited_directory") {
		good.Operations = append(good.Operations, storageOperation{Name: name, OK: true})
	}
	if e := validateStorageNative(&good, 1234, parent, "host-fixture-only"); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*storageNativeResult){
		func(r *storageNativeResult) { r.PID++ }, func(r *storageNativeResult) { r.PPID++ }, func(r *storageNativeResult) { r.UID++ },
		func(r *storageNativeResult) { r.Domain = "" }, func(r *storageNativeResult) { r.ParentIno++ },
		func(r *storageNativeResult) { r.Operations = r.Operations[:len(r.Operations)-1] },
	} {
		bad := good
		change(&bad)
		if e := validateStorageNative(&bad, 1234, parent, "host-fixture-only"); e == nil {
			t.Fatal("invalid native evidence accepted", bad)
		}
	}
	if e := validateStorageNative(nil, 1234, parent, "host-fixture-only"); e == nil {
		t.Fatal("missing native result accepted")
	}
}

var _ io.Writer = (*storageBoundedBuffer)(nil)
