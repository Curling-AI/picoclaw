//go:build unix

package memory

// A journal create that published its journal and then failed (the temporary
// name would not go) removes that journal, and only that one, while the lock is
// still proven: a holder that lost its lease, or a name that now holds another
// writer's journal, leaves it to the recovery that needs it.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
)

// failJournalTempRemoval makes s's next journal create link its journal and
// then fail to remove the temporary name: a non-empty folder takes that name.
// afterLink runs right after, with the journal's path and the file linked there.
func failJournalTempRemoval(t *testing.T, s *JSONLStore, afterLink func(path string, linked os.FileInfo)) {
	t.Helper()
	fired := false
	s.linkHistoryJournal = func(tmp, path string) error {
		if fired {
			return os.Link(tmp, path)
		}
		fired = true
		if err := os.Link(tmp, path); err != nil {
			return err
		}
		linked, err := os.Lstat(path)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = os.RemoveAll(tmp) })
		if err = os.Remove(tmp); err != nil {
			return err
		}
		if err = os.Mkdir(tmp, 0o700); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(tmp, "keep"), nil, 0o600); err != nil {
			return err
		}
		afterLink(path, linked)
		return nil
	}
	t.Cleanup(func() { s.linkHistoryJournal = nil })
}

func TestJournalAbortRemovesOnlyItsOwnJournalUnderTheLock(t *testing.T) {
	t.Run("another-writers-journal-replaced-it", func(t *testing.T) {
		dir, key := t.TempDir(), "agent:main:direct:abort-foreign"
		a := openTwoStore(t, dir)
		addTwoStore(t, a, key, "one", "two")
		other := []byte(`{"writer":"the other pod"}`)
		failJournalTempRemoval(t, a, func(path string, _ os.FileInfo) {
			// The other pod recovered this journal and published its own.
			if err := os.WriteFile(path+".other", other, 0o600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(path+".other", path); err != nil {
				t.Error(err)
			}
		})

		err := a.AddMessage(t.Context(), key, "user", "three")
		if !errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, ErrSessionLockLost) {
			t.Fatalf("AddMessage(three) = %v; want the temporary name's removal error", err)
		}
		if got, readErr := os.ReadFile(a.historyRecoveryPath(key)); readErr != nil || !bytes.Equal(got, other) {
			t.Fatalf("the other writer's journal = %q, %v; want it kept byte for byte: %q", got, readErr, other)
		}
	})

	t.Run("lease-lost-at-the-abort", func(t *testing.T) {
		dir, key := t.TempDir(), "agent:main:direct:abort-lost"
		a := openTwoStore(t, dir)
		addTwoStore(t, a, key, "one", "two")
		var ours os.FileInfo
		failJournalTempRemoval(t, a, func(_ string, linked os.FileInfo) {
			ours = linked
			locktest.Sync = func(*os.File) error { return syscall.EIO }
		})
		t.Cleanup(func() { locktest.Sync = nil })

		err := a.AddMessage(t.Context(), key, "user", "three")
		if !errors.Is(err, ErrSessionLockLost) || !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatalf("AddMessage(three) = %v; want the removal error joined with ErrSessionLockLost", err)
		}
		if current, statErr := os.Lstat(a.historyRecoveryPath(key)); statErr != nil || !os.SameFile(ours, current) {
			t.Fatalf("after the lost abort the journal is %v, %v; want this write's journal kept", current, statErr)
		}

		locktest.Sync = nil
		requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two")
	})

	t.Run("still-ours-with-the-lease-held", func(t *testing.T) {
		dir, key := t.TempDir(), "agent:main:direct:abort-ours"
		a := openTwoStore(t, dir)
		addTwoStore(t, a, key, "one", "two")
		before := readTwoStoreState(t, dir, key)
		failJournalTempRemoval(t, a, func(string, os.FileInfo) {})

		err := a.AddMessage(t.Context(), key, "user", "three")
		if !errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, ErrSessionLockLost) {
			t.Fatalf("AddMessage(three) = %v; want the temporary name's removal error", err)
		}
		if state := readTwoStoreState(t, dir, key); state.String() != before.String() {
			t.Fatalf("after the abort: %v; want the session as before, without the journal: %v", state, before)
		}
		addTwoStore(t, a, key, "three")
		if state := readTwoStoreState(t, dir, key); state.String() == before.String() ||
			!twoStoreHas(state.Lines, "three") || state.Journal {
			t.Fatalf("after the next write: %v; want three written", state)
		}
	})

	// Nothing published, nothing to remove: the abort returns the create's
	// error and spends no Check on the lock file.
	t.Run("nothing-published", func(t *testing.T) {
		dir, key := t.TempDir(), "agent:main:direct:abort-nothing"
		a := openTwoStore(t, dir)
		addTwoStore(t, a, key, "one", "two")
		a.linkHistoryJournal = func(tmp, path string) error {
			return &os.LinkError{Op: "link", Old: tmp, New: path, Err: syscall.EIO}
		}
		t.Cleanup(func() { a.linkHistoryJournal = nil })
		checks := failSessionLockCheck(t, a, 0)

		err := a.AddMessage(t.Context(), key, "user", "three")
		if !errors.Is(err, syscall.EIO) || *checks != 1 {
			t.Fatalf("AddMessage(three) = %v after %d Checks; want the link's EIO after only the "+
				"Check before the journal", err, *checks)
		}
		if readTwoStoreState(t, dir, key).Journal {
			t.Fatal("a journal was left at its name")
		}
	})
}

// Without link(2) the journal is created with O_EXCL, and a write that fails
// after that create cannot be made on a local disk: the create's identity goes
// to the abort directly, under the lock.
func TestJournalAbortRemovesOnlyItsOwnExclusiveJournal(t *testing.T) {
	for name, replaced := range map[string]bool{"still-ours": false, "another-writers-journal-replaced-it": true} {
		t.Run(name, func(t *testing.T) {
			dir, key := t.TempDir(), "agent:main:direct:abort-exclusive"
			a := openTwoStore(t, dir)
			addTwoStore(t, a, key, "one", "two")
			unlock, err := a.lockSession(key)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			path := a.historyRecoveryPath(key)
			published, err := createHistoryJournalExclusive(path, []byte("{}"))
			if err != nil {
				t.Fatal(err)
			}
			other := []byte(`{"writer":"the other pod"}`)
			if replaced {
				if err = os.WriteFile(path+".other", other, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path+".other", path); err != nil {
					t.Fatal(err)
				}
			}

			failed := errors.New("the journal's write failed")
			if err = a.abortHistoryJournal(key, published, failed); err != failed {
				t.Fatalf("abortHistoryJournal = %v; want the create's error alone", err)
			}
			got, readErr := os.ReadFile(path)
			switch {
			case replaced && (readErr != nil || !bytes.Equal(got, other)):
				t.Fatalf("the other writer's journal = %q, %v; want it kept byte for byte: %q", got, readErr, other)
			case !replaced && !errors.Is(readErr, os.ErrNotExist):
				t.Fatalf("this create's journal = %q, %v; want it removed", got, readErr)
			}
		})
	}
}
