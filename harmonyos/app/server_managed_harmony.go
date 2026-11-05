//go:build harmonyos && pico_native_services

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

type managedStandalone struct {
	s         *harmonyServer
	mu        sync.Mutex
	current   *nativeprocess.Invocation
	instances map[int]*nativeprocess.Invocation
}

type managedServerIntent struct {
	Session string       `json:"hap_session"`
	Desired bool         `json:"desired_running"`
	Config  serverConfig `json:"config"`
}

func (s *harmonyServer) persistManagedIntent(desired bool, c serverConfig) error {
	client, _, err := nativeprocess.Current()
	if err != nil {
		return err
	}
	return atomicPrivateJSON(filepath.Join(s.directory, "llama-server-desired-native.json"), managedServerIntent{Session: client.Session(), Desired: desired, Config: c})
}

func harmonyRestoreStandalone(ctx context.Context, s *harmonyServer) error {
	client, _, err := nativeprocess.Current()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(s.directory, "llama-server-desired-native.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var intent managedServerIntent
	if err = json.Unmarshal(data, &intent); err != nil {
		return err
	}
	// Only desired configuration within THIS live HAP session is restored.
	// Never adopt PIDs, restore an old HAP session, or implement boot recovery.
	if intent.Session != client.Session() || !intent.Desired {
		return nil
	}
	return s.start(ctx, intent.Config)
}

func configureHarmonyServer(s *harmonyServer) {
	b := &managedStandalone{s: s, instances: map[int]*nativeprocess.Invocation{}}
	s.backend = b
	s.observe = func(id *serverIdentity) (bool, error) {
		if id == nil {
			return false, nil
		}
		b.mu.Lock()
		p := b.instances[id.PID]
		b.mu.Unlock()
		if p == nil {
			return false, errors.New("standalone instance is not owned by this HAP session")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, err := p.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		if strconv.FormatUint(status.Record.StartTicks, 10) != id.StartTicks {
			return false, errors.New("managed server generation mismatch")
		}
		return !status.Drained, nil
	}
}

func (b *managedStandalone) Start(ctx context.Context, c serverConfig) error {
	s := b.s
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateServerConfig(c, s.executable); err != nil {
		return err
	}
	if c.MemoryMiB > 1536 {
		return errors.New("managed worker memory budget must not exceed 1536 MiB")
	}
	live, err := s.observe(s.snapshot().Identity)
	if err != nil {
		return err
	}
	if live {
		if s.snapshot().State == "ready" && s.snapshot().Config == c {
			return nil
		}
		return errors.New("stop managed server before replacing")
	}
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		default:
			return errors.New("previous managed server still draining")
		}
	}
	client, parent, err := nativeprocess.Current()
	if err != nil {
		return err
	}
	address := fmt.Sprintf("127.0.0.1:%d", c.Port)
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return err
	}
	listener.Close()
	if err = atomicPrivateJSON(filepath.Join(s.directory, "llama-server-config.json"), c); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	directory := filepath.Join(filepath.Dir(parent.Directory), "standalone-"+hex.EncodeToString(nonce[:]))
	args := []string{"llama-server", "--model", c.Model, "--host", "127.0.0.1", "--port", strconv.Itoa(c.Port), "--ctx-size", strconv.Itoa(c.Context), "--parallel", "1", "--threads", strconv.Itoa(c.Threads), "--n-gpu-layers", "0"}
	start, cancel := context.WithTimeout(ctx, 90*time.Second)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
	p, err := client.Launch(start, nativeprocess.Standalone, nativeprocess.Launch{Schema: 1, Kind: "standalone", Args: args, Env: os.Environ(), Directory: directory, MemoryMiB: c.MemoryMiB})
	if err != nil {
		return err
	}
	status, err := p.Snapshot(start)
	if err != nil {
		return err
	}
	id := serverIdentity{PID: status.Record.PID, UID: os.Getuid(), StartTicks: strconv.FormatUint(status.Record.StartTicks, 10), Executable: s.executable}
	b.mu.Lock()
	b.current = p
	b.instances[id.PID] = p
	b.mu.Unlock()
	log, err := os.OpenFile(filepath.Join(s.directory, "llama-server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	done = make(chan struct{})
	exited := make(chan struct{})
	output := make(chan error, 1)
	bind := &serverBindReceipt{port: c.Port}
	s.mu.Lock()
	s.done = done
	s.waitErr = nil
	s.current.Config = c
	s.current.Identity = &id
	s.current.State = "starting"
	s.current.Error = ""
	s.current.ReadinessProof = ""
	s.mu.Unlock()
	go func() { output <- p.FollowOutput(context.Background(), exited, io.MultiWriter(log, bind)) }()
	go func() {
		_, e := p.Wait(context.Background())
		close(exited)
		e = errors.Join(e, <-output, log.Close())
		s.mu.Lock()
		s.waitErr = e
		if s.current.State != "stopping" && s.current.State != "draining" {
			s.current.State = "failed"
			s.current.Error = "managed server exited"
			if e != nil {
				s.current.Error = e.Error()
			}
		}
		s.mu.Unlock()
		close(done)
	}()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		bind.mu.Lock()
		bound := bind.sawBind
		bind.mu.Unlock()
		if bound {
			r, e := http.NewRequestWithContext(start, "GET", "http://"+address+"/health", nil)
			if e == nil {
				response, e := httpClient.Do(r)
				if e == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
						fresh, e := p.Snapshot(start)
						if e == nil && !fresh.Record.ExitObserved && fresh.Record.StartTicks == status.Record.StartTicks {
							s.mu.Lock()
							select {
							case <-done:
								s.mu.Unlock()
								return errors.New("managed server exited before ready")
							default:
							}
							if e := s.persistManagedIntent(true, c); e != nil {
								s.mu.Unlock()
								return s.failedStart(e)
							}
							s.current.State = "ready"
							s.current.ReadinessProof = "official-instance-fresh-bind-event+http"
							s.mu.Unlock()
							return nil
						}
					}
				}
			}
		}
		select {
		case <-start.Done():
			return s.failedStart(start.Err())
		case <-done:
			return errors.New("managed server exited during startup")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *managedStandalone) Stop() error {
	s := b.s
	if err := s.persistManagedIntent(false, s.snapshot().Config); err != nil {
		return err
	}
	b.mu.Lock()
	p := b.current
	b.mu.Unlock()
	if p == nil {
		s.setState("stopped", "")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	status, err := p.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !status.Drained {
		s.setState("draining", "")
		_, err = p.Stop(ctx)
		if err != nil {
			s.setState("failed", err.Error())
			return err
		}
	}
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.current.State = "stopped"
	s.current.Error = ""
	s.current.Identity = nil
	s.current.ReadinessProof = ""
	s.done = nil
	s.mu.Unlock()
	return nil
}
