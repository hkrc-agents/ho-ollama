//go:build harmonyos

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/types/model"
)

const harmonyDownloadWriteInterval = 250 * time.Millisecond

type harmonyDigestProgress struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
}

type harmonyDownload struct {
	Model     string                           `json:"model"`
	Status    string                           `json:"status"`
	Digests   map[string]harmonyDigestProgress `json:"digests"`
	Error     string                           `json:"error,omitempty"`
	CreatedAt time.Time                        `json:"created_at"`
	UpdatedAt time.Time                        `json:"updated_at"`
}

type harmonyDownloadFile struct {
	Version   int                `json:"version"`
	Downloads []*harmonyDownload `json:"downloads"`
}

// Per-store private seam: defaults are the existing real filesystem operations.
// Tests wrap real temporary files; no process-global hook or public API exists.
type harmonyJournalFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type harmonyJournalDirectory interface {
	Sync() error
	Close() error
}
type harmonyJournalFS struct {
	createTemp    func(string, string) (harmonyJournalFile, error)
	rename        func(string, string) error
	remove        func(string) error
	openDirectory func(string) (harmonyJournalDirectory, error)
}

func harmonyJournalOSFS() harmonyJournalFS {
	return harmonyJournalFS{
		createTemp: func(dir, pattern string) (harmonyJournalFile, error) { return os.CreateTemp(dir, pattern) },
		rename:     os.Rename, remove: os.Remove,
		openDirectory: func(path string) (harmonyJournalDirectory, error) { return os.Open(path) },
	}
}

// renamed means the replacement became visible, NOT that it is crash durable.
// begin/forget restore their RAM record on error, which cannot undo a rename.
type harmonyJournalSaveError struct {
	stage   string
	renamed bool
	err     error
}

func (e *harmonyJournalSaveError) Error() string {
	if e.renamed {
		return fmt.Sprintf("download journal %s (replacement visible, durability uncertain): %v", e.stage, e.err)
	}
	return fmt.Sprintf("download journal %s before rename: %v", e.stage, e.err)
}
func (e *harmonyJournalSaveError) Unwrap() error { return e.err }

// One store belongs to one APP server. The mutex covers records, active leases
// and the atomic file replacement, so an old request cannot finish over a retry.
type harmonyDownloads struct {
	mu        sync.Mutex
	path      string
	tasks     map[string]*harmonyDownload
	active    map[string]bool
	lastWrite time.Time
	initErr   error
	writeErr  error
	journalFS harmonyJournalFS
}

func harmonyModelKey(value string) (string, error) {
	n := model.ParseName(value)
	if !n.IsValid() {
		return "", errors.New("invalid model name")
	}
	return strings.ToLower(n.String()), nil
}

func openHarmonyDownloads(directory string) *harmonyDownloads {
	s := &harmonyDownloads{path: filepath.Join(directory, "downloads.json"), tasks: make(map[string]*harmonyDownload), active: make(map[string]bool), journalFS: harmonyJournalOSFS()}
	if directory == "" {
		s.initErr = errors.New("private directory required")
		return s
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		s.initErr = err
		return s
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	var file harmonyDownloadFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err != nil || file.Version != 1 {
		s.initErr = errors.New("cannot read download records")
		return s
	}
	changed := false
	for _, task := range file.Downloads {
		if task == nil {
			s.initErr = errors.New("invalid download record")
			return s
		}
		key, err := harmonyModelKey(task.Model)
		if err != nil || s.tasks[key] != nil || task.Digests == nil {
			s.initErr = errors.New("invalid download record")
			return s
		}
		switch task.Status {
		case "downloading":
			task.Status, task.UpdatedAt = "paused", time.Now().UTC()
			changed = true
		case "paused", "failed", "completed":
		default:
			s.initErr = errors.New("invalid download status")
			return s
		}
		s.tasks[key] = task
	}
	if changed {
		s.writeErr = s.saveLocked()
	}
	return s
}

func (s *harmonyDownloads) recordsLocked() []*harmonyDownload {
	tasks := make([]*harmonyDownload, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Model < tasks[j].Model })
	return tasks
}

