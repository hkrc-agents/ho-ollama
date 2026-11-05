//go:build harmonyos

// Package nativeprocess talks to the HAP's private Native C++ manager. It does
// not spawn anything, call a shell, impersonate os.Process, or modify the public
// Ollama HTTP API. It must be explicitly configured from a native SO bootstrap.
package nativeprocess

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type Role byte

const (
	App Role = iota
	Core
	Runner
	Standalone
	Worker
)

type Config struct {
	Address    string `json:"control_address"`
	Capability string `json:"control_capability"`
	Session    string `json:"hap_session"`
	Instance   uint64 `json:"instance"`
}

type Client struct {
	address    string
	capability []byte
	session    string
	instance   uint64
}

type Record struct {
	Session         string `json:"hap_session"`
	Instance        uint64 `json:"instance"`
	Owner           uint64 `json:"owner"`
	Role            string `json:"role"`
	PID             int    `json:"pid"`
	StartTicks      uint64 `json:"start_ticks"`
	State           string `json:"state"`
	Desired         bool   `json:"desired_running"`
	OfficialCreated bool   `json:"official_created"`
	StopRequested   bool   `json:"stop_requested"`
	ExitObserved    bool   `json:"exit_observed"`
	ExitSignal      int    `json:"exit_signal"` // Signal, NOT an exit code.
	ExitSignalKnown bool   `json:"exit_signal_known"`
	ExitProof       string `json:"exit_proof"`
	Error           string `json:"error"`
}

type Status struct {
	Record    Record `json:"record"`
	Drained   bool   `json:"drained"`
	Quiescent bool   `json:"quiescent"`
}

func New(config Config) (*Client, error) {
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil || host != "127.0.0.1" {
		return nil, errors.New("private native control requires IPv4 loopback")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return nil, errors.New("invalid private native control port")
	}
	capability, err := hex.DecodeString(config.Capability)
	if err != nil || len(capability) != 32 {
		return nil, errors.New("invalid private native capability")
	}
	session, err := hex.DecodeString(config.Session)
	if err != nil || len(session) != 32 || config.Instance == 0 {
		return nil, errors.New("invalid native session/instance")
	}
	if subtle.ConstantTimeCompare(capability, session) == 1 {
		return nil, errors.New("private capability must differ from logged session identifier")
	}
	return &Client{address: config.Address, capability: capability, session: strings.ToLower(config.Session), instance: config.Instance}, nil
}

func (c *Client) call(ctx context.Context, opcode byte, role Role, target uint64, payload []byte, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(payload) > 16384 || strings.ContainsRune(string(payload), '\x00') {
		return errors.New("invalid native control payload")
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", c.address)
	if err != nil {
		return fmt.Errorf("native control connect: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(15 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	header := make([]byte, 60)
	copy(header, "PNS1")
	header[4] = opcode
	header[5] = byte(role)
	binary.BigEndian.PutUint64(header[8:16], c.instance)
	binary.BigEndian.PutUint64(header[16:24], target)
	binary.BigEndian.PutUint32(header[24:28], uint32(len(payload)))
	copy(header[28:60], c.capability)
	request := append(header, payload...)
	for len(request) > 0 {
		n, e := conn.Write(request)
		if e != nil {
			return contextError(ctx, e)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		request = request[n:]
	}
	var response [12]byte
	if _, err = io.ReadFull(conn, response[:]); err != nil {
		return contextError(ctx, err)
	}
	if string(response[:4]) != "PNR1" {
		return errors.New("invalid native control response")
	}
	code, size := binary.BigEndian.Uint32(response[4:8]), binary.BigEndian.Uint32(response[8:12])
	if size > 65536 {
		return errors.New("oversized native control response")
	}
	body := make([]byte, size)
	if _, err = io.ReadFull(conn, body); err != nil {
		return contextError(ctx, err)
	}
	if !json.Valid(body) {
		return errors.New("invalid native control response JSON")
	}
	if code != 0 {
		// Never include bootstrap/payload/capability in diagnostics.
		return fmt.Errorf("native control rejected operation %d: status %d", opcode, code)
	}
	if output != nil {
		return json.Unmarshal(body, output)
	}
	return nil
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Client) Start(ctx context.Context, role Role, payload json.RawMessage) (uint64, error) {
	if role == App || role > Worker || !json.Valid(payload) {
		return 0, errors.New("invalid native service request")
	}
	var response struct {
		OK       bool   `json:"ok"`
		Instance uint64 `json:"instance"`
	}
	err := c.call(ctx, 1, role, 0, payload, &response)
	if err != nil {
		return 0, err
	}
	if !response.OK || response.Instance == 0 {
		return 0, errors.New("missing native start acknowledgement")
	}
	return response.Instance, nil
}

func (c *Client) Status(ctx context.Context, instance uint64) (Status, error) {
	var status Status
	err := c.call(ctx, 2, 0, instance, nil, &status)
	if err != nil {
		return status, err
	}
	if status.Record.Session != c.session || status.Record.Instance != instance || !status.Record.OfficialCreated {
		return Status{}, errors.New("native instance identity mismatch")
	}
	return status, nil
}

func (c *Client) WaitTree(ctx context.Context, instance uint64) (Status, error) {
	return c.wait(ctx, instance, false)
}

// WaitStopped also covers replacements reserved concurrently with a stop.
// Recovery may wait for one old tree; user Stop/Quit must wait for quiescence.
func (c *Client) WaitStopped(ctx context.Context, instance uint64) (Status, error) {
	return c.wait(ctx, instance, true)
}

func (c *Client) wait(ctx context.Context, instance uint64, allGenerations bool) (Status, error) {
	for {
		status, err := c.Status(ctx, instance)
		if err != nil {
			return status, err
		}
		if status.Drained && (!allGenerations || status.Quiescent) {
			if !status.Record.ExitObserved {
				return status, errors.New("native drain without exit proof")
			}
			return status, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Status{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) CancelIntent(ctx context.Context, instance uint64) error {
	return c.call(ctx, 4, 0, instance, nil, nil)
}

func (c *Client) RequestStop(ctx context.Context, instance uint64) error {
	return c.call(ctx, 6, 0, instance, nil, nil)
}

// AbortTree is forced crash cleanup, NOT normal request drain or graceful Quit.
func (c *Client) AbortTree(ctx context.Context, instance uint64) error {
	return c.call(ctx, 3, 0, instance, nil, nil)
}

func (c *Client) Replace(ctx context.Context, previous uint64) (uint64, error) {
	var response struct {
		OK       bool   `json:"ok"`
		Instance uint64 `json:"instance"`
	}
	if err := c.call(ctx, 5, 0, previous, nil, &response); err != nil {
		return 0, err
	}
	if !response.OK || response.Instance == 0 {
		return 0, errors.New("missing replacement acknowledgement")
	}
	return response.Instance, nil
}
