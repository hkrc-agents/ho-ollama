//go:build harmonyos

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func migrationFixture(t *testing.T) (*migrationRecord, migrationHooks, *string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "original")
	parent := filepath.Join(base, "destination")
	for _, path := range []string{filepath.Join(source, "blobs"), filepath.Join(source, "manifests", "example"), parent} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "blobs", "weights"), []byte(strings.Repeat("weights-content\n", 10000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifests", "example", "latest"), []byte("manifest-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	selected := source
	record := &migrationRecord{Version: 1, ID: id, OldRoot: source, Parent: parent, NewRoot: filepath.Join(parent, "ollama-models-"+id), Files: []migrationFile{}, ServerConfig: serverConfig{Model: filepath.Join(source, "blobs", "weights")}}
	hooks := migrationHooks{Save: func(r *migrationRecord) error { return storageAtomicJSON(filepath.Join(base, "transaction.json"), r) }, Stop: func() error { return nil }, SetRoot: func(value string) error { selected = value; return nil }, GetRoot: func() (string, error) { return selected, nil }, ConfigureServer: func(serverConfig) error { return nil }, Start: func(context.Context, *migrationRecord, bool) error { return nil }, CheckSelection: func() error { return nil }, Space: func(string, int64) error { return nil }}
	return record, hooks, &selected
}

func TestStorageMigrationCopyVerifyCommit(t *testing.T) {
	r, h, selected := migrationFixture(t)
	var events []string
	stop, set, start, configure := h.Stop, h.SetRoot, h.Start, h.ConfigureServer
	h.Stop = func() error { events = append(events, "stop"); return stop() }
	h.SetRoot = func(path string) error { events = append(events, "switch"); return set(path) }
	h.ConfigureServer = func(c serverConfig) error {
		if c.Model != filepath.Join(r.NewRoot, "blobs", "weights") {
			t.Fatal("standalone path was not remapped")
		}
		return configure(c)
	}
	h.Start = func(ctx context.Context, r *migrationRecord, forward bool) error {
		events = append(events, "start")
		return start(ctx, r, forward)
	}
	if err := runStorageMigration(t.Context(), r, h); err != nil {
		t.Fatal(err)
	}
	if r.State != "completed" || *selected != r.NewRoot || !reflect.DeepEqual(events, []string{"stop", "switch", "start"}) {
		t.Fatal(r.State, events)
	}
	old, _ := os.OpenRoot(r.OldRoot)
	defer old.Close()
	next, _ := os.OpenRoot(r.NewRoot)
	defer next.Close()
	a, e := migrationInventory(t.Context(), old)
	if e != nil {
		t.Fatal(e)
	}
	b, e := migrationInventory(t.Context(), next)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) || r.Copied != len(a) {
		t.Fatal("old/new inventory mismatch")
	}
}