// CreateTemp uses 0600; sync before rename and sync the directory afterwards.
// A failed write is surfaced, even if rename succeeded but directory sync failed.
func (s *harmonyDownloads) saveLocked() (err error) {
	stage, renamed := "marshal", false
	defer func() {
		if err != nil {
			err = &harmonyJournalSaveError{stage: stage, renamed: renamed, err: err}
		}
		s.writeErr = err
	}()
	data, err := json.Marshal(harmonyDownloadFile{Version: 1, Downloads: s.recordsLocked()})
	if err != nil {
		return err
	}
	stage = "create"
	f, err := s.journalFS.createTemp(filepath.Dir(s.path), ".downloads-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if cleanup := s.journalFS.remove(f.Name()); cleanup != nil && !errors.Is(cleanup, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("temporary journal cleanup: %w", cleanup))
		}
	}()
	stage = "chmod"
	if err = f.Chmod(0600); err == nil {
		stage = "write"
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		stage = "file sync"
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		stage = "file close"
		err = closeErr
	} else if closeErr != nil {
		err = errors.Join(err, fmt.Errorf("file close: %w", closeErr))
	}
	if err != nil {
		return err
	}
	stage = "rename"
	if err = s.journalFS.rename(f.Name(), s.path); err != nil {
		return err
	}
	renamed = true
	stage = "directory open"
	dir, err := s.journalFS.openDirectory(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	stage = "directory sync"
	err = dir.Sync()
	closeErr = dir.Close()
	if err == nil {
		stage = "directory close"
		err = closeErr
	} else if closeErr != nil {
		err = errors.Join(err, fmt.Errorf("directory close: %w", closeErr))
	}
	if err == nil {
		s.lastWrite = time.Now()
	}
	return err
}

func harmonyDownloadError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *harmonyDownloads) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil || s.writeErr != nil {
		harmonyDownloadError(w, http.StatusServiceUnavailable, "download persistence unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Downloads []*harmonyDownload `json:"downloads"`
	}{s.recordsLocked()})
}

func (s *harmonyDownloads) forget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		harmonyDownloadError(w, http.StatusBadRequest, "invalid model request")
		return
	}
	key, err := harmonyModelKey(body.Model)
	if err != nil {
		harmonyDownloadError(w, http.StatusBadRequest, "invalid model name")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		harmonyDownloadError(w, http.StatusServiceUnavailable, "download persistence unavailable")
		return
	}
	if s.active[key] {
		harmonyDownloadError(w, http.StatusConflict, "download is active")
		return
	}
	previous := s.tasks[key]
	if previous == nil {
		harmonyDownloadError(w, http.StatusNotFound, "download not found")
		return
	}
	delete(s.tasks, key)
	if err := s.saveLocked(); err != nil {
		s.tasks[key] = previous
		harmonyDownloadError(w, http.StatusServiceUnavailable, "cannot persist download removal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *harmonyDownloads) begin(name string) (string, int, error) {
	key, err := harmonyModelKey(name)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return "", http.StatusServiceUnavailable, errors.New("download persistence unavailable")
	}
	if s.active[key] {
		return "", http.StatusConflict, errors.New("download is active")
	}
	previous := s.tasks[key]
	now := time.Now().UTC()
	task := &harmonyDownload{Model: name, Status: "downloading", Digests: make(map[string]harmonyDigestProgress), CreatedAt: now, UpdatedAt: now}
	if previous != nil {
		task.CreatedAt = previous.CreatedAt
		// A mutable registry tag may resolve to different layers on retry.
		// Retain the paused snapshot until retry starts, then accept only this
		// attempt's core-reported digests. Core owns reusable partial blobs;
		// resetting this UI summary never discards downloaded weight bytes.
	}
	s.tasks[key] = task
	if err := s.saveLocked(); err != nil {
		if previous == nil {
			delete(s.tasks, key)
		} else {
			s.tasks[key] = previous
		}
		return "", http.StatusServiceUnavailable, fmt.Errorf("cannot persist download start: %w", err)
	}
	s.active[key] = true
	return key, 0, nil
}

func (s *harmonyDownloads) finish(key, status, message string) error {
	return s.finishWithPersistenceError(key, status, message, nil)
}

// A finishing retry may durably save the failed task. Preserve the original
// error for the stream response, but let list expose that failed row for retry
// once saving succeeded; do not latch an already recovered store unavailable.
func (s *harmonyDownloads) finishWithPersistenceError(key, status, message string, prior error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	task.Status, task.Error, task.UpdatedAt = status, message, time.Now().UTC()
	delete(s.active, key)
	if err := s.saveLocked(); err != nil {
		task.Status, task.Error = "failed", "cannot persist download state"
		s.writeErr = errors.Join(prior, err)
		return s.writeErr
	}
	return prior
}

type harmonyPullEvent struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     *int64 `json:"total"`
	Completed *int64 `json:"completed"`
	Error     string `json:"error"`
}

func (s *harmonyDownloads) progress(key string, event harmonyPullEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[key]
	if event.Digest != "" {
		progress := task.Digests[event.Digest]
		if event.Total != nil {
			progress.Total = *event.Total
		}
		if event.Completed != nil {
			progress.Completed = *event.Completed
		}
		task.Digests[event.Digest] = progress
	}
	task.UpdatedAt = time.Now().UTC()
	if time.Since(s.lastWrite) >= harmonyDownloadWriteInterval {
		return s.saveLocked()
	}
	return nil
}

