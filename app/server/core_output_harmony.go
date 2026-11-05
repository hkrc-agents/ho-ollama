//go:build harmonyos

package server

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// This hook MUST be installed before cmd.Start, on the actual core command.
// Returning the same non-*os.File writer for both streams makes os/exec create
// one child pipe and copy it here. The existing logfile is only a destination;
// it is never opened or scanned for ownership evidence.
func platformCoreOutput(cmd *exec.Cmd, log io.Writer) (io.Writer, io.Writer) {
	executable, err := filepath.EvalSymlinks(cmd.Path)
	if err == nil {
		executable, err = filepath.Abs(executable)
	}
	parser := &coreBindParser{}
	if err == nil && ollamaServeArgs(cmd.Args) {
		parser.bind = func(address string) { coreCapturePipeBind(cmd, executable, address) }
	}
	output := &coreOutputWriter{writer: io.MultiWriter(log, parser)}
	return output, output
}

type coreOutputWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *coreOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

const coreBindLineLimit = 8192

// Match the pinned slog text event, not a substring in a prompt/error/log dump.
// No timestamps, prompts or other output are retained in the proof map.
var coreBindEvent = regexp.MustCompile(`^time=\S+ level=INFO source=routes\.go:[0-9]+ msg="Listening on ([^ "\r\n]+) \(version [^"\r\n]{1,128}\)"$`)

type coreBindParser struct {
	line     []byte
	dropping bool
	bind     func(string)
}

func (p *coreBindParser) Write(b []byte) (int, error) {
	for _, ch := range b {
		if ch == '\n' {
			if !p.dropping && p.bind != nil {
				if match := coreBindEvent.FindSubmatch(p.line); match != nil {
					if address, ok := coreBindAddress(string(match[1])); ok {
						p.bind(address)
					}
				}
			}
			p.line = p.line[:0]
			p.dropping = false
		} else if !p.dropping {
			if len(p.line) == coreBindLineLimit {
				p.line = p.line[:0]
				p.dropping = true
			} else {
				p.line = append(p.line, ch)
			}
		}
	}
	return len(b), nil
}

func coreBindAddress(address string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(n)), true
}

type coreBindKey struct {
	Identity CoreIdentity
	Address  string
}

var corePipeBinds = struct {
	sync.Mutex
	// Value is an optional, explicitly requested wildcard's loopback alias.
	proofs map[coreBindKey]string
}{proofs: make(map[coreBindKey]string)}

// coreRequestedBind recognizes only explicit literal wildcard OLLAMA_HOST
// settings, with the pinned envconfig.Host default-port rules. Inherited
// defaults, duplicate keys and malformed/remote settings grant no alias.
func coreRequestedBind(env []string) (string, bool) {
	value, count := "", 0
	for _, item := range env {
		if strings.HasPrefix(item, "OLLAMA_HOST=") {
			value = strings.TrimSpace(strings.TrimPrefix(item, "OLLAMA_HOST="))
			count++
		}
	}
	if count != 1 {
		return "", false
	}
	defaultPort := "11434"
	if scheme, rest, ok := strings.Cut(value, "://"); ok {
		if scheme != "http" {
			return "", false
		}
		value, defaultPort = rest, "80"
	}
	if strings.ContainsAny(value, "/?#") {
		return "", false
	}
	address, ok := coreBindAddress(value)
	if !ok {
		address, ok = coreBindAddress(net.JoinHostPort(strings.Trim(value, "[]"), defaultPort))
	}
	if !ok {
		return "", false
	}
	host, _, _ := net.SplitHostPort(address)
	if !net.ParseIP(host).IsUnspecified() {
		return "", false
	}
	return address, true
}

func coreWildcardLoopback(env []string, bound string) (string, bool) {
	requested, ok := coreRequestedBind(env)
	if !ok || requested != bound {
		return "", false
	}
	host, port, _ := net.SplitHostPort(bound)
	loopback := "::1"
	if net.ParseIP(host).To4() != nil {
		loopback = "127.0.0.1"
	}
	return net.JoinHostPort(loopback, port), true
}

func coreCapturePipeBind(cmd *exec.Cmd, executable, address string) {
	// os/exec assigns Process before starting its pipe-copy goroutine. Do not
	// read ProcessState here: Wait updates it concurrently.
	if cmd.Process == nil {
		return
	}
	id, err := CoreProcessIdentity(cmd.Process.Pid)
	if err != nil || id.PPID != os.Getpid() || id.PGID != id.PID || id.Executable != executable || !CoreIdentityMatches(id) {
		return
	}
	canonical, ok := coreBindAddress(address)
	if !ok {
		return
	}
	host, _, _ := net.SplitHostPort(canonical)
	alias := ""
	if net.ParseIP(host).IsUnspecified() {
		// Read the final cmd.Env here: upstream sets it after installing the
		// output hook, but before Start launches this pipe-copy goroutine.
		alias, ok = coreWildcardLoopback(cmd.Env, canonical)
		if !ok {
			return
		}
	}
	corePipeBinds.Lock()
	defer corePipeBinds.Unlock()
	// Bound storage across repeated launches. Eviction can only deny readiness.
	if len(corePipeBinds.proofs) >= 128 {
		for key := range corePipeBinds.proofs {
			delete(corePipeBinds.proofs, key)
			break
		}
	}
	corePipeBinds.proofs[coreBindKey{id, canonical}] = alias
}

func corePipeBindOwned(id CoreIdentity, address string) bool {
	canonical, ok := coreBindAddress(address)
	if !ok || id.PPID != os.Getpid() || id.PGID != id.PID || !CoreIdentityMatches(id) {
		return false
	}
	corePipeBinds.Lock()
	_, found := corePipeBinds.proofs[coreBindKey{id, canonical}]
	if !found {
		for key, alias := range corePipeBinds.proofs {
			if key.Identity == id && alias == canonical {
				found = true
				break
			}
		}
	}
	corePipeBinds.Unlock()
	return found && CoreIdentityMatches(id)
}
