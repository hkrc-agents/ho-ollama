//go:build harmonyos

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This is an authorization/storage probe only. It never changes OLLAMA_MODELS
// or treats a host fixture's authorization booleans as device evidence.
type storageSelection struct {
	Schema         int    `json:"schema"`
	ID             string `json:"id"`
	URI            string `json:"uri"`
	Path           string `json:"path"`
	OperationMode  int    `json:"operation_mode"`
	Persisted      bool   `json:"persisted"`
	Activated      bool   `json:"activated"`
	Time           int64  `json:"time"`
	Error          string `json:"error,omitempty"`
	CheckedModes   []int  `json:"checked_modes,omitempty"`
	CheckedResults []bool `json:"checked_results,omitempty"`
}
type storageOperation struct {
	Name        string `json:"name"`
	OK          bool   `json:"ok"`
	Errno       int    `json:"errno"`
	Error       string `json:"error,omitempty"`
	Unsupported bool   `json:"unsupported,omitempty"`
}
type storageNativeResult struct {
	Schema     int                `json:"schema"`
	PID        int                `json:"pid"`
	PPID       int                `json:"ppid"`
	UID        int                `json:"uid"`
	Domain     string             `json:"domain"`
	ParentDev  uint64             `json:"parent_dev"`
	ParentIno  uint64             `json:"parent_ino"`
	Operations []storageOperation `json:"operations"`
}
type storageProbeResult struct {
	Schema      int                  `json:"schema"`
	SelectionID string               `json:"selection_id"`
	Time        string               `json:"time"`
	Passed      bool                 `json:"passed"`
	Directory   string               `json:"probe_directory,omitempty"`
	PID         int                  `json:"pid"`
	UID         int                  `json:"uid"`
	Domain      string               `json:"domain"`
	Operations  []storageOperation   `json:"operations"`
	Native      *storageNativeResult `json:"native,omitempty"`
	ChildStderr string               `json:"child_stderr,omitempty"`
	Error       string               `json:"error,omitempty"`
}

func readStorageSelection(directory string) (storageSelection, error) {
	var s storageSelection
	f, err := os.Open(filepath.Join(filepath.Dir(directory), "model-directory-selection.json"))
	if err != nil {
		return s, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return s, err
	}
	if len(b) > 16384 {
		return s, errors.New("selection record exceeds limit")
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}
func validStorageSelection(s storageSelection, id string) error {
	if s.Schema != 1 || id == "" || s.ID != id || !s.Persisted || !s.Activated || s.Error != "" ||
		s.URI == "" || (s.OperationMode != 31 && s.OperationMode != 3) || s.Time <= 0 ||
		!filepath.IsAbs(s.Path) || strings.ContainsRune(s.Path, 0) {
		return errors.New("exact active persistent native selection with no error required")
	}
	return nil
}
func storageDomain() (string, error) {
	b, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return "", err
	}
	s := strings.Trim(string(b), "\x00\r\n ")
	if s == "" || len(s) > 255 {
		return "", errors.New("missing or oversized process domain")
	}
	return s, nil
}
func storageErrno(err error) int {
	var n syscall.Errno
	if errors.As(err, &n) {
		return int(n)
	}
	return 0
}
func storageOptional(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL)
}
func storageRecord(ops *[]storageOperation, name string, err error, optional bool) {
	o := storageOperation{Name: name, OK: err == nil, Errno: storageErrno(err)}
	if err != nil {
		o.Error = err.Error()
		o.Unsupported = optional && storageOptional(err)
	}
	*ops = append(*ops, o)
}
func storagePattern(variant byte) []byte {
	b := make([]byte, 8192)
	for i := range b {
		b[i] = byte((i*31 + 17 + int(variant)) % 251)
	}
	return b
}

type storageOwnedFile struct {
	dev uint64
	ino uint64
}

func storageFileIdentity(fd int) (storageOwnedFile, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return storageOwnedFile{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return storageOwnedFile{}, errors.New("not an exclusive regular probe file")
	}
	return storageOwnedFile{uint64(st.Dev), st.Ino}, nil
}
func storageCheckName(fd int, name string, want storageOwnedFile) error {
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || uint64(st.Dev) != want.dev || st.Ino != want.ino {
		return errors.New("probe name identity changed; refusing mutation")
	}
	return nil
}

