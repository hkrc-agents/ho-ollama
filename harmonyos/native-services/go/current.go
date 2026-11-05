//go:build harmonyos

package nativeprocess

import (
	"errors"
	"sync"
)

var current struct {
	sync.Mutex
	client *Client
	launch Launch
}

// InstallCurrent is called once by a dedicated SO's exported entry, never by
// a public CLI request or package init. There is no environment auto-detection.
func InstallCurrent(client *Client, launch Launch) error {
	if client == nil {
		return errors.New("nil native control client")
	}
	if err := launch.Validate(); err != nil {
		return err
	}
	current.Lock()
	defer current.Unlock()
	if current.client != nil {
		return errors.New("managed SO entry already configured")
	}
	launch.Args = append([]string(nil), launch.Args...)
	launch.Env = append([]string(nil), launch.Env...)
	current.client = client
	current.launch = launch
	return nil
}

func Current() (*Client, Launch, error) {
	current.Lock()
	defer current.Unlock()
	if current.client == nil {
		return nil, Launch{}, errors.New("official native SO bootstrap is required; exec fallback disabled")
	}
	launch := current.launch
	launch.Args = append([]string(nil), launch.Args...)
	launch.Env = append([]string(nil), launch.Env...)
	return current.client, launch, nil
}

// Public audit identity only; deliberately does not expose the capability.
func(c *Client)Session()string{return c.session}
