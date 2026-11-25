//go:build unix

package session

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// failLockChecks makes every session lock's Check fail as after a lost NFS
// lease, through the fsync seam the store's lost-lease tests use.
func failLockChecks(t *testing.T) {
	t.Helper()
	locktest.Sync = func(*os.File) error { return syscall.EIO }
	t.Cleanup(func() { locktest.Sync = nil })
}

// savedManager saves key's session once, with the real Check, and returns its
// file and what the file holds.
func savedManager(t *testing.T, key string) (*SessionManager, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	sm := NewSessionManager(dir)
	sm.AddMessage(key, "user", "one")
	if err := sm.Save(key); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	path := filepath.Join(dir, sanitizeFilename(key)+".json")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sm, path, saved
}

// requireOnlyTheSession fails unless dir holds the session's file and the
// .locks folder alone: no temp file is left.
func requireOnlyTheSession(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != filepath.Base(path) && name != sessionlock.Dir {
			t.Fatalf("%s left beside the session's file", name)
		}
	}
}

// After a lost lease another process may hold the session's lock: Save checks
// the lock right before its rename, and on a lost lock leaves the file as it
// was, removes its temp file and fails with the lost lock.
func TestSessionManagerSaveWithALostLockLeavesTheFile(t *testing.T) {
	key := "agent:main:direct:lost-save"
	sm, path, saved := savedManager(t, key)
	sm.AddMessage(key, "assistant", "two")
	failLockChecks(t)

	if err := sm.Save(key); !errors.Is(err, sessionlock.ErrLost) {
		t.Fatalf("Save with a lost lock = %v, want ErrLost", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(saved) {
		t.Fatalf("session file after the lost Save = %q, %v; want the saved one", got, err)
	}
	requireOnlyTheSession(t, path)
}

// A delete is the same publish: on a lost lock the file stays.
func TestSessionManagerDeleteWithALostLockLeavesTheFile(t *testing.T) {
	key := "agent:main:direct:lost-delete"
	sm, path, saved := savedManager(t, key)
	failLockChecks(t)

	if err := sm.DeleteSession(key); !errors.Is(err, sessionlock.ErrLost) {
		t.Fatalf("DeleteSession with a lost lock = %v, want ErrLost", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(saved) {
		t.Fatalf("session file after the lost delete = %q, %v; want it there", got, err)
	}
}
