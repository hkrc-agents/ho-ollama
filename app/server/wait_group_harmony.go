//go:build harmonyos && !pico_native_services

package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var ErrServiceTreeNotDrained = errors.New("service tree not drained; restart forbidden")

// WaitServiceCommand retains the exited group leader as an unreaped child
// while its inherited process group is drained. Its numeric PID/PGID cannot
// be recycled during that interval. No saved PID or foreign UID is adopted.
func WaitServiceCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		return fmt.Errorf("%w: dedicated direct-child group required", ErrServiceTreeNotDrained)
	}
	pid := cmd.Process.Pid
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: observe child %d: %v", ErrServiceTreeNotDrained, pid, err)
		}
		break
	}
	// SIGKILL only after the service has exited; normal request drain remains
	// the existing owner's responsibility before entering this wait.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := unix.Kill(-pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
			return fmt.Errorf("%w: kill group %d: %v", ErrServiceTreeNotDrained, pid, err)
		}
		members, err := serviceGroupMembers(pid)
		if err != nil {
			return fmt.Errorf("%w: enumerate: %v", ErrServiceTreeNotDrained, err)
		}
		pending := false
		for _, member := range members {
			if member.pid == pid {
				continue
			}
			pending = true // A zombie is not a completed reap or an empty group.
			if member.state == "Z" || member.state == "X" {
				// Only reap a proven direct child. Never waitpid(-1), which could
				// steal another manager's worker or the still-pinned leader.
				if member.ppid == os.Getpid() {
					var status unix.WaitStatus
					if _, err := unix.Wait4(member.pid, &status, unix.WNOHANG, nil); err != nil && err != unix.ECHILD && err != unix.ESRCH {
						return fmt.Errorf("%w: reap: %v", ErrServiceTreeNotDrained, err)
					}
				}
			}
		}
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: group %d still live", ErrServiceTreeNotDrained, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd.Wait()
}

type serviceGroupMember struct {
	pid, ppid int
	state     string
}

func serviceGroupMembers(group int) ([]serviceGroupMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []serviceGroupMember
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		path := "/proc/" + entry.Name()
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, errors.New("proc UID unavailable")
		}
		if st.Uid != uint32(os.Getuid()) {
			continue
		}
		// Read PGID from stat, including zombies. getpgid() returning ESRCH
		// is not evidence that no waitable zombie exists in procfs.
		data, err := os.ReadFile(path + "/stat")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return nil, errors.New("malformed proc stat")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			return nil, errors.New("short proc stat")
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		if pgid != group {
			continue
		}
		result = append(result, serviceGroupMember{pid, ppid, fields[0]})
	}
	return result, nil
}

func platformCoreWait(cmd *exec.Cmd) error  { return WaitServiceCommand(cmd) }
func platformCoreMayRestart(err error) bool { return !errors.Is(err, ErrServiceTreeNotDrained) }
