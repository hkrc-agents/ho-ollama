//go:build harmonyos

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

type migrationFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}
type migrationRecord struct {
	TargetDevice uint64            `json:"target_device"`
	TargetInode  uint64            `json:"target_inode"`
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	State        string            `json:"state"`
	OldRoot      string            `json:"old_root"`
	NewRoot      string            `json:"new_root"`
	Parent       string            `json:"parent"`
	SelectionID  string            `json:"selection_id"`
	Mode         string            `json:"mode,omitempty"`
	OldSelection *storageSelection `json:"old_selection,omitempty"`
	NewSelection *storageSelection `json:"new_selection,omitempty"`
	CoreReady    bool              `json:"core_ready"`
	ServerReady  bool              `json:"server_ready"`
	ServerConfig serverConfig      `json:"server_config"`
	Files        []migrationFile   `json:"files"`
	Copied       int               `json:"copied"`
	Bytes        int64             `json:"bytes"`
	Error        string            `json:"error,omitempty"`
	Updated      string            `json:"updated"`
}

// Replacement may be visible even if directory Sync fails. Callers must treat
// ANY error as uncertain and re-read/recover, not claim an atomic rollback.
func storageAtomicJSON(path string, value any) (err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("storage journal exceeds limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".storage-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
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
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func storageReadJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("storage record exceeds limit")
	}
	return json.Unmarshal(data, value)
}

