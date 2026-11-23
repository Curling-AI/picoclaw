//go:build unix

package memory

// Tests of a session lock lost in the middle of a write, as after a lost NFS
// lease: the lock's own descriptor fails with EIO while writes through new
// descriptors still reach the server, so only a Check on the lock's fd shows
// it. The checkLockForTest seam plays that failure on a local disk.

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// failSessionLockCheck makes the nth Check of s's session locks fail as a lost
// lease does, and returns the count of Checks s made.
func failSessionLockCheck(t *testing.T, s *JSONLStore, nth int) *int {
	t.Helper()
	calls := 0
	s.checkLockForTest = func(*sessionlock.Lock) error {
		calls++
		if calls == nth {
			return fmt.Errorf("%w: %w", sessionlock.ErrLost, syscall.EIO)
		}
		return nil
	}
	t.Cleanup(func() { s.checkLockForTest = nil })
	return &calls
}

// requireNextHolderOwnsTheSession: once the lost writer released the lock,
// another process's store recovers whatever it left and appends; the history
// and its page are whole. The lost bit belonged to that one hold: the lost
// writer's next hold writes again.
func requireNextHolderOwnsTheSession(t *testing.T, lostWriter *JSONLStore, dir, key string, want ...string) {
	t.Helper()
	next := openTwoStore(t, dir)
	if err := next.AddMessage(t.Context(), key, "user", "four"); err != nil {
		t.Fatalf("the next holder's AddMessage(four) = %v, want nil", err)
	}
	state := readTwoStoreState(t, dir, key)
	page, pageErr := twoStorePage(t, next, key)
	if want = append(want, "four"); !slices.Equal(state.Lines, want) || state.Journal || state.Marker ||
		pageErr != nil || !page.Complete {
		t.Fatalf("after the next holder: %v page complete=%v err=%v; want lines %v, complete, no journal or latch",
			state, page.Complete, pageErr, want)
	}
	if err := lostWriter.AddMessage(t.Context(), key, "user", "five"); err != nil {
		t.Fatalf("the lost writer's next hold: AddMessage(five) = %v, want nil", err)
	}
	want = append(want, "five")
	if got := readTwoStoreState(t, dir, key); !slices.Equal(got.Lines, want) {
		t.Fatalf("after the lost writer's next hold: %v; want lines %v", got, want)
	}
}

// A writer whose lease was lost must not publish, roll back, or retire its journal: the next
// holder owns that recovery.
func TestLostLockAbortsBeforeTheMetaAndUndoesNothing(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-meta"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	before := readTwoStoreState(t, dir, key)
	// Checks 1 (before the journal) and 2 (the end of beginHistoryWrite, before
	// the truncate) pass; the third, before the meta, fails.
	checks := failSessionLockCheck(t, a, 3)

	err := a.AddMessage(t.Context(), key, "user", "three")
	if !errors.Is(err, ErrSessionLockLost) {
		t.Fatalf("AddMessage(three) after the lock was lost = %v, want ErrSessionLockLost", err)
	}
	lost := readTwoStoreState(t, dir, key)
	if lost.MetaCount != 2 || lost.MetaBytes != before.MetaBytes {
		t.Fatalf("meta after the lost write: %v; want the old meta (count 2, %d bytes) unpublished",
			lost, before.MetaBytes)
	}
	if !lost.Journal || lost.Decision || lost.Marker {
		t.Fatalf("after the lost write: %v; want the journal kept, no decision and no latch", lost)
	}
	if !slices.Equal(lost.Lines, []string{"one", "two", "three"}) || lost.JSONLBytes <= before.JSONLBytes {
		t.Fatalf("transcript after the lost write: %v; want three's bytes left in place, not undone", lost)
	}
	if *checks != 3 {
		t.Fatalf("Checks = %d, want 3: the journal's, the truncate's and the meta's, none after the loss", *checks)
	}

	a.checkLockForTest = nil
	requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two")
}

// loseLeaseAfterJournal makes s lose its lease right after it publishes key's
// journal: the lock file goes, as the server forgets s's lock, and other takes
// the lock on a new file, recovers that journal and appends three-B. The
// returned func reports whether that ran and other's error.
func loseLeaseAfterJournal(t *testing.T, s, other *JSONLStore, dir, key string) func() (bool, error) {
	t.Helper()
	journal, played := s.historyRecoveryPath(key), false
	var otherErr error
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if err := fileutil.WriteFileAtomic(path, data, mode); err != nil || path != journal || played {
			return err
		}
		played = true
		if err := os.Remove(sessionlock.Path(dir, sanitizeKey(key))); err != nil {
			return err
		}
		otherErr = other.AddMessage(t.Context(), key, "user", "three-B")
		return nil
	}
	t.Cleanup(func() { s.writeFile = nil })
	return func() (bool, error) { return played, otherErr }
}

// A step of A in flight when A lost its lease must not cut the rows B wrote
// after taking the lock: neither the append's truncate nor the rewrite's
// replace of the transcript runs. A loses its lease right after its journal;
// the Check that ends beginHistoryWrite (the second, after the journal's)
// fails, before either step.
func TestLostLockAbortsBeforeTheTruncateAndKeepsTheNextHoldersRows(t *testing.T) {
	for name, write := range map[string]func(*JSONLStore, string) error{
		"append-truncate": func(a *JSONLStore, key string) error {
			return a.AddMessage(t.Context(), key, "user", "three-A")
		},
		"rewrite-replace": func(a *JSONLStore, key string) error {
			return a.SetHistory(t.Context(), key, []providers.Message{{Role: "user", Content: "one-A"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key := "agent:main:direct:lost-" + name
			a := openTwoStore(t, dir)
			addTwoStore(t, a, key, "one", "two")
			b := openTwoStore(t, dir)
			checks := failSessionLockCheck(t, a, 2)
			played := loseLeaseAfterJournal(t, a, b, dir, key)

			err := write(a, key)
			if ran, bErr := played(); !ran || bErr != nil {
				t.Fatalf("precondition: B must append after A's journal (ran=%v, B = %v)", ran, bErr)
			}
			if !errors.Is(err, ErrSessionLockLost) || *checks != 2 {
				t.Fatalf("A's write after the lock was lost = %v after %d Checks; want ErrSessionLockLost at the 2nd",
					err, *checks)
			}
			lost := readTwoStoreState(t, dir, key)
			if !slices.Equal(lost.Lines, []string{"one", "two", "three-B"}) || lost.MetaCount != 3 || lost.Journal ||
				lost.Marker {
				t.Fatalf("after A's lost write: %v; want B's row kept, B's meta, no journal and no latch", lost)
			}

			a.writeFile, a.checkLockForTest = nil, nil
			requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two", "three-B")
		})
	}
}
