//go:build harmonyos

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ollama/ollama/api"
	coreapi "github.com/ollama/ollama/app/server"
	"github.com/ollama/ollama/envconfig"
)

type harmonyStorageManager struct {
	directory   string
	token       string
	core        *harmonyCore
	server      *harmonyServer
	lifetime    context.Context
	gate        sync.RWMutex
	modelUse    sync.RWMutex
	mu          sync.Mutex
	last        *migrationRecord
	blocked     string
	cancel      context.CancelFunc
	jobContext  context.Context
	inspect     func(context.Context, string, string) (harmonyModelFile, error)
	cacheMu     sync.Mutex
	cacheKey    string
	cacheModels []serverModel
}

func newHarmonyStorageManager(directory, token string, core *harmonyCore, server *harmonyServer, life context.Context) *harmonyStorageManager {
	return &harmonyStorageManager{directory: directory, token: token, core: core, server: server, lifetime: life, inspect: harmonyRunInspect}
}
func (m *harmonyStorageManager) json(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func (m *harmonyStorageManager) authenticated(r *http.Request) bool {
	c, err := r.Cookie("token")
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(m.token)) == 1
}
func (m *harmonyStorageManager) model(ctx context.Context, name string) (harmonyModelFile, error) {
	root, err := m.core.activeModelRoot()
	if err != nil {
		return harmonyModelFile{}, err
	}
	return m.inspect(ctx, root, name)
}

func (m *harmonyStorageManager) inventory() ([]serverModel, error) {
	root, err := m.core.activeModelRoot()
	if err != nil {
		return nil, err
	}
	if m.core.Snapshot().State != "ready" {
		return []serverModel{}, nil
	}
	ctx, cancel := context.WithTimeout(m.lifetime, 30*time.Second)
	defer cancel()
	client := api.NewClient(envconfig.ConnectableHost(), http.DefaultClient)
	tags, err := client.List(ctx)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(tags.Models)
	key := root + string(data)
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if key == m.cacheKey {
		return append([]serverModel{}, m.cacheModels...), nil
	}
	items := []serverModel{}
	for _, tag := range tags.Models {
		file, e := m.inspect(ctx, root, tag.Name)
		if e == nil {
			items = append(items, serverModel{tag.Name, file.Path})
		}
	}
	m.cacheKey, m.cacheModels = key, items
	return items, nil
}

