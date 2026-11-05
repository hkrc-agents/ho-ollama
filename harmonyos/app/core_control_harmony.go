//go:build harmonyos

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	core "github.com/ollama/ollama/app/server"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/envconfig"
)

type coreSnapshot struct {
	ModelRoot      string `json:"model_root,omitempty"`
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
	PID            int    `json:"pid,omitempty"`
	StartTicks     uint64 `json:"start_ticks,omitempty"`
	Executable     string `json:"executable,omitempty"`
	UID            uint32 `json:"uid"`
	PGID           int    `json:"pgid,omitempty"`
	ReadinessProof string `json:"readiness_proof,omitempty"`
	Restarts       int    `json:"restarts"`
}

// One wrapper owns the upstream core supervisor, not the APP HTTP server. The
// Start context is the service lifetime context. Stop serializes with Start;
// Snapshot remains available during either operation. No independent server
// state, model loading, or request proxying belongs here.
type harmonyCore struct {
	op           sync.Mutex
	mu           sync.Mutex
	snapshot     coreSnapshot
	database     *store.Store
	cancel       context.CancelFunc
	done         chan struct{}
	pidPath      string
	executable   string
	identity     core.CoreIdentity
	startTimeout time.Duration
	stopTimeout  time.Duration
	poll         time.Duration
	healthPoll   time.Duration
	run          func(context.Context) error
	// Narrow seams permit lifecycle tests without starting a model/server.
	observe func(string) (core.CoreIdentity, error)
	ready   func(context.Context, core.CoreIdentity, *url.URL) bool
	// Only confirmed exit allows the upstream loop to replace its own child.
	gone  func(core.CoreIdentity) (bool, error)
	proof func(core.CoreIdentity, string) string
}

func newHarmonyCore(database *store.Store) *harmonyCore {
	c := &harmonyCore{database: database, snapshot: coreSnapshot{State: "stopped"},
		pidPath:      filepath.Join(os.Getenv("HOME"), ".ollama", "desktop-server.pid"),
		startTimeout: 20 * time.Second, stopTimeout: 8 * time.Second, poll: 100 * time.Millisecond, healthPoll: 2 * time.Second}
	c.run = func(ctx context.Context) error { return core.New(database, false).Run(ctx) }
	c.observe = c.observeChild
	c.ready = coreHTTPReady
	c.gone = func(id core.CoreIdentity) (bool, error) {
		live, err := serverIdentityStatLive(&serverIdentity{PID: id.PID, StartTicks: strconv.FormatUint(id.StartTicks, 10)})
		return !live, err
	}
	c.proof = core.CoreListenerProof
	return configureHarmonyCore(c)
}

func (c *harmonyCore) Snapshot() coreSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}

func (c *harmonyCore) setState(state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot.State = state
	if state != "ready" {
		c.snapshot.ReadinessProof = ""
	}
	c.snapshot.Error = ""
	if err != nil {
		c.snapshot.Error = err.Error()
	}
}

func (c *harmonyCore) Start(ctx context.Context) error {
	c.op.Lock()
	defer c.op.Unlock()
	return c.startLocked(ctx)
}

func (c *harmonyCore) startLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.done != nil {
		select {
		case <-c.done:
			c.done = nil
		default:
			if c.Snapshot().State == "ready" {
				return nil
			}
			return errors.New("core still active; stop it before retrying")
		}
	}
	c.setState("starting", nil)
	modelRoot := ""
	if c.database != nil {
		var err error
		modelRoot, err = c.selectedModelRoot()
		if err != nil {
			c.setState("failed", err)
			return err
		}
	}
	bind, endpoint, err := c.endpoint()
	if err != nil {
		c.setState("failed", err)
		return err
	}
	if err = os.MkdirAll(filepath.Dir(c.pidPath), 0700); err != nil {
		c.setState("failed", err)
		return err
	}
	lock, err := os.OpenFile(c.pidPath+".control.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		if lock != nil {
			lock.Close()
		}
		c.setState("failed", err)
		return err
	}
	// A preflight is not a reservation: readiness also verifies the listener's
	// inode against the child fd table, so a later bind race cannot pass ready.
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		lock.Close()
		err = fmt.Errorf("core port unavailable: %w", err)
		c.setState("failed", err)
		return err
	}
	listener.Close()
	if c.executable == "" {
		c.executable, err = exec.LookPath("ollama")
		if err == nil {
			c.executable, err = filepath.EvalSymlinks(c.executable)
		}
		if err == nil {
			c.executable, err = filepath.Abs(c.executable)
		}
		if err != nil {
			lock.Close()
			c.setState("failed", err)
			return err
		}
	}
	c.identity = core.CoreIdentity{}
	c.mu.Lock()
	c.snapshot = coreSnapshot{State: "starting", ModelRoot: modelRoot}
	c.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.done = make(chan struct{})
	runDone := make(chan error, 1)
	go func() { runDone <- c.run(runCtx) }()
	go c.watch(runCtx, endpoint, runDone, c.done, lock)
	deadline := time.NewTimer(c.startTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			err = c.snapshotError()
			if err == nil {
				err = errors.New("core exited before ready")
			}
			return err
		case <-ctx.Done():
			err = ctx.Err()
		case <-deadline.C:
			err = errors.New("core readiness timed out (owned listener required)")
		case <-ticker.C:
			s := c.Snapshot()
			if s.State == "ready" {
				return nil
			}
			if s.State != "failed" {
				continue
			}
			err = errors.New(s.Error)
		}
		c.setState("failed", err)
		cancel()
		_ = c.waitStopped()
		return err
	}
}

func (c *harmonyCore) snapshotError() error {
	s := c.Snapshot()
	if s.Error != "" {
		return errors.New(s.Error)
	}
	return nil
}

