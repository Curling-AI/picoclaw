package fstools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileTempInTargetDir locks the EXDEV fix: the atomic-write temp file
// must be created in the target's directory (a subdir can be a separate mount
// — e.g. artifacts/ as its own volume — and rename cannot cross devices).
// We can't create a real cross-device mount in a unit test, so instead we
// assert no stray .tmp-* ever appears in the sandbox ROOT while writing into
// a subdirectory. (seucaranguejo fork)
func TestWriteFileTempInTargetDir(t *testing.T) {
	root := t.TempDir()
	fsys := buildFs(root, true, nil)
	if err := fsys.WriteFile(
		filepath.Join(root, "artifacts", "site", "index.html"),
		[]byte("<html></html>"),
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "artifacts", "site", "index.html"))
	if err != nil || string(b) != "<html></html>" {
		t.Fatalf("content mismatch: %v %q", err, b)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[:5] == ".tmp-" {
			t.Errorf("temp file leaked in sandbox root: %s (must be created in the target dir)", e.Name())
		}
	}
}
