package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFileDirect is the non-atomic fallback used when the atomic temp+rename
// path fails on S3-backed filesystems (Mountpoint). Verify it writes content
// and creates missing parent directories.
func TestWriteFileDirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")

	if err := writeFileDirect(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeFileDirect: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}