func (m *harmonyStorageManager) openVerifiedModel(ctx context.Context, file harmonyModelFile) (*os.File, error) {
	root, err := m.core.activeModelRoot()
	if err != nil {
		return nil, err
	}
	scope, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer scope.Close()
	if file.Path != filepath.Join(root, "blobs", strings.ReplaceAll(file.Digest, ":", "-")) {
		return nil, errors.New("invalid model descriptor")
	}
	f, err := scope.OpenFile(filepath.Join("blobs", strings.ReplaceAll(file.Digest, ":", "-")), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	valid := false
	defer func() {
		if !valid {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
		return nil, errors.New("weight file changed")
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, harmonyContextReader{ctx, f}, make([]byte, 128<<10))
	if err != nil || n != file.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != file.Digest {
		return nil, errors.New("weight checksum mismatch; no export sent")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	valid = true
	return f, nil
}

func (m *harmonyStorageManager) verifiedModel(ctx context.Context, name string) (harmonyModelFile, error) {
	file, err := m.model(ctx, name)
	if err != nil {
		return file, err
	}
	f, err := m.openVerifiedModel(ctx, file)
	if err != nil {
		return file, err
	}
	if err = f.Close(); err != nil {
		return file, err
	}
	again, err := m.model(ctx, name)
	if err != nil {
		return file, err
	}
	if again.Digest != file.Digest || again.Manifest != file.Manifest || again.Size != file.Size {
		return file, errors.New("model reference changed during verification")
	}
	return file, nil
}

func (m *harmonyStorageManager) export(w http.ResponseWriter, r *http.Request, metadata bool) {
	file, err := m.model(r.Context(), r.URL.Query().Get("model"))
	if err != nil {
		http.Error(w, "Cannot export this model as one GGUF: "+err.Error(), 409)
		return
	}
	if metadata {
		file.Path = ""
		m.json(w, file)
		return
	}
	f, err := m.openVerifiedModel(r.Context(), file)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("X-GGUF-SHA256", strings.TrimPrefix(file.Digest, "sha256:"))
	w.Header().Set("Content-Disposition", `attachment; filename="model.gguf"`)
	// One pinned descriptor survives manifest/blob unlink. Native receiver also
	// hashes the completed staging file before publishing to its picker URI.
	io.CopyBuffer(w, harmonyContextReader{r.Context(), f}, make([]byte, 128<<10))
}

func (m *harmonyStorageManager) save(record *migrationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record.State == "switching" && m.jobContext != nil && m.jobContext.Err() != nil {
		return m.jobContext.Err()
	}
	err := storageAtomicJSON(filepath.Join(m.directory, "storage-migration.json"), record)
	copy := *record
	copy.Files = append([]migrationFile(nil), record.Files...)
	m.last = &copy
	if err != nil {
		m.blocked = "Cannot persist migration state; recovery required: " + err.Error()
	} else if record.State == "rolled_back" || record.State == "completed" || record.State == "discarded" {
		m.blocked = ""
	}
	return err
}

func (m *harmonyStorageManager) stopped() error {
	if err := m.server.stop(); err != nil {
		return err
	}
	if err := m.core.Stop(); err != nil {
		return err
	}
	// A newly constructed supervisor must not assume a historical PID is gone.
	id, err := coreapi.CoreReadIdentity(m.core.pidPath + ".identity.json")
	if err == nil {
		current, e := coreapi.CoreProcessIdentity(id.PID)
		if e == nil && reflect.DeepEqual(id, current) {
			return errors.New("old non-child core is still live; normal owner must stop it before recovery")
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("old core ownership unknown: %w", e)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *harmonyStorageManager) setRoot(root string) error {
	settings, err := m.core.database.Settings()
	if err != nil {
		return err
	}
	settings.Models = root
	return m.core.database.SetSettings(settings)
}

func (m *harmonyStorageManager) configure(config serverConfig) error {
	m.server.op.Lock()
	defer m.server.op.Unlock()
	if m.server.snapshot().State != "stopped" {
		return errors.New("standalone server must be stopped before changing model root")
	}
	if err := storageAtomicJSON(filepath.Join(m.directory, "llama-server-config.json"), config); err != nil {
		return err
	}
	m.server.mu.Lock()
	m.server.current.Config = config
	m.server.mu.Unlock()
	return nil
}

func (m *harmonyStorageManager) hooks(check func() error) migrationHooks {
	return migrationHooks{Save: m.save, Stop: m.stopped, SetRoot: m.setRoot, GetRoot: m.core.selectedModelRoot, ConfigureServer: m.configure, CheckSelection: check,
		Space: func(path string, size int64) error { return harmonyGGUFSpace(path, (size+1)/2) },
		Start: func(_ context.Context, record *migrationRecord, forward bool) error {
			if err := m.lifetime.Err(); err != nil {
				return err
			}
			if record.CoreReady {
				if err := m.core.Start(m.lifetime); err != nil {
					return err
				}
			}
			if record.ServerReady {
				return m.server.start(m.lifetime, migrationMappedConfig(record, forward))
			}
			return nil
		}}
}

func (m *harmonyStorageManager) status(w http.ResponseWriter) {
	root, err := m.core.activeModelRoot()
	message := ""
	if err != nil {
		message = err.Error()
	}
	// Report the saved selection even if access has been revoked. Never present
	// the private library as active merely because the custom root is unavailable.
	defaultRoot, defaultErr := privateStorageRoot()
	configuredRoot := ""
	mode := "unknown"
	settings, settingsErr := m.core.database.Settings()
	if settingsErr == nil {
		configuredRoot = settings.Models
		mode = "custom"
		if defaultErr == nil && filepath.Clean(configuredRoot) == defaultRoot {
			mode = "private"
		}
	}
	// Startup policy checks are independent of the latest staged Picker record.
	// Surface a revoked active grant even if stat() can still see its directory.
	authorizationError := ""
	if mode == "custom" {
		var receipt struct {
			Schema   int    `json:"schema"`
			OldRoot  string `json:"old_root"`
			NewRoot  string `json:"new_root"`
			OldError string `json:"old_error"`
			NewError string `json:"new_error"`
			Error    string `json:"error"`
		}
		e := storageReadJSON(filepath.Join(filepath.Dir(m.directory), "model-directory-active-restore.json"), &receipt)
		if e == nil && receipt.Schema == 1 {
			authorizationError = receipt.Error
			if configuredRoot == receipt.OldRoot && receipt.OldError != "" {
				authorizationError = receipt.OldError
			}
			if configuredRoot == receipt.NewRoot && receipt.NewError != "" {
				authorizationError = receipt.NewError
			}
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			authorizationError = "Saved library authorization restore result unavailable; restart or recover storage."
		}
		if authorizationError != "" {
			root = ""
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var record *migrationRecord
	if m.last != nil {
		copy := *m.last
		copy.Files = nil
		record = &copy
	}
	m.json(w, map[string]any{"root": root, "configured_root": configuredRoot, "default_root": defaultRoot, "mode": mode,
		"error": message, "authorization_error": authorizationError, "blocked": m.blocked, "migration": record, "running": m.cancel != nil})
}

// The manager directory is files/desktop, not HOME. HAP provisions HOME/models.
// Do not derive this from OLLAMA_MODELS, which may already name a custom store.
func privateStorageRoot() (string, error) {
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		return "", errors.New("private app home unavailable")
	}
	return filepath.Join(home, "models"), nil
}

func decodeStorageRequest(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("trailing request")
	}
	return nil
}

func (m *harmonyStorageManager) begin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SelectionID string `json:"selection_id"`
		Confirm     bool   `json:"confirm"`
		Mode        string `json:"mode"`
	}
	if decodeStorageRequest(w, r, &request) != nil || !request.Confirm || (request.Mode != "" && request.Mode != "custom" && request.Mode != "private") || (request.Mode == "private" && request.SelectionID != "") {
		http.Error(w, "confirm migration and provide the native selection id", 400)
		return
	}
	if !m.gate.TryLock() {
		http.Error(w, "active model/service operation; cancel or wait before migration", 409)
		return
	}
	release := true
	defer func() {
		if release {
			m.gate.Unlock()
		}
	}()
	m.mu.Lock()
	blocked := m.blocked
	last := m.last
	m.mu.Unlock()
	if blocked != "" || (last != nil && last.State != "completed" && last.State != "rolled_back" && last.State != "discarded") {
		http.Error(w, "recover/discard the previous migration first", 409)
		return
	}
	if last != nil && last.State == "rolled_back" && last.Mode != "private" {
		if _, err := os.Lstat(last.NewRoot); err == nil {
			http.Error(w, "discard the previous inactive copy before starting another migration", 409)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "previous copy state unavailable", 409)
			return
		}
	}
	var selection storageSelection
	var err error
	check := func() error { return nil }
	parent, target := "", ""
	if request.Mode == "private" {
		target, err = privateStorageRoot()
		if err == nil {
			// This is a switch to the retained private library, never an overwrite
			// or merge. The HAP creates this directory; do not create an arbitrary root.
			info, e := os.Lstat(target)
			err = e
			if e == nil && !info.IsDir() {
				err = errors.New("private library must be an existing directory, not a link")
			}
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		parent = filepath.Dir(target)
	} else {
		selection, err = readStorageSelection(m.directory)
		if err == nil {
			err = validStorageSelection(selection, request.SelectionID)
		}
		if err != nil {
			http.Error(w, "Native persistent directory authorization is not confirmed: "+err.Error(), 409)
			return
		}
		check = func() error {
			latest, e := readStorageSelection(m.directory)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(latest, selection) {
				return errors.New("directory authorization selection changed during migration")
			}
			return validStorageSelection(latest, selection.ID)
		}
		runner, err := installedStorageProbeRunner()
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		probeCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		proof := runner.run(probeCtx, selection)
		if !proof.Passed {
			http.Error(w, "Go/native file operations did not qualify selected directory: "+proof.Error, 409)
			return
		}
		parent = selection.Path
	}
	// Explicitly returning to the private library is possible even after a
	// custom grant is revoked. Preserve the saved old path for rollback.
	settings, err := m.core.database.Settings()
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	old := settings.Models
	if request.Mode != "private" {
		old, err = m.core.activeModelRoot()
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	}
	if !filepath.IsAbs(old) || filepath.Clean(old) == target {
		http.Error(w, "invalid or already selected model library", 409)
		return
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	key := hex.EncodeToString(id)
	if target == "" {
		target = filepath.Join(parent, "ollama-models-"+key)
	}
	record := &migrationRecord{Version: 1, ID: key, Mode: request.Mode, OldRoot: old, Parent: parent, NewRoot: target, SelectionID: selection.ID, CoreReady: m.core.Snapshot().State == "ready", ServerReady: m.server.snapshot().State == "ready", ServerConfig: m.server.snapshot().Config, Files: []migrationFile{}}
	if request.Mode != "private" {
		record.NewSelection = &selection
	}
	if last != nil {
		if last.State == "completed" && last.NewRoot == old {
			record.OldSelection = last.NewSelection
		} else if last.OldRoot == old {
			record.OldSelection = last.OldSelection
		}
	}
	ctx, cancelJob := context.WithCancel(m.lifetime)
	m.mu.Lock()
	m.cancel = cancelJob
	m.jobContext = ctx
	m.mu.Unlock()
	release = false
	go func() {
		defer m.gate.Unlock()
		defer cancelJob()
		err := runStorageMigration(ctx, record, m.hooks(check))
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cancel = nil
		m.jobContext = nil
		if err != nil && (m.last == nil || m.last.State == "preparing") {
			m.blocked = err.Error()
		}
		if record.State == "recovery_required" {
			m.blocked = record.Error
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	m.json(w, map[string]string{"id": key})
}

func (m *harmonyStorageManager) Recover() error {
	m.gate.Lock()
	defer m.gate.Unlock()
	var record migrationRecord
	err := storageReadJSON(filepath.Join(m.directory, "storage-migration.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Lock()
		m.blocked = ""
		m.mu.Unlock()
		return nil
	}
	if err == nil {
		id, e := hex.DecodeString(record.ID)
		allowed := map[string]bool{"preparing": true, "copying": true, "verifying": true, "switching": true, "starting": true, "completed": true, "rolled_back": true, "recovery_required": true, "discarded": true}
		validTarget := (record.Mode == "" || record.Mode == "custom") && filepath.IsAbs(record.Parent) && record.NewRoot == filepath.Join(record.Parent, "ollama-models-"+record.ID)
		if record.Mode == "private" {
			root, rootErr := privateStorageRoot()
			validTarget = rootErr == nil && record.NewRoot == root && record.Parent == filepath.Dir(root)
		}
		if e != nil || len(id) != 16 || record.Version != 1 || !allowed[record.State] || !filepath.IsAbs(record.OldRoot) || !validTarget {
			err = errors.New("invalid migration journal")
		}
	}
	if err != nil {
		m.mu.Lock()
		m.blocked = "Migration journal unreadable; no automatic library switch: " + err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	copy := record
	m.last = &copy
	m.mu.Unlock()
	if record.State == "completed" || record.State == "rolled_back" || record.State == "discarded" {
		m.mu.Lock()
		m.blocked = ""
		m.mu.Unlock()
		return nil
	}
	h := m.hooks(func() error { return nil })
	// Recovery never chooses a second library silently: interrupted transactions
	// explicitly restore the recorded old selection/configuration, keeping copies.
	err = migrationRollback(&record, h, errors.New("interrupted migration recovered to old library"))
	if record.State == "rolled_back" {
		m.mu.Lock()
		m.blocked = ""
		m.mu.Unlock()
		return nil
	}
	return err
}

func (m *harmonyStorageManager) discard(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if decodeStorageRequest(w, r, &request) != nil || !request.Confirm {
		http.Error(w, "explicit discard confirmation required", 400)
		return
	}
	if !m.gate.TryLock() {
		http.Error(w, "model operation active", 409)
		return
	}
	defer m.gate.Unlock()
	m.mu.Lock()
	var record *migrationRecord
	if m.last != nil {
		copy := *m.last
		record = &copy
	}
	m.mu.Unlock()
	if record == nil || request.ID != record.ID || record.State != "rolled_back" || record.Mode == "private" {
		http.Error(w, "no matching rolled-back copy", 409)
		return
	}
	selected, err := m.core.selectedModelRoot()
	if err != nil || selected == record.NewRoot {
		http.Error(w, "cannot discard an active/unknown library", 409)
		return
	}
	selectedInfo, e := os.Stat(selected)
	targetInfo, targetErr := os.Stat(record.NewRoot)
	if e != nil || (targetErr != nil && !errors.Is(targetErr, os.ErrNotExist)) || (targetErr == nil && os.SameFile(selectedInfo, targetInfo)) {
		http.Error(w, "copy aliases an active or unknown library", 409)
		return
	}
	selection, err := readStorageSelection(m.directory)
	if err == nil {
		err = validStorageSelection(selection, selection.ID)
	}
	if err != nil || selection.Path != record.Parent {
		http.Error(w, "reselect and authorize the recorded parent before discarding this copy", 409)
		return
	}
	s := m.server.snapshot()
	if s.State != "stopped" {
		http.Error(w, "stop independent server before discard", 409)
		return
	}
	if err = discardStorageCopy(r.Context(), record); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if err = migrationSave(record, m.hooks(func() error { return nil }), "discarded"); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	m.status(w)
}

func (m *harmonyStorageManager) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !m.authenticated(r) {
			http.Error(w, "APP token required", 403)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/harmony/storage/status":
			m.status(w)
			return
		case r.Method == "POST" && r.URL.Path == "/api/harmony/storage/migrate":
			m.begin(w, r)
			return
		case r.Method == "POST" && r.URL.Path == "/api/harmony/storage/cancel":
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.cancel == nil {
				http.Error(w, "no migration running", 409)
				return
			}
			if m.last != nil && (m.last.State == "switching" || m.last.State == "starting") {
				http.Error(w, "commit has begun; wait for completion or recovery", 409)
				return
			}
			m.cancel()
			m.json(w, map[string]bool{"cancel_requested": true})
			return
		case r.Method == "POST" && r.URL.Path == "/api/harmony/storage/recover":
			m.mu.Lock()
			running := m.cancel != nil
			m.mu.Unlock()
			if running {
				http.Error(w, "migration is active", 409)
				return
			}
			if err := m.Recover(); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			m.status(w)
			return
		case r.Method == "POST" && r.URL.Path == "/api/harmony/storage/discard":
			m.discard(w, r)
			return
		}
		if !m.gate.TryRLock() {
			http.Error(w, "model library migration active; retry after completion", 423)
			return
		}
		defer m.gate.RUnlock()
		deleting := r.Method == "DELETE" && r.URL.Path == "/api/delete"
		using := r.Method == "POST" && (r.URL.Path == "/api/harmony/gguf" || r.URL.Path == "/api/pull" || r.URL.Path == "/api/generate" || r.URL.Path == "/api/chat" || r.URL.Path == "/api/harmony/services" || strings.HasPrefix(r.URL.Path, "/api/v1/chat"))
		using = using || (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/harmony/gguf/"))
		if deleting {
			if !m.modelUse.TryLock() {
				http.Error(w, "download/import/inference/export active; wait or cancel before deleting", 409)
				return
			}
			defer m.modelUse.Unlock()
		} else if using {
			if !m.modelUse.TryRLock() {
				http.Error(w, "model deletion active; retry after completion", 409)
				return
			}
			defer m.modelUse.RUnlock()
		}
		if r.Method == "POST" && r.URL.Path == "/api/v1/settings" {
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				http.Error(w, "settings too large", 400)
				return
			}
			var values map[string]json.RawMessage
			if json.Unmarshal(data, &values) != nil {
				http.Error(w, "invalid settings", 400)
				return
			}
			settings, err := m.core.database.Settings()
			if err != nil {
				http.Error(w, err.Error(), 503)
				return
			}
			for key, value := range values {
				if strings.EqualFold(key, "models") {
					var requested string
					if json.Unmarshal(value, &requested) != nil || requested != settings.Models {
						http.Error(w, "use the authorized migration workflow to change model location", 409)
						return
					}
					delete(values, key)
				}
			}
			values["Models"], _ = json.Marshal(settings.Models)
			data, _ = json.Marshal(values)
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
		}
		m.mu.Lock()
		blocked := m.blocked
		m.mu.Unlock()
		if blocked != "" && r.Method != "GET" {
			http.Error(w, "storage recovery required: "+blocked, 409)
			return
		}
		if deleting {
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
			if err != nil {
				http.Error(w, "invalid delete body", 400)
				return
			}
			var request struct {
				Model string `json:"model"`
				Name  string `json:"name"`
			}
			if json.Unmarshal(data, &request) != nil {
				http.Error(w, "invalid delete request", 400)
				return
			}
			name := request.Model
			if name == "" {
				name = request.Name
			}
			key, err := harmonyModelKey(name)
			if err != nil {
				http.Error(w, "invalid model name", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
			if state := m.server.snapshot().State; state == "ready" || state == "starting" {
				file, e := m.model(r.Context(), name)
				configured := m.server.snapshot().Config.Model
				inUse := e != nil || file.Path == configured
				if !inUse {
					a, e1 := os.Stat(file.Path)
					b, e2 := os.Stat(configured)
					inUse = e1 != nil || e2 != nil || os.SameFile(a, b)
				}
				if inUse {
					http.Error(w, "stop independent llama-server before deleting its model", 409)
					return
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			client := api.NewClient(envconfig.ConnectableHost(), http.DefaultClient)
			running, err := client.ListRunning(ctx)
			if err != nil {
				http.Error(w, "cannot confirm models are unloaded", 409)
				return
			}
			for _, item := range running.Models {
				loaded, e := harmonyModelKey(item.Name)
				if e != nil || loaded == key {
					http.Error(w, "unload this Ollama model before deleting", 409)
					return
				}
			}
		}
		if r.Method == "GET" && r.URL.Path == "/api/harmony/gguf/metadata" {
			m.export(w, r, true)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/harmony/gguf/export" {
			m.export(w, r, false)
			return
		}
		next.ServeHTTP(w, r)
	})
}