func TestStoragePrivateSwitchTransaction(t *testing.T) {
	for _, fault := range []string{"none", "cancel", "stop", "journal_switching", "set_root", "configure", "start", "journal_completed", "rollback"} {
		t.Run(fault, func(t *testing.T) {
			r, h, selected := migrationFixture(t)
			r.Mode = "private"
			r.NewRoot = t.TempDir()
			r.Parent = filepath.Dir(r.NewRoot)
			retained := filepath.Join(r.NewRoot, "existing-private-model")
			if err := os.WriteFile(retained, []byte("private bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(r.OldRoot, "blobs", "weights")
			before, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			save, set := h.Save, h.SetRoot
			h.Copy = func(context.Context, *os.Root, *os.Root, migrationFile) error {
				t.Fatal("private switch copied data")
				return nil
			}
			switch fault {
			case "cancel":
				cancel()
			case "stop":
				h.Stop = func() error { return errors.New("stop ACK failed") }
			case "journal_switching", "journal_completed":
				h.Save = func(r *migrationRecord) error {
					if "journal_"+r.State == fault {
						return errors.New("journal failure")
					}
					return save(r)
				}
			case "set_root":
				h.SetRoot = func(root string) error {
					if err := set(root); err != nil {
						return err
					}
					if root == r.NewRoot {
						return errors.New("setting visible but sync failed")
					}
					return nil
				}
			case "configure":
				h.ConfigureServer = func(c serverConfig) error {
					if c.Model != r.ServerConfig.Model {
						return errors.New("configure failure")
					}
					return nil
				}
			case "start", "rollback":
				h.Start = func(_ context.Context, _ *migrationRecord, forward bool) error {
					if forward || fault == "rollback" {
						return errors.New("restart failure")
					}
					return nil
				}
			}
			err = runStorageMigration(ctx, r, h)
			if fault == "none" {
				if err != nil || r.State != "completed" || *selected != r.NewRoot {
					t.Fatal(r.State, *selected, err)
				}
			} else {
				state := "rolled_back"
				if fault == "stop" || fault == "rollback" {
					state = "recovery_required"
				}
				if err == nil || r.State != state || *selected != r.OldRoot {
					t.Fatal(fault, r.State, *selected, err)
				}
			}
			if data, err := os.ReadFile(retained); err != nil || string(data) != "private bytes" {
				t.Fatal("private bytes changed", err)
			}
			if data, err := os.ReadFile(original); err != nil || !reflect.DeepEqual(data, before) {
				t.Fatal("custom bytes changed", err)
			}
			r.State = "rolled_back"
			if err := discardStorageCopy(t.Context(), r); err == nil {
				t.Fatal("private library accepted for discard")
			}
		})
	}
}

func TestStorageMigrationFaultMatrix(t *testing.T) {
	for _, fault := range []string{"cancel", "space", "copy_enospc", "bad_copy", "source_change", "selection_lost", "target_exists", "journal_preparing", "journal_copying", "journal_switching", "journal_completed", "set_root_after_commit", "start_new", "rollback_failure"} {
		t.Run(fault, func(t *testing.T) {
			r, h, selected := migrationFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			originalSave := h.Save
			originalSet := h.SetRoot
			switch fault {
			case "cancel":
				cancel()
			case "space":
				h.Space = func(string, int64) error { return syscall.ENOSPC }
			case "copy_enospc", "rollback_failure":
				h.Copy = func(ctx context.Context, source, target *os.Root, f migrationFile) error {
					target.MkdirAll(filepath.Dir(f.Name), 0700)
					out, e := target.OpenFile(f.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if e != nil {
						return e
					}
					in, e := source.Open(f.Name)
					if e != nil {
						out.Close()
						return e
					}
					io.CopyN(out, in, 7)
					in.Close()
					out.Close()
					return syscall.ENOSPC
				}
			case "bad_copy":
				h.Copy = func(ctx context.Context, source, target *os.Root, f migrationFile) error {
					if e := migrationCopyFile(ctx, source, target, f); e != nil {
						return e
					}
					return target.WriteFile(f.Name, []byte("wrong"), 0600)
				}
			case "source_change":
				h.Copy = func(ctx context.Context, source, target *os.Root, f migrationFile) error {
					if e := migrationCopyFile(ctx, source, target, f); e != nil {
						return e
					}
					return source.WriteFile(f.Name, []byte("changed externally"), 0600)
				}
			case "selection_lost":
				checks := 0
				h.CheckSelection = func() error {
					checks++
					if checks > 1 {
						return errors.New("selection changed")
					}
					return nil
				}
			case "target_exists":
				os.Mkdir(r.NewRoot, 0700)
				os.WriteFile(filepath.Join(r.NewRoot, "untouched"), []byte("existing user data"), 0600)
			case "set_root_after_commit":
				h.SetRoot = func(root string) error {
					originalSet(root)
					if root == r.NewRoot {
						return errors.New("post-commit settings error")
					}
					return nil
				}
			case "start_new":
				h.Start = func(_ context.Context, _ *migrationRecord, forward bool) error {
					if forward {
						return errors.New("new core failed")
					}
					return nil
				}
			default:
				phase := strings.TrimPrefix(fault, "journal_")
				fired := false
				h.Save = func(record *migrationRecord) error {
					if record.State == phase && !fired {
						fired = true
						return syscall.ENOSPC
					}
					return originalSave(record)
				}
			}
			if fault == "rollback_failure" {
				h.SetRoot = func(string) error { return errors.New("rollback settings unavailable") }
			}
			if err := runStorageMigration(ctx, r, h); err == nil {
				t.Fatal("fault passed migration")
			}
			if *selected != r.OldRoot {
				t.Fatal("old selection was not restored")
			}
			if fault == "journal_preparing" {
				if r.State != "preparing" {
					t.Fatal(r.State)
				}
			} else if fault == "rollback_failure" {
				if r.State != "recovery_required" {
					t.Fatal(r.State)
				}
			} else if r.State != "rolled_back" {
				t.Fatal(r.State)
			}
			if _, e := os.Stat(filepath.Join(r.OldRoot, "blobs", "weights")); e != nil {
				t.Fatal("old library removed", e)
			}
			if fault == "target_exists" {
				data, _ := os.ReadFile(filepath.Join(r.NewRoot, "untouched"))
				if string(data) != "existing user data" {
					t.Fatal("target overwritten")
				}
			}
		})
	}
}

func TestStorageMigrationRejectNestedAndSpecialSources(t *testing.T) {
	for _, kind := range []string{"nested", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			r, h, selected := migrationFixture(t)
			if kind == "nested" {
				r.Parent = r.OldRoot
				r.NewRoot = filepath.Join(r.Parent, "ollama-models-"+r.ID)
			} else {
				if err := os.Symlink("weights", filepath.Join(r.OldRoot, "blobs", "unsafe")); err != nil {
					t.Fatal(err)
				}
			}
			if err := runStorageMigration(t.Context(), r, h); err == nil || *selected != r.OldRoot {
				t.Fatal("unsafe migration accepted")
			}
		})
	}
}

func TestStorageMigrationDiscardOwnedPartial(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "unexpected_file"}[extra], func(t *testing.T) {
			r, h, _ := migrationFixture(t)
			h.Copy = func(ctx context.Context, source, target *os.Root, file migrationFile) error {
				if e := migrationCopyFile(ctx, source, target, file); e != nil {
					return e
				}
				return syscall.ENOSPC
			}
			if runStorageMigration(t.Context(), r, h) == nil {
				t.Fatal("expected partial failure")
			}
			if extra {
				os.WriteFile(filepath.Join(r.NewRoot, "user-extra"), []byte("do not delete"), 0600)
			}
			err := discardStorageCopy(t.Context(), r)
			if extra {
				if err == nil {
					t.Fatal("discarded unexpected user file")
				}
				if _, e := os.Stat(filepath.Join(r.NewRoot, ".pico-migration-owner")); e != nil {
					t.Fatal("mutated before validating all entries")
				}
			} else if err != nil {
				t.Fatal(err)
			} else if _, e := os.Stat(r.NewRoot); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("owned partial remains")
			}
			if _, e := os.Stat(r.OldRoot); e != nil {
				t.Fatal("old store lost")
			}
		})
	}
}
