//go:build harmonyos

package main

// Platform file ingress only. Ollama's official client/core still own blob
// installation, GGUF parsing, model creation and templates. No source path is
// accepted, and no shared core blob is deleted after cancellation/failure.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/envconfig"
)

const harmonyGGUFMax = int64(4 << 30)

type harmonyImportReceipt struct {
	Model  string `json:"model"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
}

type harmonyGGUF struct {
	directory    string
	mu           sync.Mutex // Bound staging and duplicate disk use to one ingress per APP.
	space        func(string, int64) error
	writeReceipt func(string, []byte) error
	inspect      harmonyImportInspector
}

func harmonyGGUFSpace(directory string, size int64) error {
	var info syscall.Statfs_t
	if err := syscall.Statfs(directory, &info); err != nil {
		return err
	}
	// Staging plus core's own streamed blob copy, with a fixed free-space reserve.
	if uint64(info.Bavail)*uint64(info.Bsize) < uint64(size)*2+(256<<20) {
		return errors.New("insufficient free space for staging and core upload")
	}
	return nil
}

func newHarmonyGGUF(directory string) *harmonyGGUF {
	return &harmonyGGUF{directory: filepath.Join(directory, "gguf-imports"), space: harmonyGGUFSpace, writeReceipt: harmonyImportWrite}
}

// An immutable intent is synced BEFORE Create. Success is a separate synced
// marker, never an in-place update. A missing marker means unconfirmed, not
// rollback: core may have published the model before an error/disconnect.
func harmonyImportWrite(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

type harmonyContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r harmonyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *harmonyGGUF) importFile(w http.ResponseWriter, r *http.Request) {
	if !s.mu.TryLock() {
		http.Error(w, "another GGUF upload is active", 409)
		return
	}
	defer s.mu.Unlock()
	size, err := strconv.ParseInt(r.Header.Get("X-GGUF-Size"), 10, 64)
	if err != nil || size < 24 || size > harmonyGGUFMax || (r.ContentLength >= 0 && r.ContentLength != size) {
		http.Error(w, "GGUF size must match the upload (24 bytes to 4 GiB)", 400)
		return
	}
	if err := os.MkdirAll(s.directory, 0700); err != nil {
		http.Error(w, "cannot prepare private staging", 503)
		return
	}
	if err := s.space(s.directory, size); err != nil {
		http.Error(w, err.Error(), 507)
		return
	}
	release, err := s.diskLease()
	if err != nil {
		http.Error(w, "import store is busy", 409)
		return
	}
	defer release()
	f, err := os.CreateTemp(s.directory, "upload-*.part")
	if err != nil {
		http.Error(w, "cannot create private staging", 503)
		return
	}
	closed, removed := false, false
	defer func() {
		if !closed {
			if err := f.Close(); err != nil {
				slog.Error("GGUF staging close", "error", err)
			}
		}
		if !removed {
			if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("GGUF staging cleanup", "path", f.Name(), "error", err)
			}
		}
	}()
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, hash), io.LimitReader(harmonyContextReader{r.Context(), r.Body}, size+1), make([]byte, 128<<10))
	if err != nil || n != size {
		http.Error(w, "incomplete or oversized GGUF upload", 400)
		return
	}
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err != nil || string(magic[:]) != "GGUF" {
		http.Error(w, "file is not a GGUF; core validates its contents during create", 400)
		return
	}
	if err := f.Sync(); err != nil {
		http.Error(w, "cannot sync private staging", 503)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "cannot read private staging", 503)
		return
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		http.Error(w, "cannot allocate import identity", 503)
		return
	}
	key := hex.EncodeToString(id)
	receipt := harmonyImportReceipt{Model: "harmony-import-" + key + ":latest", Digest: digest, Size: size, Status: "unconfirmed"}
	data, _ := json.Marshal(receipt)
	path := filepath.Join(s.directory, key+".json")
	if err := s.writeReceipt(path, data); err != nil {
		http.Error(w, "cannot persist import intent; no model create requested", 503)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	emit := func(value any) error {
		if err := encoder.Encode(value); err != nil {
			return err
		}
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		return nil
	}
	if err := emit(map[string]string{"status": "Uploading to Ollama", "model": receipt.Model}); err != nil {
		return
	}
	client := api.NewClient(envconfig.ConnectableHost(), http.DefaultClient)
	// net/http takes ownership of an io.ReadCloser body. Keep our file lifetime
	// explicit by passing a non-closing bounded SectionReader, not *os.File.
	if err := client.CreateBlob(r.Context(), digest, io.NewSectionReader(f, 0, size)); err != nil {
		_ = emit(map[string]string{"error": fmt.Sprintf("blob upload failed: %v; inspect import record", err), "model": receipt.Model})
		return
	}
	// Release our own temporary copy before model creation; never unlink a core blob.
	err = f.Close()
	closed = true
	if err == nil {
		err = os.Remove(f.Name())
		removed = err == nil
	}
	if err != nil {
		_ = emit(map[string]string{"error": "private staging cleanup failed; model create not requested", "model": receipt.Model})
		return
	}
	confirmed := false
	err = client.Create(r.Context(), &api.CreateRequest{Model: receipt.Model, Files: map[string]string{"model.gguf": digest}}, func(event api.ProgressResponse) error {
		if event.Status == "success" {
			confirmed = true
			return nil
		}
		return emit(map[string]string{"status": event.Status, "model": receipt.Model})
	})
	if err == nil && !confirmed {
		err = errors.New("create ended before explicit success")
	}
	if err == nil {
		err = s.writeReceipt(filepath.Join(s.directory, key+".complete"), []byte(digest+"\n"))
	}
	if err != nil {
		_ = emit(map[string]string{"error": fmt.Sprintf("import result unconfirmed: %v; inspect %s before retrying", err, receipt.Model), "model": receipt.Model})
		return
	}
	_ = emit(map[string]string{"status": "success", "model": receipt.Model})
}

func (s *harmonyGGUF) list(w http.ResponseWriter, r *http.Request) {
	if !s.mu.TryLock() {
		http.Error(w, "GGUF import is active; retry listing after completion", 409)
		return
	}
	defer s.mu.Unlock()
	receipts, issues, err := s.records()
	if err != nil {
		http.Error(w, "cannot read import records", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"imports": receipts, "issues": issues})
}
