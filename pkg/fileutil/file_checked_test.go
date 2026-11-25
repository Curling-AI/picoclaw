package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// requireOnlyFile fails unless dir holds path alone, with want: no temp file.
func requireOnlyFile(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", filepath.Base(path), got, err, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries, want %s alone", len(entries), filepath.Base(path))
	}
}

// The check runs once, after the new content is on disk in the temp file and
// before path shows it.
func TestWriteFileAtomicCheckedRunsTheCheckRightBeforeTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := WriteFileAtomicChecked(path, []byte("new"), 0o600, func() error {
		calls++
		if got, _ := os.ReadFile(path); string(got) != "old" {
			t.Errorf("at the check, path = %q; want the old content", got)
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 2 {
			t.Errorf("at the check, dir holds %d entries; want path and the temp file", len(entries))
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("WriteFileAtomicChecked = %v after %d checks; want nil after 1", err, calls)
	}
	requireOnlyFile(t, path, "new")
}

// A refused check leaves path as it was, removes the temp file, returns the
// check's error, and never falls back to the direct write, even when a second
// check would pass.
func TestWriteFileAtomicCheckedRefusedLeavesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("lock lost")
	calls := 0
	err := WriteFileAtomicChecked(path, []byte("new"), 0o600, func() error {
		if calls++; calls == 1 {
			return lost
		}
		return nil
	})
	if !errors.Is(err, lost) || calls != 1 {
		t.Fatalf("WriteFileAtomicChecked = %v after %d checks; want the check's error after 1", err, calls)
	}
	requireOnlyFile(t, path, "old")
}
