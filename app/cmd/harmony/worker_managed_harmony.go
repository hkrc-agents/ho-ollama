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
	"strings"
	"time"

	nativeprocess "github.com/ollama/ollama/harmonyos/nativeprocess"
)

func harmonyManagedInspect(ctx context.Context, root, name string) (harmonyModelFile, bool, error) {
	var result harmonyModelFile
	client, parent, err := nativeprocess.Current()
	if err != nil {
		return result, true, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return result, true, err
	}
	directory := filepath.Join(filepath.Dir(parent.Directory), "inspect-"+hex.EncodeToString(nonce[:]))
	p, err := client.Launch(ctx, nativeprocess.Worker, nativeprocess.Launch{Schema: 1, Kind: "model-store", Args: []string{"model-store", root, name}, Env: os.Environ(), Directory: directory})
	if err != nil {
		return result, true, err
	}
	_, err = p.Wait(ctx)
	if ctx.Err() != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		err = errors.Join(err, p.Abort(cleanup))
		_, drain := p.Wait(cleanup)
		err = errors.Join(err, drain)
	}
	if err != nil {
		return result, true, err
	}
	data, err := p.Output("stdout", 65536)
	if err != nil {
		return result, true, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, true, err
	}
	if _, err = hexDigest(strings.TrimPrefix(result.Digest, "sha256:")); err != nil {
		return result, true, err
	}
	expected := filepath.Join(root, "blobs", strings.ReplaceAll(result.Digest, ":", "-"))
	if result.Path != expected || result.Size < 24 {
		return result, true, errors.New("invalid managed model descriptor")
	}
	return result, true, nil
}
