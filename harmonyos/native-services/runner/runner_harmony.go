//go:build harmonyos && pico_native_services

package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

// managedRunner is deliberately not an exec.Cmd: native children are owned by
// the HAP registry, not waitpid or a Go os.Process. Results retain unknown exits.
type managedRunner struct {
	invocation *nativeprocess.Invocation
	pid        int
	done       chan struct{}
	mu         sync.Mutex
	err        error
	exited     bool
}

func findManagedLlamaServer() (string, error) {
	if _, _, err := nativeprocess.Current(); err != nil {
		return "", err
	}
	root := os.Getenv("PICO_NATIVE_LIB_DIR")
	if !filepath.IsAbs(root) {
		return "", errors.New("installed native library root missing")
	}
	path := filepath.Join(root, "libpico_llama_server.so")
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("installed server SO is not regular")
	}
	return path, nil
}

func launchManagedRunner(exe string, args []string, gpuLibs []string, extra map[string]string, out io.Writer) (*managedRunner, error) {
	client, parent, err := nativeprocess.Current()
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	// This exec.Cmd is only an upstream environment-formatting container. It
	// is never started; all actual creation goes through nativeprocess.Launch.
	template := exec.Command(exe, args...)
	SetupLlamaServerCommandEnv(template, exe, gpuLibs, extra)
	dir := filepath.Join(filepath.Dir(parent.Directory), "runner-"+hex.EncodeToString(nonce[:]))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invocation, err := client.Launch(ctx, nativeprocess.Runner, nativeprocess.Launch{Schema: 1, Kind: "runner", Args: append([]string{"llama-server"}, args...), Env: template.Env, Directory: dir})
	if err != nil {
		return nil, err
	}
	status, err := invocation.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	result := &managedRunner{invocation: invocation, pid: status.Record.PID, done: make(chan struct{})}
	exit := make(chan struct{})
	outputDone := make(chan error, 1)
	if out == nil {
		out = io.Discard
	}
	go func() {
		e := invocation.FollowOutput(context.Background(), exit, out)
		if e != nil {
			check, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			e = errors.Join(e, invocation.Abort(check))
		}
		outputDone <- e
	}()
	go func() {
		_, e := invocation.Wait(context.Background())
		close(exit)
		e = errors.Join(e, <-outputDone)
		result.mu.Lock()
		result.err = e
		result.exited = true
		result.mu.Unlock()
		close(result.done)
	}()
	return result, nil
}

func (p *managedRunner) PID() int {
	if p == nil {
		return 0
	}
	return p.pid
}
func (p *managedRunner) Exited() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}
func (p *managedRunner) Description() string {
	if p == nil {
		return "not created"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exited {
		return "running"
	}
	if p.err != nil {
		return p.err.Error()
	}
	return "SO returned 0 with receipt and official exit proof"
}
func (p *managedRunner) Wait() error {
	if p == nil {
		return errors.New("nil managed runner")
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
func (p *managedRunner) Stop() error {
	if p == nil {
		return nil
	}
	if p.Exited() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.invocation.RequestStop(ctx); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("managed runner stop incomplete: %w", ctx.Err())
	}
}