// All names are fixed inside a new exclusive directory. No recursive removal,
// symlink/hardlink creation, ownership changes or model-file operations occur.
func storageGoIO(fd int) ([]storageOperation, map[string]storageOwnedFile) {
	ops := []storageOperation{}
	owned := map[string]storageOwnedFile{}
	run := func(name string, fn func() error) bool {
		err := fn()
		storageRecord(&ops, name, err, false)
		return err == nil
	}
	var a int = -1
	if !run("create", func() error {
		var err error
		a, err = unix.Openat(fd, "a", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return err
		}
		i, e := storageFileIdentity(a)
		if e == nil {
			owned["a"] = i
		}
		return e
	}) {
		if a >= 0 {
			unix.Close(a)
		}
		return ops, owned
	}
	defer func() {
		if a >= 0 {
			unix.Close(a)
		}
	}()
	write := func(variant byte) error {
		b := storagePattern(variant)
		for len(b) > 0 {
			n, e := unix.Write(a, b)
			if e == syscall.EINTR {
				continue
			}
			if e != nil {
				return e
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
		return nil
	}
	read := func(variant byte) error {
		if _, e := unix.Seek(a, 0, 0); e != nil {
			return e
		}
		var st unix.Stat_t
		if e := unix.Fstat(a, &st); e != nil {
			return e
		}
		if st.Size != 8192 {
			return errors.New("probe file length mismatch")
		}
		b := make([]byte, 8192)
		done := 0
		for done < len(b) {
			n, e := unix.Read(a, b[done:])
			if e == syscall.EINTR {
				continue
			}
			if e != nil {
				return e
			}
			if n == 0 {
				return io.ErrUnexpectedEOF
			}
			done += n
		}
		if !bytes.Equal(b, storagePattern(variant)) {
			return errors.New("probe byte mismatch")
		}
		return nil
	}
	readRenamed := func(variant byte) error {
		if e := storageCheckName(fd, "b", owned["b"]); e != nil {
			return e
		}
		renamed, e := unix.Openat(fd, "b", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		defer unix.Close(renamed)
		id, e := storageFileIdentity(renamed)
		if e != nil {
			return e
		}
		if id != owned["b"] {
			return errors.New("renamed file identity mismatch")
		}
		old := a
		a = renamed
		defer func() { a = old }()
		return read(variant)
	}
	if !run("write", func() error { return write(0) }) || !run("fsync", func() error { return unix.Fsync(a) }) || !run("read", func() error { return read(0) }) {
		return ops, owned
	}
	if !run("rename_new", func() error {
		if e := storageCheckName(fd, "a", owned["a"]); e != nil {
			return e
		}
		var st unix.Stat_t
		if e := unix.Fstatat(fd, "b", &st, unix.AT_SYMLINK_NOFOLLOW); e != syscall.ENOENT {
			if e != nil {
				return e
			}
			return errors.New("rename destination unexpectedly exists")
		}
		if e := unix.Renameat(fd, "a", fd, "b"); e != nil {
			return e
		}
		owned["b"] = owned["a"]
		delete(owned, "a")
		return readRenamed(0)
	}) {
		return ops, owned
	}
	unix.Close(a)
	a = -1
	if !run("replacement_create", func() error {
		var e error
		a, e = unix.Openat(fd, "a", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return e
		}
		i, e := storageFileIdentity(a)
		if e == nil {
			owned["a"] = i
		}
		return e
	}) || !run("replacement_write", func() error { return write(1) }) || !run("replacement_fsync", func() error { return unix.Fsync(a) }) {
		return ops, owned
	}
	if !run("rename_replace", func() error {
		if e := storageCheckName(fd, "a", owned["a"]); e != nil {
			return e
		}
		if e := storageCheckName(fd, "b", owned["b"]); e != nil {
			return e
		}
		if e := unix.Renameat(fd, "a", fd, "b"); e != nil {
			return e
		}
		owned["b"] = owned["a"]
		delete(owned, "a")
		return nil
	}) || !run("replacement_read", func() error { return readRenamed(1) }) {
		return ops, owned
	}
	run("unlink", func() error {
		if e := storageCheckName(fd, "b", owned["b"]); e != nil {
			return e
		}
		if e := unix.Unlinkat(fd, "b", 0); e != nil {
			return e
		}
		delete(owned, "b")
		return nil
	})
	storageRecord(&ops, "dirsync", unix.Fsync(fd), true)
	var fs unix.Statfs_t
	storageRecord(&ops, "statfs", unix.Fstatfs(fd, &fs), true)
	return ops, owned
}

var storageRequired = []string{"mkdir", "create", "write", "fsync", "read", "rename_new", "replacement_create", "replacement_write", "replacement_fsync", "rename_replace", "replacement_read", "unlink", "dirsync", "statfs", "cleanup"}

func storageEvidence(ops []storageOperation) error {
	seen := map[string]bool{}
	for _, o := range ops {
		known := o.Name == "domain" || o.Name == "inherited_directory" || o.Name == "reopened_directory"
		for _, name := range storageRequired {
			if o.Name == name {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unexpected operation %s", o.Name)
		}
		if seen[o.Name] {
			return fmt.Errorf("duplicate operation %s", o.Name)
		}
		seen[o.Name] = true
		if o.OK && (o.Errno != 0 || o.Unsupported || o.Error != "") {
			return fmt.Errorf("contradictory operation %s", o.Name)
		}
		if !o.OK && !(o.Unsupported && (o.Name == "dirsync" || o.Name == "statfs") && storageOptional(syscall.Errno(o.Errno))) {
			return fmt.Errorf("%s failed: errno=%d %s", o.Name, o.Errno, o.Error)
		}
	}
	for _, name := range storageRequired {
		if !seen[name] {
			return fmt.Errorf("missing operation %s", name)
		}
	}
	return nil
}

type storageBoundedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	overflow bool
}

func (b *storageBoundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := 16384 - b.Len()
	if len(p) > room {
		b.overflow = true
		p = p[:room]
	}
	b.Buffer.Write(p)
	return n, nil
}

type storageProbeRunner struct{ helper, launcher string }

func validateStorageNative(result *storageNativeResult, pid int, parent unix.Stat_t, domain string) error {
	if result == nil {
		return errors.New("native evidence missing")
	}
	var err error
	if result.Schema != 1 || pid <= 1 || result.PID != pid || result.PPID != os.Getpid() || result.UID != os.Getuid() || result.Domain == "" || result.Domain != domain || result.ParentDev != uint64(parent.Dev) || result.ParentIno != parent.Ino {
		err = errors.New("native identity/domain/inherited-directory mismatch")
	}
	err = errors.Join(err, storageEvidence(result.Operations))
	for _, name := range []string{"domain", "inherited_directory"} {
		found := false
		for _, op := range result.Operations {
			if op.Name == name {
				found = true
			}
		}
		if !found {
			err = errors.Join(err, fmt.Errorf("missing native control %s", name))
		}
	}
	return err
}

func installedStorageProbeRunner() (storageProbeRunner, error) {
	exe, err := os.Executable()
	if err != nil {
		return storageProbeRunner{}, err
	}
	dir := filepath.Dir(exe)
	return storageProbeRunner{filepath.Join(dir, "pico-storage-probe"), filepath.Join(dir, "pico-service-launcher")}, nil
}
func (runner storageProbeRunner) native(ctx context.Context, fd int, parent unix.Stat_t, domain string) (*storageNativeResult, string, error) {
	if result, stderr, handled, err := harmonyManagedStorageProbe(ctx, fd, parent, domain); handled {
		return result, stderr, err
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	f := os.NewFile(uintptr(dup), "storage-probe-directory")
	defer f.Close()
	cmd := exec.CommandContext(ctx, runner.launcher, "1536", runner.helper)
	cmd.ExtraFiles = []*os.File{f}
	cmd.WaitDelay = 2 * time.Second
	var out, stderr storageBoundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if out.overflow || stderr.overflow {
		err = errors.Join(err, errors.New("child output exceeded bound"))
	}
	var result storageNativeResult
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	parseErr := dec.Decode(&result)
	var extra any
	if parseErr == nil && dec.Decode(&extra) != io.EOF {
		parseErr = errors.New("trailing native evidence")
	}
	err = errors.Join(err, parseErr)
	if parseErr == nil {
		pid := -1
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		err = errors.Join(err, validateStorageNative(&result, pid, parent, domain))
	}
	return &result, stderr.String(), err
}

func (runner storageProbeRunner) run(ctx context.Context, selection storageSelection) (result storageProbeResult) {
	result = storageProbeResult{Schema: 1, SelectionID: selection.ID, Time: time.Now().UTC().Format(time.RFC3339Nano), PID: os.Getpid(), UID: os.Getuid()}
	fail := func(err error) {
		if err != nil {
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += err.Error()
		}
	}
	if ctx.Err() != nil {
		fail(ctx.Err())
		return
	}
	domain, err := storageDomain()
	if err != nil {
		storageRecord(&result.Operations, "domain", err, false)
		fail(err)
		return
	}
	result.Domain = domain
	// Use the native-authorized path verbatim. Never accept a request path.
	root, err := unix.Open(selection.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		storageRecord(&result.Operations, "open_selection", err, false)
		fail(err)
		return
	}
	defer unix.Close(root)
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		fail(err)
		return
	}
	name := ".pico-storage-probe-" + hex.EncodeToString(nonce[:])
	result.Directory = name
	err = unix.Mkdirat(root, name, 0700)
	storageRecord(&result.Operations, "mkdir", err, false)
	if err != nil {
		fail(err)
		return
	}
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		fail(err)
		storageRecord(&result.Operations, "cleanup", errors.New("created directory not opened; retained for scoped recovery"), false)
		return
	}
	defer unix.Close(fd)
	var parent unix.Stat_t
	if err = unix.Fstat(fd, &parent); err != nil {
		storageRecord(&result.Operations, "directory_identity", err, false)
		storageRecord(&result.Operations, "cleanup", errors.New("directory identity unavailable; retained for scoped recovery"), false)
		fail(err)
		return
	}
	ops, owned := storageGoIO(fd)
	result.Operations = append(result.Operations, ops...)
	if ctx.Err() == nil {
		result.Native, result.ChildStderr, err = runner.native(ctx, fd, parent, domain)
		fail(err)
	} else {
		fail(ctx.Err())
	}
	var cleanup error
	for _, file := range []string{"a", "b"} {
		if id, ok := owned[file]; ok {
			e := storageCheckName(fd, file, id)
			if e == nil {
				e = unix.Unlinkat(fd, file, 0)
			}
			cleanup = errors.Join(cleanup, e)
		}
	}
	// Verify the exclusive directory itself before removing that one name.
	var current unix.Stat_t
	if e := unix.Fstatat(root, name, &current, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		cleanup = errors.Join(cleanup, e)
	} else if current.Dev != parent.Dev || current.Ino != parent.Ino || current.Mode&unix.S_IFMT != unix.S_IFDIR {
		cleanup = errors.Join(cleanup, errors.New("probe directory identity changed; retained"))
	} else {
		cleanup = errors.Join(cleanup, unix.Unlinkat(root, name, unix.AT_REMOVEDIR))
	}
	storageRecord(&result.Operations, "cleanup", cleanup, false)
	fail(cleanup)
	fail(storageEvidence(result.Operations))
	if result.Native == nil {
		fail(errors.New("native evidence missing"))
	}
	result.Passed = result.Error == ""
	return
}

func withStorageProbe(next http.Handler, token, directory string, root context.Context) http.Handler {
	runner, runnerErr := installedStorageProbeRunner()
	return storageProbeHandler(next, token, directory, root, runner, runnerErr)
}

// Internal constructor seam permits host fixtures; no HTTP field selects helpers.
func storageProbeHandler(next http.Handler, token, directory string, root context.Context, runner storageProbeRunner, runnerErr error) http.Handler {
	busy := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/harmony/storage/selection" && r.URL.Path != "/api/harmony/storage/probe" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, e := r.Cookie("token")
		if e != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
			http.Error(w, "APP token required", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/api/harmony/storage/selection" {
			s, e := readStorageSelection(directory)
			if e != nil {
				http.Error(w, e.Error(), 409)
				return
			}
			json.NewEncoder(w).Encode(s)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/harmony/storage/probe" {
			http.NotFound(w, r)
			return
		}
		if root.Err() != nil || r.Context().Err() != nil {
			http.Error(w, "APP/request cancelled", 503)
			return
		}
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			http.Error(w, "storage probe already active", 409)
			return
		}
		var request struct {
			SelectionID string `json:"selection_id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		var extra any
		if e = dec.Decode(&request); e != nil || dec.Decode(&extra) != io.EOF {
			http.Error(w, "invalid probe request", 400)
			return
		}
		s, e := readStorageSelection(directory)
		if e == nil {
			e = validStorageSelection(s, request.SelectionID)
		}
		if e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		if runnerErr != nil {
			http.Error(w, runnerErr.Error(), 500)
			return
		}
		ctx, cancel := context.WithTimeout(root, 15*time.Second)
		defer cancel()
		stop := context.AfterFunc(r.Context(), cancel)
		defer stop()
		result := runner.run(ctx, s)
		// Selection/activation can change while the native UI remains usable.
		// Results for an old grant must not qualify the currently selected folder.
		latest, selectionErr := readStorageSelection(directory)
		if selectionErr == nil {
			selectionErr = validStorageSelection(latest, s.ID)
		}
		if selectionErr == nil && (latest.Path != s.Path || latest.URI != s.URI || latest.Time != s.Time) {
			selectionErr = errors.New("selection changed during probe")
		}
		if selectionErr != nil {
			result.Passed = false
			result.Error = errors.Join(errors.New(result.Error), selectionErr).Error()
		}
		if b, e := json.Marshal(result); e != nil || len(b) > 65536 {
			http.Error(w, "probe result exceeds limit", 500)
			return
		}
		if e := atomicPrivateJSON(filepath.Join(directory, "storage-probe-result.json"), result); e != nil {
			http.Error(w, "persist probe result: "+e.Error(), 500)
			return
		}
		if !result.Passed {
			w.WriteHeader(http.StatusConflict)
		}
		json.NewEncoder(w).Encode(result)
	})
}
