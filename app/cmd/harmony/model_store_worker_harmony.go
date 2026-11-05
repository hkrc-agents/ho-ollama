//go:build harmonyos

package main

// One-shot metadata worker: scopes OLLAMA_MODELS to its process rather than
// mutating the HTTP server's global environment while requests are running.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollama/ollama/fs/gguf"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

type harmonyModelFile struct {
	Model    string `json:"model"`
	Manifest string `json:"manifest"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	Path     string `json:"path,omitempty"`
}

func harmonyInspectModel(name string) (harmonyModelFile, error) {
	var result harmonyModelFile
	n := model.ParseName(name)
	if !n.IsValid() {
		return result, errors.New("invalid model name")
	}
	m, err := manifest.ParseNamedManifest(n)
	if err != nil {
		return result, err
	}
	result.Model, result.Manifest = n.String(), m.Digest()
	if m.SelectedDigest() != "" {
		result.Manifest = m.SelectedDigest()
	}
	for _, layer := range m.Layers {
		switch layer.MediaType {
		case "application/vnd.ollama.image.model":
			if result.Digest != "" {
				return result, errors.New("split/multiple weight files cannot be exported as one GGUF")
			}
			if !strings.HasPrefix(layer.Digest, "sha256:") {
				return result, errors.New("unsupported weight digest")
			}
			if _, err := hexDigest(strings.TrimPrefix(layer.Digest, "sha256:")); err != nil {
				return result, err
			}
			result.Digest, result.Size = layer.Digest, layer.Size
		case "application/vnd.ollama.image.template", "application/vnd.ollama.image.system", "application/vnd.ollama.image.params", "application/vnd.ollama.image.messages", "application/vnd.ollama.image.license":
			// Raw GGUF export intentionally excludes these Ollama-specific layers.
		default:
			return result, fmt.Errorf("single-GGUF export cannot represent layer type %s", layer.MediaType)
		}
	}
	if result.Digest == "" {
		return result, errors.New("model has no GGUF weight file")
	}
	result.Path, err = manifest.BlobsPath(result.Digest)
	if err != nil {
		return result, err
	}
	info, err := os.Lstat(result.Path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() != result.Size {
		return result, errors.New("weight file is not a regular file of the declared size")
	}
	metadata, err := gguf.ReadFileMetadata(result.Path, 1024)
	if err != nil {
		return result, err
	}
	if count, ok := metadata.UintOK("split.count"); ok && count > 1 {
		return result, errors.New("split GGUF requires a multi-file export, not a single weights file")
	}
	return result, nil
}

func harmonyStoreWorker(args []string, output io.Writer) error {
	if len(args) != 2 || !filepath.IsAbs(args[0]) {
		return errors.New("worker requires an absolute model root and model name")
	}
	info, err := os.Stat(args[0])
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("model root is not a directory")
	}
	if err := os.Setenv("OLLAMA_MODELS", args[0]); err != nil {
		return err
	}
	result, err := harmonyInspectModel(args[1])
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func harmonyRunInspect(ctx context.Context, root, name string) (harmonyModelFile, error) {
	if result, handled, err := harmonyManagedInspect(ctx, root, name); handled {
		return result, err
	}
	var result harmonyModelFile
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--model-store-worker", root, name)
	// All parameters are argv entries, never shell commands. No inheritance of
	// APP authentication cookies, nor arbitrary user-supplied executable path.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	if err = cmd.Start(); err != nil {
		return result, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, 65537))
	if readErr != nil || len(data) > 65536 {
		cmd.Process.Kill()
	}
	err = errors.Join(readErr, cmd.Wait())
	if err != nil {
		var details struct {
			Error string `json:"error"`
		}
		if len(data) < 65536 && json.Unmarshal(data, &details) == nil && details.Error != "" {
			return result, errors.New(details.Error)
		}
		return result, fmt.Errorf("model inspection failed: %w", err)
	}
	if len(data) > 65536 {
		return result, errors.New("model descriptor exceeds limit")
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if _, err = hexDigest(strings.TrimPrefix(result.Digest, "sha256:")); err != nil {
		return result, err
	}
	expected := filepath.Join(root, "blobs", strings.ReplaceAll(result.Digest, ":", "-"))
	if result.Path != expected || result.Size < 24 {
		return result, errors.New("invalid worker descriptor")
	}
	return result, nil
}
