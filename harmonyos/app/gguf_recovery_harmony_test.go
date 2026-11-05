//go:build harmonyos

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importReceiptFixture(t *testing.T, s *harmonyGGUF) harmonyImportReceipt {
	t.Helper()
	os.MkdirAll(s.directory, 0700)
	key := strings.Repeat("c", 32)
	record := harmonyImportReceipt{Model: "harmony-import-" + key + ":latest", Digest: "sha256:" + strings.Repeat("d", 64), Size: 32, Status: "unconfirmed"}
	data, _ := json.Marshal(record)
	if err := harmonyImportWrite(filepath.Join(s.directory, key+".json"), data); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestImportRecoveryRequiresMatchingCurrentWeights(t *testing.T) {
	for _, kind := range []string{"matching", "different", "absent"} {
		t.Run(kind, func(t *testing.T) {
			s := ggufStore(t)
			record := importReceiptFixture(t, s)
			s.inspect = func(context.Context, string) (harmonyModelFile, error) {
				if kind == "absent" {
					return harmonyModelFile{}, os.ErrNotExist
				}
				result := harmonyModelFile{Digest: record.Digest, Size: record.Size, Manifest: strings.Repeat("e", 64)}
				if kind == "different" {
					result.Digest = "sha256:" + strings.Repeat("f", 64)
				}
				return result, nil
			}
			body, _ := json.Marshal(map[string]string{"model": record.Model})
			r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
			w := httptest.NewRecorder()
			s.reconcile(w, r)
			want := 409
			if kind == "matching" {
				want = 200
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			records, _, err := s.records()
			if err != nil || len(records) != 1 {
				t.Fatal(err)
			}
			if (records[0].Status == "recovered") != (kind == "matching") {
				t.Fatal(records)
			}
		})
	}
}

func TestImportCorruptRecordDoesNotHideValidModels(t *testing.T) {
	s := ggufStore(t)
	importReceiptFixture(t, s)
	bad := filepath.Join(s.directory, strings.Repeat("a", 32)+".json")
	os.WriteFile(bad, []byte("{"), 0600)
	records, issues, err := s.records()
	if err != nil || len(records) != 1 || len(issues) != 1 {
		t.Fatal(records, issues, err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("corrupt evidence was deleted")
	}
}

func TestImportStaleStageCleanupHonorsLease(t *testing.T) {
	s := ggufStore(t)
	record := importReceiptFixture(t, s)
	stage := filepath.Join(s.directory, "upload-12345.part")
	os.WriteFile(stage, []byte("partial"), 0600)
	untouched := filepath.Join(s.directory, "user-file.part")
	os.WriteFile(untouched, []byte("preserve"), 0600)
	unlock, err := s.diskLease()
	if err != nil {
		t.Fatal(err)
	}
	other := newHarmonyGGUF(filepath.Dir(s.directory))
	if err := other.recoverStaging(); err == nil {
		t.Fatal("cleaned despite active cross-instance lease")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("active staging removed")
	}
	unlock()
	if err := other.recoverStaging(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staging remains")
	}
	if _, err := os.Stat(untouched); err != nil {
		t.Fatal("unknown file removed")
	}
	records, _, err := s.records()
	if err != nil || records[0].Model != record.Model {
		t.Fatal("receipt lost")
	}
}

func TestImportStartupRecoveryAndArchive(t *testing.T) {
	s := ggufStore(t)
	record := importReceiptFixture(t, s)
	s.inspect = func(context.Context, string) (harmonyModelFile, error) {
		return harmonyModelFile{Digest: record.Digest, Size: record.Size, Manifest: strings.Repeat("e", 64)}, nil
	}
	s.reconcilePending(t.Context())
	records, _, err := s.records()
	if err != nil || len(records) != 1 || records[0].Status != "recovered" {
		t.Fatal(records, err)
	}
	bad := strings.Repeat("a", 32) + ".json"
	if err := os.WriteFile(filepath.Join(s.directory, bad), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside.json", strings.Repeat("c", 32) + ".json", bad} {
		body, _ := json.Marshal(map[string]any{"file": name, "confirm": true})
		w := httptest.NewRecorder()
		s.archive(w, httptest.NewRequest("POST", "/", strings.NewReader(string(body))))
		if (w.Code == 200) != (name == bad) {
			t.Fatal(name, w.Code, w.Body.String())
		}
	}
	_, issues, err := s.records()
	if err != nil || len(issues) != 0 {
		t.Fatal(issues, err)
	}
	files, _ := filepath.Glob(filepath.Join(s.directory, bad+".corrupt-*"))
	if len(files) != 1 {
		t.Fatal("damaged evidence not archived", files)
	}
}
