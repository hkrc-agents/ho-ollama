//go:build harmonyos

package manifest

import (
	"bytes"
	"github.com/ollama/ollama/types/model"
	"os"
	"testing"
)

func TestHarmonyManifestCopyLifecycle(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	name := model.ParseName("example")
	path, err := V2PathForName(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte(`{"schemaVersion":2}`), []byte(`{"schemaVersion":2,"layers":[]}`)} {
		digest, err := writeManifestBlob(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := linkManifest(name, digest); err != nil {
			t.Fatal(err)
		}
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("not a regular manifest: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("content mismatch: %v", err)
		}
		resolved, err := ResolvePathForName(name)
		if err != nil || resolved != path {
			t.Fatalf("resolve: %q %v", resolved, err)
		}
		blob, err := BlobsPath(digest)
		if err != nil {
			t.Fatal(err)
		}
		// Ordinary-file copy must remain readable without the source blob.
		if err := os.Remove(blob); err != nil {
			t.Fatal(err)
		}
		got, err = os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("copy depends on blob: %v", err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePathForName(name); !os.IsNotExist(err) {
		t.Fatalf("after removal: %v", err)
	}
}
