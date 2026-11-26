//go:build unix

package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// failLockChecks makes every session lock's Check fail as after a lost NFS
// lease, through the fsync seam the store's lost-lease tests use. It returns
// what the sessions folder held at each Check: the contents of Save's temp
// files.
func failLockChecks(t *testing.T) *[][]string {
	t.Helper()
	var checks [][]string
	locktest.Sync = func(lockFile *os.File) error {
		dir := filepath.Dir(filepath.Dir(lockFile.Name()))
		temps, _ := filepath.Glob(filepath.Join(dir, "session-*.tmp"))
		contents := make([]string, len(temps))
		for i, temp := range temps {
			got, _ := os.ReadFile(temp)
			contents[i] = string(got)
		}
		checks = append(checks, contents)
		return syscall.EIO
	}
	t.Cleanup(func() { locktest.Sync = nil })
	return &checks
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
// the lock right before its rename, once its temp file holds the new session,
// and on a lost lock leaves the file as it was, removes its temp file and
// fails with the lost lock.
func TestSessionManagerSaveWithALostLockLeavesTheFile(t *testing.T) {
	key := "agent:main:direct:lost-save"
	sm, path, saved := savedManager(t, key)
	sm.AddMessage(key, "assistant", "two")
	checks := failLockChecks(t)

	if err := sm.Save(key); !errors.Is(err, sessionlock.ErrLost) {
		t.Fatalf("Save with a lost lock = %v, want ErrLost", err)
	}
	if len(*checks) != 1 || len((*checks)[0]) != 1 || !strings.Contains((*checks)[0][0], `"two"`) {
		t.Fatalf("temp files at each Check = %q, want one Check with one temp holding the new message", *checks)
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

// A removal that fails keeps the session in memory too: the file is still the
// session's, and the next Save adds to it.
func TestSessionManagerFailedRemovalKeepsTheSession(t *testing.T) {
	key := "agent:main:direct:delete-remove-fails"
	sm, path, _ := savedManager(t, key)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := sm.DeleteSession(key); err == nil {
		t.Fatal("DeleteSession in a read-only folder = nil, want the removal's error")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sm.AddMessage(key, "assistant", "two")
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save after the failed removal: %v", err)
	}
	requireSavedMessages(t, path, "one", "two")
}

// A delete refused by a lost lock keeps the session in memory, as a busy one
// does: the next Save, with the lock back, adds to the history.
func TestSessionManagerDeleteWithALostLockKeepsTheSession(t *testing.T) {
	key := "agent:main:direct:lost-delete-keeps"
	sm, path, _ := savedManager(t, key)
	failLockChecks(t)

	if err := sm.DeleteSession(key); !errors.Is(err, sessionlock.ErrLost) {
		t.Fatalf("DeleteSession with a lost lock = %v, want ErrLost", err)
	}
	locktest.Sync = nil
	sm.AddMessage(key, "assistant", "two")
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save after the lost delete: %v", err)
	}
	requireSavedMessages(t, path, "one", "two")
}