func (s *harmonyDownloads) pull(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var request struct {
		Model string `json:"model"`
		Name  string `json:"name"` // core's legacy alias
	}
	if err != nil || json.Unmarshal(body, &request) != nil {
		harmonyDownloadError(w, http.StatusBadRequest, "invalid pull request")
		return
	}
	name := request.Model
	if name == "" {
		name = request.Name
	}
	key, status, err := s.begin(name)
	if err != nil {
		harmonyDownloadError(w, status, err.Error())
		return
	}
	finished := false
	finish := func(state, message string) error {
		finished = true
		return s.finish(key, state, message)
	}
	defer func() {
		if !finished {
			state, message := "failed", "core stream ended without success"
			if r.Context().Err() != nil {
				state, message = "paused", ""
			}
			_ = finish(state, message)
		}
	}()
	target := *envconfig.ConnectableHost()
	target.Path, target.RawQuery = "/api/pull", r.URL.RawQuery
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		_ = finish("failed", "cannot create core request")
		harmonyDownloadError(w, http.StatusBadGateway, "cannot create core request")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	// Never forward APP cookies or other APP credentials. Do not follow redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(upstream)
	if err != nil {
		state, message := "failed", "core request failed"
		if r.Context().Err() != nil {
			state, message = "paused", ""
		}
		if finish(state, message) != nil {
			harmonyDownloadError(w, http.StatusServiceUnavailable, "cannot persist download state")
		} else {
			harmonyDownloadError(w, http.StatusBadGateway, "core request failed")
		}
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	// Delay response headers until a line is available, allowing storage failures
	// before the stream starts to carry an HTTP error status.
	started := false
	writeLine := func(line []byte) error {
		if !started {
			w.WriteHeader(response.StatusCode)
			started = true
		}
		_, err := w.Write(line)
		if err == nil {
			err = http.NewResponseController(w).Flush()
			if errors.Is(err, http.ErrNotSupported) {
				err = nil
			}
		}
		return err
	}
	streamError := func(message string, persistenceErr error) {
		state := "failed"
		status := http.StatusBadGateway
		if persistenceErr != nil {
			status = http.StatusServiceUnavailable
		} else if r.Context().Err() != nil {
			state, message = "paused", ""
		}
		finished = true
		if finishErr := s.finishWithPersistenceError(key, state, message, persistenceErr); finishErr != nil {
			if persistenceErr == nil {
				message = "cannot persist download state"
			}
			status = http.StatusServiceUnavailable
		}
		if !started {
			harmonyDownloadError(w, status, message)
		} else if message != "" {
			data, _ := json.Marshal(map[string]string{"error": message})
			_ = writeLine(append(data, '\n'))
		}
	}
	reader := bufio.NewReaderSize(response.Body, 1<<20)
	for {
		line, readErr := reader.ReadSlice('\n')
		if r.Context().Err() != nil {
			streamError("client canceled download", nil)
			return
		}
		if readErr != nil && readErr != io.EOF {
			streamError("core stream interrupted", nil)
			return
		}
		if len(bytes.TrimSpace(line)) != 0 {
			var event harmonyPullEvent
			if json.Unmarshal(line, &event) != nil {
				streamError("invalid core download response", nil)
				return
			}
			if err := s.progress(key, event); err != nil {
				streamError("cannot persist download progress", err)
				return
			}
			terminal, state, message := false, "", ""
			switch {
			case event.Error != "":
				terminal, state, message = true, "failed", event.Error
			case response.StatusCode < 200 || response.StatusCode >= 300:
				terminal, state, message = true, "failed", fmt.Sprintf("core HTTP %d", response.StatusCode)
			case event.Status == "success":
				terminal, state = true, "completed"
			}
			if terminal {
				if err := finish(state, message); err != nil {
					if !started {
						harmonyDownloadError(w, http.StatusServiceUnavailable, "cannot persist download state")
					} else {
						_ = writeLine([]byte("{\"error\":\"cannot persist download state\"}\n"))
					}
					return
				}
			}
			if err := writeLine(line); err != nil {
				if !finished {
					_ = finish("paused", "")
				}
				return
			}
			if terminal {
				return
			}
			// A final JSON object without a newline must not run into the
			// synthetic error record emitted for a missing terminal success.
			if readErr == io.EOF && line[len(line)-1] != '\n' {
				if err := writeLine([]byte{'\n'}); err != nil {
					_ = finish("paused", "")
					return
				}
			}
		}
		if readErr == io.EOF {
			streamError("core stream ended without success", nil)
			return
		}
	}
}
