//go:build harmonyos && pico_native_services

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/ollama/ollama/app/server"
	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

type managedCoreState struct {
	mu         sync.Mutex
	invocation *nativeprocess.Invocation
	instances  map[int]*nativeprocess.Invocation
	bind       string
	pending    string
	dropping   bool
}

var managedCoreBind = regexp.MustCompile(`^time=\S+ level=INFO source=routes\.go:[0-9]+ msg="Listening on ([^ "\r\n]+) \(version [^"\r\n]{1,128}\)"$`)

func (m *managedCoreState) Write(data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range data {
		if ch == '\n' {
			if !m.dropping {
				if match := managedCoreBind.FindStringSubmatch(m.pending); len(match) == 2 {
					m.bind = match[1]
				}
			}
			m.pending = ""
			m.dropping = false
		} else if !m.dropping {
			if len(m.pending) >= 8192 {
				m.pending = ""
				m.dropping = true
			} else {
				m.pending += string(ch)
			}
		}
	}
	return len(data), nil
}

func configureHarmonyCore(c *harmonyCore) *harmonyCore {
	client, parent, err := nativeprocess.Current()
	if err != nil {
		c.run = func(context.Context) error { return err }
		return c
	}
	state := &managedCoreState{instances: map[int]*nativeprocess.Invocation{}}
	c.executable = filepath.Join(os.Getenv("PICO_NATIVE_LIB_DIR"), "libpico_ollama_core.so")
	c.run = func(ctx context.Context) error {
		// Preserve a single Go owner of core recovery, with the existing delay.
		// No new Native core restart loop competes with this owner.
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			env := map[string]string{}
			for _, item := range os.Environ() {
				key, value, _ := strings.Cut(item, "=")
				env[key] = value
			}
			settings, e := c.database.Settings()
			if e != nil {
				return e
			}
			cloud, e := c.database.CloudDisabled()
			if e != nil {
				return e
			}
			if settings.Expose {
				env["OLLAMA_HOST"] = "0.0.0.0"
			}
			if settings.Browser {
				env["OLLAMA_ORIGINS"] = "*"
			}
			if settings.Models != "" {
				env["OLLAMA_MODELS"], e = c.selectedModelRoot()
				if e != nil {
					return e
				}
			}
			if settings.ContextLength > 0 {
				env["OLLAMA_CONTEXT_LENGTH"] = strconv.Itoa(settings.ContextLength)
			}
			env["OLLAMA_NO_CLOUD"] = "0"
			if cloud {
				env["OLLAMA_NO_CLOUD"] = "1"
			}
			values := make([]string, 0, len(env))
			for key, value := range env {
				values = append(values, key+"="+value)
			}
			var nonce [16]byte
			if _, e = rand.Read(nonce[:]); e != nil {
				return e
			}
			directory := filepath.Join(filepath.Dir(parent.Directory), "core-"+hex.EncodeToString(nonce[:]))
			start, cancel := context.WithTimeout(ctx, 15*time.Second)
			p, e := client.Launch(start, nativeprocess.Core, nativeprocess.Launch{Schema: 1, Kind: "core", Args: []string{"ollama", "serve"}, Env: values, Directory: directory})
			cancel()
			if e != nil {
				return e
			} // No ambiguous-create retry.
			check, stop := context.WithTimeout(context.Background(), 5*time.Second)
			status, e := p.Snapshot(check)
			stop()
			if e != nil {
				return e
			}
			state.mu.Lock()
			state.invocation = p
			state.instances[status.Record.PID] = p
			state.bind = ""
			state.pending = ""
			state.dropping = false
			state.mu.Unlock()
			log, e := os.OpenFile(filepath.Join(os.Getenv("HOME"), ".ollama", "logs", "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if e != nil {
				return e
			}
			done := make(chan struct{})
			wait := make(chan error, 1)
			output := make(chan error, 1)
			go func() { output <- p.FollowOutput(context.Background(), done, io.MultiWriter(log, state)) }()
			go func() { _, e := p.Wait(context.Background()); close(done); wait <- e }()
			select {
			case e = <-wait:
			case <-ctx.Done():
				stopCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
				_, e = p.Stop(stopCtx)
				stop()
				if e == nil {
					e = <-wait
				} else {
					log.Close()
					return e
				}
			}
			e = errors.Join(e, <-output, log.Close())
			if ctx.Err() != nil {
				return e
			}
			// Wait includes official tree drain; only now can a new core start.
		}
		return ctx.Err()
	}
	c.observe = func(string) (core.CoreIdentity, error) {
		state.mu.Lock()
		p := state.invocation
		state.mu.Unlock()
		if p == nil {
			return core.CoreIdentity{}, os.ErrNotExist
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, e := p.Snapshot(ctx)
		if e != nil {
			return core.CoreIdentity{}, e
		}
		if status.Record.ExitObserved {
			return core.CoreIdentity{}, core.ErrCoreExited
		}
		return core.CoreIdentity{PID: status.Record.PID, UID: uint32(os.Getuid()), StartTicks: status.Record.StartTicks, Executable: c.executable}, nil
	}
	c.gone = func(id core.CoreIdentity) (bool, error) {
		state.mu.Lock()
		p := state.instances[id.PID]
		state.mu.Unlock()
		if p == nil {
			return false, errors.New("core instance not registered in this APP")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, e := p.Snapshot(ctx)
		return e == nil && status.Drained, e
	}
	c.proof = func(id core.CoreIdentity, address string) string {
		state.mu.Lock()
		p := state.instances[id.PID]
		bound := state.bind
		current := state.invocation
		state.mu.Unlock()
		if p == nil || p != current || bound != address {
			return ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, e := p.Snapshot(ctx)
		if e != nil || status.Record.ExitObserved || status.Record.StartTicks != id.StartTicks {
			return ""
		}
		return "official-instance-fresh-bind-event"
	}
	c.ready = func(ctx context.Context, id core.CoreIdentity, endpoint *url.URL) bool {
		if c.proof(id, endpoint.Host) == "" {
			return false
		}
		u := *endpoint
		u.Path = "/api/version"
		request, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if e != nil {
			return false
		}
		transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, e := client.Do(request)
		if e != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 200 && c.proof(id, endpoint.Host) != ""
	}
	return c
}
