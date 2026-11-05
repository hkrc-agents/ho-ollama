//go:build harmonyos

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type harmonyImportIssue struct {
	File  string `json:"file"`
	Error string `json:"error"`
}
type harmonyImportMarker struct {
	Digest     string `json:"digest"`
	Manifest   string `json:"manifest"`
	Reconciled bool   `json:"reconciled"`
}
type harmonyImportInspector func(context.Context, string) (harmonyModelFile, error)

func (s *harmonyGGUF) reconcilePending(ctx context.Context) error {
	if s.inspect == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.diskLease()
	if err != nil {
		return err
	}
	defer release()
	records, _, err := s.records()
	if err != nil {
		return err
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if record.Status != "unconfirmed" {
			continue
		}
		file, e := s.inspect(ctx, record.Model)
		if e != nil || file.Digest != record.Digest || file.Size != record.Size || file.Manifest == "" {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(record.Model, "harmony-import-"), ":latest")
		marker, _ := json.Marshal(harmonyImportMarker{Digest: record.Digest, Manifest: file.Manifest, Reconciled: true})
		if e = s.writeReceipt(filepath.Join(s.directory, key+".complete"), marker); e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
	}
	return nil
}

func (s *harmonyGGUF) archive(w http.ResponseWriter, r *http.Request) {
	if !s.mu.TryLock() {
		http.Error(w, "import active", 409)
		return
	}
	defer s.mu.Unlock()
	release, err := s.diskLease()
	if err != nil {
		http.Error(w, "import lease unavailable", 409)
		return
	}
	defer release()
	var request struct {
		File    string `json:"file"`
		Confirm bool   `json:"confirm"`
	}
	if decodeStorageRequest(w, r, &request) != nil || !request.Confirm || !regexp.MustCompile(`^[0-9a-f]{32}\.(json|complete)$`).MatchString(request.File) {
		http.Error(w, "explicit archive confirmation and a generated record filename required", 400)
		return
	}
	_, issues, err := s.records()
	if err != nil {
		http.Error(w, "records unavailable", 503)
		return
	}
	known := false
	for _, issue := range issues {
		if issue.File == request.File {
			known = true
		}
	}
	if !known {
		http.Error(w, "only a currently damaged record can be archived", 409)
		return
	}
	path := filepath.Join(s.directory, request.File)
	info, err := os.Lstat(path)
	if err != nil {
		http.Error(w, "record unavailable", 409)
		return
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || identity.Nlink != 1 || identity.Uid != uint32(os.Getuid()) {
		http.Error(w, "record ownership unknown; retained", 409)
		return
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		http.Error(w, "archive identity unavailable", 503)
		return
	}
	name := request.File + ".corrupt-" + hex.EncodeToString(id)
	if err = os.Rename(path, filepath.Join(s.directory, name)); err != nil {
		http.Error(w, "archive failed; record retained", 503)
		return
	}
	d, err := os.Open(s.directory)
	if err == nil {
		err = errors.Join(d.Sync(), d.Close())
	}
	if err != nil {
		http.Error(w, "archive visible but durability unconfirmed", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"archived": name, "note": "Model data unchanged; damaged record retained for review"})
}

func (s *harmonyGGUF) diskLease() (func(), error) {
	if err := os.MkdirAll(s.directory, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.directory, ".lease"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func boundedImportRead(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("record is not a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, 4097))
}

func (s *harmonyGGUF) records() ([]harmonyImportReceipt, []harmonyImportIssue, error) {
	entries, err := os.ReadDir(s.directory)
	if errors.Is(err, os.ErrNotExist) {
		entries, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	receipts := []harmonyImportReceipt{}
	issues := []harmonyImportIssue{}
	for _, entry := range entries {
		key, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		id, e := hex.DecodeString(key)
		var record harmonyImportReceipt
		data, readErr := boundedImportRead(filepath.Join(s.directory, entry.Name()))
		if e != nil || len(id) != 16 || readErr != nil || json.Unmarshal(data, &record) != nil || record.Model != "harmony-import-"+key+":latest" || record.Size < 24 || record.Size > harmonyGGUFMax || !strings.HasPrefix(record.Digest, "sha256:") {
			issues = append(issues, harmonyImportIssue{entry.Name(), "invalid import intent retained; no model data removed"})
			continue
		}
		if _, e = hexDigest(strings.TrimPrefix(record.Digest, "sha256:")); e != nil {
			issues = append(issues, harmonyImportIssue{entry.Name(), "invalid digest retained"})
			continue
		}
		record.Status = "unconfirmed"
		marker, e := boundedImportRead(filepath.Join(s.directory, key+".complete"))
		if e == nil {
			var outcome harmonyImportMarker
			if string(marker) == record.Digest+"\n" {
				record.Status = "completed"
			} else if json.Unmarshal(marker, &outcome) == nil && outcome.Digest == record.Digest && outcome.Manifest != "" {
				record.Status = "completed"
				if outcome.Reconciled {
					record.Status = "recovered"
				}
			} else {
				issues = append(issues, harmonyImportIssue{key + ".complete", "invalid completion marker retained"})
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			issues = append(issues, harmonyImportIssue{key + ".complete", "completion marker unreadable; outcome unconfirmed"})
		}
		receipts = append(receipts, record)
	}
	return receipts, issues, nil
}

// Every upload holds an on-disk lease: restarting must not remove another
// still-active server's staging. Cleanup never scans a user's source directory.
func (s *harmonyGGUF) recoverStaging() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.diskLease()
	if err != nil {
		return err
	}
	defer release()
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	pattern := regexp.MustCompile(`^upload-[0-9]+\.part$`)
	for _, entry := range entries {
		if !pattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(s.directory, entry.Name())
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
			return errors.New("staging ownership unknown; retained")
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	}
	d, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (s *harmonyGGUF) reconcile(w http.ResponseWriter, r *http.Request) {
	if !s.mu.TryLock() {
		http.Error(w, "import active", 409)
		return
	}
	defer s.mu.Unlock()
	release, err := s.diskLease()
	if err != nil {
		http.Error(w, "import lease unavailable", 409)
		return
	}
	defer release()
	var request struct {
		Model string `json:"model"`
	}
	if decodeStorageRequest(w, r, &request) != nil {
		http.Error(w, "invalid reconciliation request", 400)
		return
	}
	records, _, err := s.records()
	if err != nil {
		http.Error(w, "cannot read import records", 503)
		return
	}
	var record *harmonyImportReceipt
	for _, item := range records {
		if item.Model == request.Model {
			copy := item
			record = &copy
			break
		}
	}
	if record == nil {
		http.Error(w, "no valid import intent for this name", 404)
		return
	}
	if record.Status != "unconfirmed" {
		json.NewEncoder(w).Encode(record)
		return
	}
	if s.inspect == nil {
		http.Error(w, "model inspection unavailable", 503)
		return
	}
	file, err := s.inspect(r.Context(), record.Model)
	if err != nil {
		http.Error(w, "Model is absent or cannot currently be inspected; not proof of rollback: "+err.Error(), 409)
		return
	}
	if file.Digest != record.Digest || file.Size != record.Size {
		http.Error(w, "name refers to different weights; no record or model overwritten", 409)
		return
	}
	key := strings.TrimSuffix(strings.TrimPrefix(record.Model, "harmony-import-"), ":latest")
	marker, err := json.Marshal(harmonyImportMarker{Digest: record.Digest, Manifest: file.Manifest, Reconciled: true})
	if err == nil {
		err = s.writeReceipt(filepath.Join(s.directory, key+".complete"), marker)
	}
	if err != nil {
		http.Error(w, "Cannot record recovery; existing marker/uncertainty retained: "+err.Error(), 503)
		return
	}
	record.Status = "recovered"
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(record)
}