func migrationInventory(ctx context.Context, root *os.Root) ([]migrationFile, error) {
	files := []migrationFile{}
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if name == ".pico-migration-owner" {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special/link entry: %s", name)
		}
		if len(files) >= 16384 {
			return errors.New("model library exceeds migration file-count limit")
		}
		f, err := root.OpenFile(name, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		opened, e := f.Stat()
		if e == nil && !os.SameFile(info, opened) {
			e = errors.New("source entry changed while opening")
		}
		h := sha256.New()
		var n int64
		if e == nil {
			n, e = io.CopyBuffer(h, harmonyContextReader{ctx, f}, make([]byte, 128<<10))
		}
		after, aerr := f.Stat()
		e = errors.Join(e, aerr, f.Close())
		if e != nil {
			return e
		}
		if n != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return errors.New("model library changed while hashing")
		}
		files = append(files, migrationFile{name, n, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, err
}

type migrationHooks struct {
	Save            func(*migrationRecord) error
	Stop            func() error
	SetRoot         func(string) error
	GetRoot         func() (string, error)
	ConfigureServer func(serverConfig) error
	Start           func(context.Context, *migrationRecord, bool) error
	CheckSelection  func() error
	Space           func(string, int64) error
	// Copy is a per-transaction bounded fault seam, never chosen by an HTTP field.
	Copy func(context.Context, *os.Root, *os.Root, migrationFile) error
}

func migrationCopyFile(ctx context.Context, source, target *os.Root, item migrationFile) error {
	if err := target.MkdirAll(filepath.Dir(item.Name), 0700); err != nil {
		return err
	}
	f, err := source.Open(item.Name)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != item.Size {
		return errors.New("source file changed before copy")
	}
	out, err := target.OpenFile(item.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), harmonyContextReader{ctx, f}, make([]byte, 128<<10))
	if err == nil && (n != item.Size || hex.EncodeToString(h.Sum(nil)) != item.Digest) {
		err = errors.New("source content changed during copy")
	}
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

func migrationMappedConfig(record *migrationRecord, forward bool) serverConfig {
	c := record.ServerConfig
	if !forward {
		return c
	}
	rel, err := filepath.Rel(record.OldRoot, c.Model)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		c.Model = filepath.Join(record.NewRoot, rel)
	}
	return c
}

func migrationSave(record *migrationRecord, h migrationHooks, state string) error {
	record.State = state
	record.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	return h.Save(record)
}

// Rollback changes only the APP-selected root/configuration, never the old
// library contents. A partial target is retained and explicitly discardable.
func migrationRollback(record *migrationRecord, h migrationHooks, cause error) error {
	rollback := h.Stop()
	if rollback == nil {
		rollback = h.SetRoot(record.OldRoot)
	}
	if rollback == nil {
		root, err := h.GetRoot()
		rollback = err
		if root != record.OldRoot {
			rollback = errors.Join(rollback, errors.New("old library selection not restored"))
		}
	}
	if rollback == nil {
		rollback = h.ConfigureServer(record.ServerConfig)
	}
	if rollback == nil {
		rollback = h.Start(context.Background(), record, false)
	}
	record.Error = cause.Error()
	state := "rolled_back"
	if rollback != nil {
		state = "recovery_required"
		record.Error = errors.Join(cause, rollback).Error()
	}
	if err := migrationSave(record, h, state); err != nil {
		record.State = "recovery_required"
		record.Error = errors.Join(cause, rollback, err).Error()
		return errors.New(record.Error)
	}
	return errors.Join(cause, rollback)
}

func runStorageMigration(ctx context.Context, record *migrationRecord, h migrationHooks) error {
	if record.Mode == "private" {
		return returnToPrivateStorage(ctx, record, h)
	}
	if h.Copy == nil {
		h.Copy = migrationCopyFile
	}
	if err := h.CheckSelection(); err != nil {
		return err
	}
	if err := migrationSave(record, h, "preparing"); err != nil {
		return err
	}
	if err := h.Stop(); err != nil {
		return migrationRollback(record, h, err)
	}
	fail := func(err error) error { return migrationRollback(record, h, err) }
	source, err := os.OpenRoot(record.OldRoot)
	if err != nil {
		return fail(err)
	}
	defer source.Close()
	parent, err := os.OpenRoot(record.Parent)
	if err != nil {
		return fail(err)
	}
	defer parent.Close()
	// Never copy into, over or underneath the old store. Fresh dedicated name only.
	old, err := filepath.EvalSymlinks(record.OldRoot)
	if err != nil {
		return fail(err)
	}
	parentPath, err := filepath.EvalSymlinks(record.Parent)
	if err != nil {
		return fail(err)
	}
	rel, err := filepath.Rel(old, parentPath)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return fail(errors.New("migration target parent must not be inside the old library"))
	}
	record.Files, err = migrationInventory(ctx, source)
	if err != nil {
		return fail(err)
	}
	var bytes int64
	for _, item := range record.Files {
		if item.Size > 1<<50-bytes {
			return fail(errors.New("library size overflow"))
		}
		bytes += item.Size
	}
	if err = h.Space(record.Parent, bytes); err != nil {
		return fail(err)
	}
	name := "ollama-models-" + record.ID
	if record.NewRoot != filepath.Join(record.Parent, name) {
		return fail(errors.New("invalid dedicated migration target"))
	}
	if err = parent.Mkdir(name, 0700); err != nil {
		return fail(err)
	}
	target, err := parent.OpenRoot(name)
	if err != nil {
		return fail(err)
	}
	defer target.Close()
	directory, err := target.Open(".")
	if err != nil {
		return fail(err)
	}
	info, err := directory.Stat()
	directory.Close()
	if err != nil {
		return fail(err)
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fail(errors.New("target identity unavailable"))
	}
	record.TargetDevice, record.TargetInode = uint64(identity.Dev), identity.Ino
	marker, err := target.OpenFile(".pico-migration-owner", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	n, err := marker.WriteString(record.ID + "\n")
	if err == nil && n != len(record.ID)+1 {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = marker.Sync()
	}
	err = errors.Join(err, marker.Close())
	if err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "copying"); err != nil {
		return fail(err)
	}
	for _, item := range record.Files {
		if err = h.Copy(ctx, source, target, item); err != nil {
			return fail(err)
		}
		record.Copied++
		record.Bytes += item.Size
		if err = migrationSave(record, h, "copying"); err != nil {
			return fail(err)
		}
	}
	if err = migrationSave(record, h, "verifying"); err != nil {
		return fail(err)
	}
	actual, err := migrationInventory(ctx, target)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(actual, record.Files) {
		return fail(errors.New("copied library verification failed"))
	}
	unchanged, err := migrationInventory(ctx, source)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(unchanged, record.Files) {
		return fail(errors.New("old library changed during migration"))
	}
	if err = h.CheckSelection(); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	// This is the cancellation boundary. Never abandon a half-updated selection.
	if err = migrationSave(record, h, "switching"); err != nil {
		return fail(err)
	}
	// Public weights in an explicitly selected user directory remain readable to
	// independently authorized CLI/SDK callers. Only our own copied nodes change.
	var directories []string
	err = fs.WalkDir(target.FS(), ".", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		mode := os.FileMode(0644)
		if d.IsDir() {
			mode = 0755
			directories = append(directories, path)
		}
		if path == ".pico-migration-owner" {
			mode = 0600
		}
		return target.Chmod(path, mode)
	})
	if err != nil {
		return fail(err)
	}
	for i := len(directories) - 1; i >= 0; i-- {
		d, e := target.Open(directories[i])
		if e != nil {
			return fail(e)
		}
		e = errors.Join(d.Sync(), d.Close())
		if e != nil {
			return fail(e)
		}
	}
	for _, root := range []*os.Root{target, parent} {
		d, e := root.Open(".")
		if e != nil {
			return fail(e)
		}
		e = errors.Join(d.Sync(), d.Close())
		if e != nil {
			return fail(e)
		}
	}
	if err = h.SetRoot(record.NewRoot); err != nil {
		return fail(err)
	}
	selected, err := h.GetRoot()
	if err != nil || selected != record.NewRoot {
		return fail(errors.Join(err, errors.New("new root selection not confirmed")))
	}
	if err = h.ConfigureServer(migrationMappedConfig(record, true)); err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "starting"); err != nil {
		return fail(err)
	}
	if err = h.Start(context.Background(), record, true); err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "completed"); err != nil {
		return fail(err)
	}
	return nil
}

