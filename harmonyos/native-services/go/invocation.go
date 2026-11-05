//go:build harmonyos

package nativeprocess

// Invocation is an official managed instance, not an os.Process/exec.Cmd. Its
// program result comes only from the instance-bound SO return receipt; neither
// a successful kill nor callback signal zero manufactures a successful exit.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Launch struct {
	Schema         int      `json:"schema"`
	Kind           string   `json:"kind"`
	Args           []string `json:"argv"`
	Env            []string `json:"env"`
	Directory      string   `json:"directory"`
	ProbeDirectory string   `json:"probe_directory,omitempty"`
	ProbeDev       uint64   `json:"probe_dev,omitempty"`
	ProbeIno       uint64   `json:"probe_ino,omitempty"`
	MemoryMiB int `json:"memory_mib,omitempty"`
}

// ReturnReceipt is written atomically by the native SO entry only after the
// component returns. A crash deliberately leaves it absent.
type ReturnReceipt struct {
	Session  string `json:"hap_session"`
	Instance uint64 `json:"instance"`
	PID      int    `json:"pid"`
	Code     int    `json:"code"`
}

type Result struct {
	Status  Status
	Receipt *ReturnReceipt
}

type Invocation struct {
	client    *Client
	Instance  uint64
	Directory string
}

func (l Launch) Validate() error {
	if l.MemoryMiB<0||l.MemoryMiB>1536||(l.MemoryMiB>0&&l.MemoryMiB<128){return errors.New("managed memory budget must be 128–1536 MiB or the default")}
	if l.Schema != 1 || (l.Kind != "app" && l.Kind != "core" && l.Kind != "runner" && l.Kind != "standalone" && l.Kind != "model-store" && l.Kind != "storage-probe") {
		return errors.New("invalid managed component kind/schema")
	}
	if !filepath.IsAbs(l.Directory) || filepath.Clean(l.Directory) != l.Directory || l.Directory == "/" || strings.ContainsAny(l.Directory, "\x00\r\n") {
		return errors.New("managed invocation needs an absolute private directory")
	}
	if len(l.Args) == 0 || len(l.Args) > 256 || len(l.Env) > 256 {
		return errors.New("invalid managed argv/environment count")
	}
	if l.Kind == "storage-probe" {
		if !filepath.IsAbs(l.ProbeDirectory) || filepath.Clean(l.ProbeDirectory) != l.ProbeDirectory || l.ProbeDirectory == "/" || strings.ContainsRune(l.ProbeDirectory, 0) || l.ProbeIno == 0 {
			return errors.New("invalid managed storage directory identity")
		}
	} else if l.ProbeDirectory != "" || l.ProbeDev != 0 || l.ProbeIno != 0 {
		return errors.New("storage directory fields on non-storage worker")
	}
	for _, value := range l.Args {
		if strings.ContainsRune(value, 0) {
			return errors.New("NUL in managed argv")
		}
	}
	seen := map[string]bool{}
	for _, value := range l.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsAny(value, "\x00") || seen[key] {
			return errors.New("invalid or duplicate managed environment")
		}
		seen[key] = true
	}
	return nil
}

func (c *Client) Launch(ctx context.Context, role Role, launch Launch) (*Invocation, error) {
	if err := launch.Validate(); err != nil {
		return nil, err
	}
	want := map[Role][]string{Core: {"core"}, Runner: {"runner"}, Standalone: {"standalone"}, Worker: {"model-store", "storage-probe"}}
	ok := false
	for _, kind := range want[role] {
		ok = ok || launch.Kind == kind
	}
	if !ok {
		return nil, errors.New("managed role/kind mismatch")
	}
	// Each caller reserves a new private directory; existing directories are
	// never reused after a timeout, and failed delivery is NOT retried.
	if err := os.Mkdir(launch.Directory, 0700); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	id, err := c.Start(ctx, role, payload)
	if err != nil {
		return nil, fmt.Errorf("managed create unresolved; retain invocation directory, do not retry: %w", err)
	}
	return &Invocation{client: c, Instance: id, Directory: launch.Directory}, nil
}

func (p *Invocation) Snapshot(ctx context.Context) (Status, error) {
	return p.client.Status(ctx, p.Instance)
}

// RequestStop cancels restart intent. The native entry watches its own desired
// state and requests cooperative component shutdown; this is not an abort ACK.
func (p *Invocation) RequestStop(ctx context.Context) error {
	return p.client.RequestStop(ctx, p.Instance)
}
func (p *Invocation) Abort(ctx context.Context) error { return p.client.AbortTree(ctx, p.Instance) }

func (p *Invocation) Wait(ctx context.Context) (Result, error) {
	status, err := p.client.WaitStopped(ctx, p.Instance)
	result := Result{Status: status}
	if err != nil {
		return result, err
	}
	data, err := os.ReadFile(filepath.Join(p.Directory, "return.json"))
	if err != nil {
		return result, fmt.Errorf("managed exit without a component return receipt: %w", err)
	}
	if len(data) > 4096 {
		return result, errors.New("oversized managed return receipt")
	}
	var receipt ReturnReceipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		return result, err
	}
	if receipt.Session != status.Record.Session || receipt.Instance != p.Instance || receipt.PID != status.Record.PID {
		return result, errors.New("managed return receipt identity mismatch")
	}
	result.Receipt = &receipt
	if receipt.Code != 0 {
		return result, fmt.Errorf("managed component returned %d", receipt.Code)
	}
	return result, nil
}

// Stop keeps normal shutdown separate from emergency abort. Deadline failure
// retains the handle so its owner can explicitly decide whether to abort.
func (p *Invocation) Stop(ctx context.Context) (Result, error) {
	if err := p.RequestStop(ctx); err != nil {
		return Result{}, err
	}
	return p.Wait(ctx)
}

func (p *Invocation) Output(name string, limit int64) ([]byte, error) {
	if name != "stdout" && name != "stderr" {
		return nil, errors.New("invalid managed output stream")
	}
	if limit < 0 || limit > 64*1024*1024 {
		return nil, errors.New("invalid managed output bound")
	}
	f, err := os.Open(filepath.Join(p.Directory, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("managed output exceeds bound or is not regular")
	}
	data := make([]byte, info.Size())
	n, err := f.ReadAt(data, 0)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}

// AwaitRegistration closes the official-create publication race. A child can
// enter Main before the HAP has returned from the official start call.
func (c *Client) AwaitRegistration(ctx context.Context) error {
	for {
		status, err := c.Status(ctx, c.instance)
		if err == nil && status.Record.State == "running" {
			return nil
		}
		if err == nil && status.Record.ExitObserved {
			return errors.New("native entry already retired")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
