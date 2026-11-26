package session

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if history := sm.GetHistory(key); len(history) != 0 {
		t.Fatalf("history after the delete = %v, want none", history)
	}
}

// A refused delete keeps the session in memory: the next message and Save add
// to the history instead of writing a new, empty session over the file.
func TestSessionManagerRefusedDeleteKeepsTheSession(t *testing.T) {
	key := "agent:main:direct:lock-delete-refused"
	sm, lock, path := lockedManager(t, key)

	if err := sm.DeleteSession(key); !errors.Is(err, sessionlock.ErrBusy) {
		t.Fatalf("DeleteSession while locked = %v, want ErrBusy", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	sm.AddMessage(key, "user", "two")
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save after the refused delete: %v", err)
	}
	requireSavedMessages(t, path, "one", "two")
}

// Without a file, the delete still forgets the session: with no storage, and
// with storage but nothing saved yet. With no storage it touches no disk.
func TestSessionManagerDeleteWithoutAFileForgetsTheSession(t *testing.T) {
	for name, storage := range map[string]string{"no storage": "", "nothing saved": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			key := "agent:main:direct:delete-unsaved"
			sm := NewSessionManager(storage)
			sm.AddMessage(key, "user", "one")

			if err := sm.DeleteSession(key); err != nil {
				t.Fatalf("DeleteSession = %v, want nil", err)
			}
			if history := sm.GetHistory(key); len(history) != 0 {
				t.Fatalf("history after the delete = %v, want none", history)
			}
			if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
				t.Fatalf("the delete wrote %v in the working folder", entries)
			}
		})
	}
}

// requireSavedMessages fails unless the session's file holds each content.
func requireSavedMessages(t *testing.T, path string, contents ...string) {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range contents {
		if !strings.Contains(string(saved), strconv.Quote(content)) {
			t.Fatalf("session file = %s, want the message %q", saved, content)
		}
	}
}
