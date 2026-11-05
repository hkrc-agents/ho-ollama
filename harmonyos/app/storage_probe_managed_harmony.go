//go:build harmonyos && pico_native_services

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
	"golang.org/x/sys/unix"
)

func harmonyManagedStorageProbe(ctx context.Context, fd int, parent unix.Stat_t, domain string) (*storageNativeResult, string, bool, error) {
	client, launch, err := nativeprocess.Current()
	if err != nil {
		return nil, "", true, err
	}
	path, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, "", true, err
	}
	if !filepath.IsAbs(path) || strings.HasSuffix(path, " (deleted)") {
		return nil, "", true, errors.New("managed probe directory has no stable reopen path")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, "", true, err
	}
	directory := filepath.Join(filepath.Dir(launch.Directory), "storage-"+hex.EncodeToString(nonce[:]))
	p, err := client.Launch(ctx, nativeprocess.Worker, nativeprocess.Launch{Schema: 1, Kind: "storage-probe", Args: []string{"storage-probe"}, Env: os.Environ(), Directory: directory,
		ProbeDirectory: path, ProbeDev: uint64(parent.Dev), ProbeIno: parent.Ino})
	if err != nil {
		return nil, "", true, err
	}
	status, err := p.Snapshot(ctx)
	if err != nil {
		return nil, "", true, err
	}
	_, err = p.Wait(ctx)
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = errors.Join(err, p.Abort(cleanup))
		_, drain := p.Wait(cleanup)
		err = errors.Join(err, drain)
	}
	stderr, stderrErr := p.Output("stderr", 65536)
	if err != nil {
		return nil, string(stderr), true, errors.Join(err, stderrErr)
	}
	data, err := p.Output("stdout", 65536)
	if err != nil {
		return nil, string(stderr), true, err
	}
	var result storageNativeResult
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, string(stderr), true, err
	}
	// Siblings created by appspawn do not have the Go APP's kernel PPID. The
	// official registry instance is the ownership proof instead. Reopening is
	// tested honestly as reopening, never reported as inherited-FD coverage.
	if result.Schema != 1 || result.PID != status.Record.PID || !status.Record.OfficialCreated || result.UID != os.Getuid() || result.Domain == "" || result.Domain != domain || result.ParentDev != uint64(parent.Dev) || result.ParentIno != parent.Ino {
		return &result, string(stderr), true, errors.New("managed storage actor/directory identity mismatch")
	}
	reopened := false
	for _, op := range result.Operations {
		if op.Name == "reopened_directory" {
			reopened = true
		}
		if op.Name == "inherited_directory" {
			return &result, string(stderr), true, errors.New("managed storage probe incorrectly claimed FD inheritance")
		}
	}
	if !reopened {
		return &result, string(stderr), true, errors.New("missing managed directory reopen proof")
	}
	return &result, string(stderr), true, errors.Join(storageEvidence(result.Operations), stderrErr)
}
