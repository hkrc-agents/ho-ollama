//go:build harmonyos

package nativeprocess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type Bootstrap struct {
	Schema int `json:"schema"`
	Config
	Owner   uint64 `json:"owner"`
	Role    string `json:"role"`
	Payload string `json:"payload"`
}

func DecodeBootstrap(text string) (Bootstrap, Launch, *Client, error) {
	var b Bootstrap
	var launch Launch
	if len(text) > 65536 {
		return b, launch, nil, errors.New("oversized SO bootstrap")
	}
	decode := func(text string, out any) error {
		d := json.NewDecoder(strings.NewReader(text))
		d.DisallowUnknownFields()
		if err := d.Decode(out); err != nil {
			return err
		}
		if d.Decode(new(any)) != io.EOF {
			return errors.New("trailing SO configuration")
		}
		return nil
	}
	if err := decode(text, &b); err != nil {
		return b, launch, nil, err
	}
	if b.Schema != 1 {
		return b, launch, nil, errors.New("unsupported SO bootstrap schema")
	}
	if err := decode(b.Payload, &launch); err != nil {
		return b, launch, nil, err
	}
	if err := launch.Validate(); err != nil {
		return b, launch, nil, err
	}
	want := map[string]string{"app": "app", "core": "core", "runner": "runner", "standalone": "standalone", "model-store": "worker", "storage-probe": "worker"}
	if want[launch.Kind] != b.Role {
		return b, launch, nil, errors.New("SO role/kind mismatch")
	}
	client, err := New(b.Config)
	return b, launch, client, err
}

// ConfigureEnvironment is called once in the dedicated official child before
// starting the component. Go's own environ must be updated explicitly; C
// setenv before dlopen alone does not fix a runtime reconstructed from procfs.
func (l Launch) ConfigureEnvironment() error {
	if err := l.Validate(); err != nil {
		return err
	}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if err := os.Unsetenv(key); err != nil {
			return err
		}
	}
	for _, value := range l.Env {
		key, val, _ := strings.Cut(value, "=")
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	os.Args = append([]string(nil), l.Args...)
	return nil
}

// WatchIntent propagates the HAP registry's cancellation to the component's
// cooperative shutdown context. Transport errors do NOT prove parent death.
func (c *Client) WatchIntent(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, time.Second)
				status, err := c.Status(check, c.instance)
				stop()
				if err == nil && (status.Record.StopRequested || status.Record.ExitObserved) {
					return
				}
			}
		}
	}()
	return ctx, cancel
}
