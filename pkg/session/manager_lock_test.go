package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func lockedManager(t *testing.T, key string) (*SessionManager, *sessionlock.Lock, string) {
	t.Helper()
	old := sessionManagerLockWait
	sessionManagerLockWait = 50 * time.Millisecond
	t.Cleanup(func() { sessionManagerLockWait = old })

	dir := t.TempDir()
	sm := NewSessionManager(dir)
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "one")

	lock, err := sessionlock.Acquire(dir, sanitizeFilename(key), 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	return sm, lock, filepath.Join(dir, sanitizeFilename(key)+".json")
}

func TestSessionManagerSaveWaitsForTheSessionLock(t *testing.T) {
	key := "agent:main:direct:lock-save"
	sm, lock, path := lockedManager(t, key)

	if err := sm.Save(key); !errors.Is(err, sessionlock.ErrBusy) {
		t.Fatalf("Save while locked = %v, want ErrBusy", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("session file written without the lock: %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save after release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session file missing after release: %v", err)
	}
}

func TestSessionManagerDeleteWaitsForTheSessionLock(t *testing.T) {
	key := "agent:main:direct:lock-delete"
	sm, lock, path := lockedManager(t, key)
	sm.AddFullMessage(key, providers.Message{Role: "user", Content: "two"})
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sm.DeleteSession(key); !errors.Is(err, sessionlock.ErrBusy) {
		t.Fatalf("DeleteSession while locked = %v, want ErrBusy", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file removed without the lock: %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := sm.DeleteSession(key); err != nil {
		t.Fatalf("DeleteSession after release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there after delete: %v", err)
	}
}