func (c *harmonyCore) endpoint() (string, *url.URL, error) {
	u := envconfig.ConnectableHost()
	bind := envconfig.Host().Host
	if c.database != nil {
		settings, err := c.database.Settings()
		if err != nil {
			return "", nil, err
		}
		// Upstream Server.cmd sets OLLAMA_HOST=0.0.0.0 (without a port)
		// when Expose is selected, overriding even a custom environment port.
		if settings.Expose {
			bind = "0.0.0.0:11434"
			u = &url.URL{Scheme: "http", Host: "127.0.0.1:11434"}
		}
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, parseErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || n < 1 || n > 65535 || u.Scheme != "http" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || u.Path != "" {
		return "", nil, errors.New("core requires a local HTTP endpoint with a nonzero port")
	}
	return bind, u, nil
}

func (c *harmonyCore) observeChild(executable string) (core.CoreIdentity, error) {
	b, err := os.ReadFile(c.pidPath)
	if err != nil {
		return core.CoreIdentity{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return core.CoreIdentity{}, err
	}
	id, err := core.CoreProcessIdentity(pid)
	if err != nil {
		return id, err
	}
	if id.PPID != os.Getpid() || id.PGID != pid || id.Executable != executable {
		return id, errors.New("core PID file does not name our child")
	}
	if id != c.identity {
		return core.CoreRecordChild(c.pidPath+".identity.json", pid, executable)
	}
	return id, nil
}

func coreHTTPReady(ctx context.Context, id core.CoreIdentity, endpoint *url.URL) bool {
	if !core.CoreListenerOwned(id, endpoint.Host) {
		return false
	}
	u := *endpoint
	u.Path = "/api/version"
	request, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return false
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK && core.CoreListenerOwned(id, endpoint.Host)
}

func (c *harmonyCore) watch(ctx context.Context, endpoint *url.URL, runDone <-chan error, done chan struct{}, lock *os.File) {
	defer close(done)
	defer lock.Close()
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	interval := c.poll
	for {
		select {
		case err := <-runDone:
			c.mu.Lock()
			c.snapshot.ReadinessProof = ""
			if c.snapshot.State != "failed" {
				if ctx.Err() != nil {
					c.snapshot.State = "stopped"
					c.snapshot.Error = ""
				} else {
					c.snapshot.State = "failed"
					c.snapshot.Error = "core supervisor exited"
					if err != nil {
						c.snapshot.Error = err.Error()
					}
				}
			}
			c.mu.Unlock()
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				continue
			}
			id, err := c.observe(c.executable)
			if err != nil {
				if c.identity.PID > 0 {
					if errors.Is(err, os.ErrNotExist) || errors.Is(err, core.ErrCoreExited) {
						if gone, e := c.gone(c.identity); e == nil && gone {
							c.setState("starting", nil)
							continue // Upstream Run owns the delayed restart.
						}
					}
					c.setState("failed", fmt.Errorf("core instance lost: %w", err))
					c.cancel()
				}
				continue
			}
			if c.identity.PID > 0 && id != c.identity {
				gone, e := c.gone(c.identity)
				if e != nil || !gone {
					c.setState("failed", errors.New("core instance replaced without confirmed exit"))
					c.cancel()
					continue
				}
				c.mu.Lock()
				c.snapshot.Restarts++
				c.mu.Unlock()
			}
			c.identity = id
			ready := c.ready(ctx, id, endpoint)
			next := c.poll
			if ready && c.healthPoll > 0 {
				next = c.healthPoll
			}
			if next != interval {
				ticker.Reset(next)
				interval = next
			}
			proof := ""
			if ready {
				if c.proof != nil {
					proof = c.proof(id, endpoint.Host)
				} else {
					proof = core.CoreListenerProof(id, endpoint.Host)
				}
			}
			c.mu.Lock()
			if c.snapshot.State == "starting" || c.snapshot.State == "ready" {
				c.snapshot.PID, c.snapshot.StartTicks, c.snapshot.Executable = id.PID, id.StartTicks, id.Executable
				c.snapshot.UID, c.snapshot.PGID = id.UID, id.PGID
				if ready {
					c.snapshot.State = "ready"
					c.snapshot.Error = ""
					c.snapshot.ReadinessProof = ""
					if proof != "" {
						c.snapshot.ReadinessProof = proof + "+http"
					}
				} else {
					c.snapshot.State = "starting"
					c.snapshot.ReadinessProof = ""
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *harmonyCore) waitStopped() error {
	timer := time.NewTimer(c.stopTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		return nil
	case <-timer.C:
		return errors.New("core stop timed out; supervisor still active")
	}
}

func (c *harmonyCore) Stop() error {
	c.op.Lock()
	defer c.op.Unlock()
	return c.stopLocked()
}

// Restart holds the operation lock across both phases; callers should use it
// instead of independently invoking Stop and Start.
func (c *harmonyCore) Restart(ctx context.Context) error {
	c.op.Lock()
	defer c.op.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.stopLocked(); err != nil {
		return err
	}
	return c.startLocked(ctx)
}

func (c *harmonyCore) stopLocked() error {
	if c.done == nil {
		c.setState("stopped", nil)
		return nil
	}
	select {
	case <-c.done:
		c.setState("stopped", nil)
		return nil
	default:
	}
	// This marks the cancellation boundary; upstream currently closes active
	// HTTP requests on SIGINT, rather than guaranteeing request completion.
	c.setState("draining", nil)
	c.cancel()
	c.setState("stopping", nil)
	if err := c.waitStopped(); err != nil {
		c.setState("failed", err)
		return err
	}
	c.setState("stopped", nil)
	return nil
}
