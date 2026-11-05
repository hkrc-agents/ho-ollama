//go:build harmonyos

package server

import (
	"bytes"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestServiceGroupWaitDrainsBeforeReturn(t *testing.T) {
	var previous int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0)
	// No product processes: both commands are owned by this test.
	other := exec.Command("/bin/sleep", "20")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	cmd := exec.Command("/bin/sh", "-c", "sleep 20 & echo $!; exit 17")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	err := WaitServiceCommand(cmd)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatalf("exit result %v", err)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(out.String()))
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if os.IsNotExist(e) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			t.Fatal("stat")
		}
		if time.Now().After(deadline) {
			t.Fatal("runner not reaped after wait: " + string(data))
		}
		time.Sleep(time.Millisecond * 10)
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("other service affected", err)
	}
}

func TestServiceGroupWaitRejectsUnownedCommand(t *testing.T) {
	if err := WaitServiceCommand(exec.Command("/bin/true")); !errors.Is(err, ErrServiceTreeNotDrained) {
		t.Fatal(err)
	}
	if platformCoreMayRestart(ErrServiceTreeNotDrained) {
		t.Fatal("unsafe restart")
	}
}
