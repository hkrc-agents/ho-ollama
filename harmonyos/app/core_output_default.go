//go:build !harmonyos

package server

import (
	"io"
	"os/exec"
)

func platformCoreOutput(_ *exec.Cmd, log io.Writer) (io.Writer, io.Writer) {
	return log, log
}