// Switch to the retained private store under the same journal, stop/ACK,
// configuration, cancellation boundary and rollback rules as a copied migration.
// Neither library's contents are changed. A configured standalone model must
// already exist in the private library to restart successfully, or we roll back.
func returnToPrivateStorage(ctx context.Context, record *migrationRecord, h migrationHooks) error {
	if err := migrationSave(record, h, "preparing"); err != nil {
		return err
	}
	fail := func(err error) error { return migrationRollback(record, h, err) }
	if err := h.Stop(); err != nil {
		return fail(err)
	}
	root, err := os.OpenRoot(record.NewRoot)
	if err != nil {
		return fail(err)
	}
	info, err := os.Lstat(record.NewRoot)
	if err == nil && !info.IsDir() {
		err = errors.New("private library must not be replaced by a link or file")
	}
	err = errors.Join(err, root.Close())
	if err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "switching"); err != nil {
		return fail(err)
	}
	if err = h.SetRoot(record.NewRoot); err != nil {
		return fail(err)
	}
	selected, err := h.GetRoot()
	if err != nil || selected != record.NewRoot {
		return fail(errors.Join(err, errors.New("private root selection not confirmed")))
	}
	if err = h.ConfigureServer(migrationMappedConfig(record, true)); err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "starting"); err != nil {
		return fail(err)
	}
	if err = h.Start(context.Background(), record, true); err != nil {
		return fail(err)
	}
	if err = migrationSave(record, h, "completed"); err != nil {
		return fail(err)
	}
	return nil
}

// Explicit discard of an inactive failed copy. Verify the exact owned root,
// every filename and every complete/prefix copy BEFORE removing anything.
func discardStorageCopy(ctx context.Context, record *migrationRecord) error {
	if record.State != "rolled_back" || record.Mode == "private" {
		return errors.New("only a rolled-back inactive copy can be discarded")
	}
	parent, err := os.OpenRoot(record.Parent)
	if err != nil {
		return err
	}
	defer parent.Close()
	name := "ollama-models-" + record.ID
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || record.TargetInode == 0 || uint64(identity.Dev) != record.TargetDevice || identity.Ino != record.TargetInode {
		return errors.New("target identity changed; refusing discard")
	}
	target, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer target.Close()
	marker, err := target.ReadFile(".pico-migration-owner")
	if err != nil || string(marker) != record.ID+"\n" {
		return errors.New("migration ownership marker mismatch")
	}
	source, err := os.OpenRoot(record.OldRoot)
	if err != nil {
		return err
	}
	defer source.Close()
	expected := map[string]migrationFile{}
	dirs := map[string]bool{".": true}
	for _, file := range record.Files {
		expected[file.Name] = file
		for dir := filepath.Dir(file.Name); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
	}
	var files, folders []string
	err = fs.WalkDir(target.FS(), ".", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if d.IsDir() {
			if !dirs[path] {
				return errors.New("unexpected directory; copy retained")
			}
			folders = append(folders, path)
			return nil
		}
		info, e := target.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("unexpected special file; copy retained")
		}
		if path == ".pico-migration-owner" {
			files = append(files, path)
			return nil
		}
		item, ok := expected[path]
		if !ok || info.Size() > item.Size {
			return errors.New("unexpected/modified file; copy retained")
		}
		f, e := target.Open(path)
		if e != nil {
			return e
		}
		h := sha256.New()
		n, e := io.CopyBuffer(h, harmonyContextReader{ctx, f}, make([]byte, 128<<10))
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		actual := hex.EncodeToString(h.Sum(nil))
		if n == item.Size {
			if actual != item.Digest {
				return errors.New("modified copied file; copy retained")
			}
		} else {
			original, e := source.Open(path)
			if e != nil {
				return e
			}
			h.Reset()
			_, e = io.Copy(h, io.LimitReader(harmonyContextReader{ctx, original}, n))
			e = errors.Join(e, original.Close())
			if e != nil {
				return e
			}
			if actual != hex.EncodeToString(h.Sum(nil)) {
				return errors.New("modified partial file; copy retained")
			}
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		if err = target.Remove(path); err != nil {
			return err
		}
	}
	for i := len(folders) - 1; i >= 0; i-- {
		if folders[i] != "." {
			if err = target.Remove(folders[i]); err != nil {
				return err
			}
		}
	}
	if err = parent.Remove(name); err != nil {
		return err
	}
	d, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
