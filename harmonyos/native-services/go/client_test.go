//go:build harmonyos

package nativeprocess

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type request struct {
	Opcode, Role   byte
	Caller, Target uint64
	Payload        []byte
	Capability     []byte
}

func fixture(t *testing.T, handler func(net.Conn, request)) (*Client, Config) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				var bytes [60]byte
				if _, err := io.ReadFull(conn, bytes[:]); err != nil {
					return
				}
				if string(bytes[:4]) != "PNS1" {
					return
				}
				size := binary.BigEndian.Uint32(bytes[24:28])
				if size > 16384 {
					return
				}
				payload := make([]byte, size)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
				handler(conn, request{bytes[4], bytes[5], binary.BigEndian.Uint64(bytes[8:16]), binary.BigEndian.Uint64(bytes[16:24]), payload, bytes[28:60]})
			}()
		}
	}()
	config := Config{Address: listener.Addr().String(), Capability: strings.Repeat("a", 64), Session: strings.Repeat("b", 64), Instance: 1}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return client, config
}

func respond(conn net.Conn, code uint32, body any) {
	bytes, _ := json.Marshal(body)
	var header [12]byte
	copy(header[:], "PNR1")
	binary.BigEndian.PutUint32(header[4:8], code)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(bytes)))
	// Deliberate fragmentation across protocol header and UTF-8 JSON bytes.
	for _, b := range append(header[:], bytes...) {
		if _, err := conn.Write([]byte{b}); err != nil {
			return
		}
	}
}

func TestBootstrapValidation(t *testing.T) {
	valid := Config{"127.0.0.1:19000", strings.Repeat("a", 64), strings.Repeat("b", 64), 1}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"wildcard":           func(c *Config) { c.Address = "0.0.0.0:19000" },
		"DNS":                func(c *Config) { c.Address = "localhost:19000" },
		"zero_port":          func(c *Config) { c.Address = "127.0.0.1:0" },
		"missing_capability": func(c *Config) { c.Capability = "" },
		"logged_capability":  func(c *Config) { c.Capability = c.Session },
		"missing_session":    func(c *Config) { c.Session = "old" },
		"missing_instance":   func(c *Config) { c.Instance = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			change(&bad)
			if _, err := New(bad); err == nil {
				t.Fatal("invalid bootstrap accepted")
			}
		})
	}
}

func TestStartUsesNativeControlProtocol(t *testing.T) {
	client, _ := fixture(t, func(conn net.Conn, r request) {
		if r.Opcode != 1 || r.Role != byte(Core) || r.Caller != 1 || r.Target != 0 || len(r.Capability) != 32 {
			t.Error("wrong native request")
		}
		if !json.Valid(r.Payload) {
			t.Error("invalid JSON payload")
		}
		respond(conn, 0, map[string]any{"ok": true, "instance": 2})
	})
	id, err := client.Start(context.Background(), Core, json.RawMessage(`{"argv":["serve"],"test":"中文🙂"}`))
	if err != nil || id != 2 {
		t.Fatal(id, err)
	}
}

func TestStartRejectsAPPAndMalformedPayload(t *testing.T) {
	client, _ := fixture(t, func(net.Conn, request) { t.Error("invalid request reached native server") })
	for _, role := range []Role{App, Role(255)} {
		if _, err := client.Start(context.Background(), role, json.RawMessage(`{}`)); err == nil {
			t.Fatal("forbidden role")
		}
	}
	if _, err := client.Start(context.Background(), Core, json.RawMessage(`{bad`)); err == nil {
		t.Fatal("invalid payload")
	}
}

func TestExitCallbackIsNotWholeTreeDrain(t *testing.T) {
	var calls atomic.Int32
	client, config := fixture(t, func(conn net.Conn, r request) {
		n := calls.Add(1)
		respond(conn, 0, Status{Record: Record{Session: strings.Repeat("b", 64), Instance: r.Target, PID: 200, OfficialCreated: true, ExitObserved: true, ExitSignal: 9}, Drained: n >= 2})
	})
	_ = config
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := client.WaitTree(ctx, 2)
	if err != nil || calls.Load() < 2 || !status.Drained || status.Record.ExitSignal != 9 {
		t.Fatal(status, err)
	}
}

func TestStatusRejectsDifferentSessionOrUnregisteredChild(t *testing.T) {
	for _, wrong := range []string{"session", "official", "instance"} {
		t.Run(wrong, func(t *testing.T) {
			client, _ := fixture(t, func(conn net.Conn, r request) {
				record := Record{Session: strings.Repeat("b", 64), Instance: r.Target, OfficialCreated: true}
				switch wrong {
				case "session":
					record.Session = strings.Repeat("c", 64)
				case "official":
					record.OfficialCreated = false
				case "instance":
					record.Instance++
				}
				respond(conn, 0, Status{Record: record})
			})
			if _, err := client.Status(context.Background(), 2); err == nil {
				t.Fatal("wrong identity accepted")
			}
		})
	}
}

func TestContextCancellationInterruptsBlockedRead(t *testing.T) {
	ready := make(chan struct{}, 1)
	client, _ := fixture(t, func(conn net.Conn, r request) { ready <- struct{}{}; var byte [1]byte; conn.Read(byte[:]) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := client.Status(ctx, 2); result <- err }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("request not received")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close blocked native request")
	}
}

func TestNativeErrorDoesNotExposeCapabilityOrPayload(t *testing.T) {
	client, config := fixture(t, func(conn net.Conn, r request) {
		respond(conn, 401, map[string]string{"error": "secret payload should not be logged"})
	})
	_, err := client.Start(context.Background(), Core, json.RawMessage(`{"private":"secret"}`))
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), config.Capability) {
		t.Fatal("unsafe/missing native error")
	}
}

func TestMalformedNativeResponseRejected(t *testing.T) {
	for _, kind := range []string{"magic", "size", "JSON"} {
		t.Run(kind, func(t *testing.T) {
			client, _ := fixture(t, func(conn net.Conn, r request) {
				var header [12]byte
				copy(header[:], "PNR1")
				switch kind {
				case "magic":
					copy(header[:], "WRNG")
				case "size":
					binary.BigEndian.PutUint32(header[8:12], 1000000)
				case "JSON":
					binary.BigEndian.PutUint32(header[8:12], 1)
				}
				conn.Write(header[:])
				if kind == "JSON" {
					conn.Write([]byte("{"))
				}
			})
			if _, err := client.Status(context.Background(), 2); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestDrainedWithoutExitProofRejected(t *testing.T) {
	client, _ := fixture(t, func(conn net.Conn, r request) {
		respond(conn, 0, Status{Record: Record{Session: strings.Repeat("b", 64), Instance: r.Target, OfficialCreated: true}, Drained: true})
	})
	if _, err := client.WaitTree(context.Background(), 2); err == nil {
		t.Fatal("missing exit proof accepted")
	}
}

func TestStopWaitIncludesConcurrentReplacement(t *testing.T) {
	var calls atomic.Int32
	client, _ := fixture(t, func(conn net.Conn, r request) {
		n := calls.Add(1)
		respond(conn, 0, Status{Record: Record{Session: strings.Repeat("b", 64), Instance: r.Target, OfficialCreated: true, ExitObserved: true}, Drained: true, Quiescent: n >= 2})
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.WaitStopped(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 2 {
		t.Fatal("stop ignored a still-live replacement")
	}
}
