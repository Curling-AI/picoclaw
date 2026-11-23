//go:build unix

package memory

// Every destructive step of the store past the write path proves the lock
// first: recovery, the recovery latch, deletions and prunes. Each test makes
// the Check of that step fail as after a lost NFS lease (checkLockForTest) and
// asserts the step did not happen.

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/memory/internal/locktest"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// failMetaPublishOnce makes s's next meta publish of key fail with EIO, so the
// write goes to its recovery.
func failMetaPublishOnce(t *testing.T, s *JSONLStore, key string) {
	t.Helper()
	meta, fired := s.metaPath(key), false
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == meta && !fired {
			fired = true
			return syscall.EIO
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	t.Cleanup(func() { s.writeFile = nil })
}

func readSessionFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The recovery of a failed append proves the lock before it starts (no
// decision is recorded) and again before it applies the decision (the OLD
// rollback does not cut the row). Checks 1 to 3 are the write's own.
func TestLostLockStopsTheRecoveryOfAFailedWrite(t *testing.T) {
	for name, c := range map[string]struct {
		nth      int
		decision bool
	}{
		"before-reading-the-journal": {nth: 4, decision: false},
		"before-the-old-rollback":    {nth: 5, decision: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key := "agent:main:direct:lost-recovery-" + name
			a := openTwoStore(t, dir)
			addTwoStore(t, a, key, "one", "two")
			before := readTwoStoreState(t, dir, key)
			failMetaPublishOnce(t, a, key)
			checks := failSessionLockCheck(t, a, c.nth)

			err := a.AddMessage(t.Context(), key, "user", "three")
			if !errors.Is(err, ErrSessionLockLost) {
				t.Fatalf("AddMessage(three) = %v, want ErrSessionLockLost from its recovery", err)
			}
			lost := readTwoStoreState(t, dir, key)
			if !slices.Equal(lost.Lines, []string{"one", "two", "three"}) || lost.MetaCount != 2 ||
				lost.MetaBytes != before.MetaBytes || !lost.Journal || lost.Decision != c.decision || lost.Marker {
				t.Fatalf("after the lost recovery: %v; want three's row kept, the old meta, the journal, decision=%v",
					lost, c.decision)
			}
			if *checks != c.nth {
				t.Fatalf("Checks = %d, want %d: none after the loss", *checks, c.nth)
			}

			a.writeFile, a.checkLockForTest = nil, nil
			requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two")
		})
	}
}

// The recovery latch has no reset: a holder that lost its lease does not set
// it. A complete row past the meta's checkpoint makes the append latch first.
func TestLostLockDoesNotSetTheRecoveryLatch(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-latch"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	appendRawHistory(t, a, key, []byte(durableExternalRow))
	before := readSessionFile(t, a.jsonlPath(key))
	checks := failSessionLockCheck(t, a, 1)

	err := a.AddMessage(t.Context(), key, "user", "three")
	if !errors.Is(err, ErrSessionLockLost) || *checks != 1 {
		t.Fatalf("AddMessage(three) = %v after %d Checks, want ErrSessionLockLost at the first", err, *checks)
	}
	lost := readTwoStoreState(t, dir, key)
	if lost.Marker || lost.Journal || lost.MetaCount != 2 ||
		!bytes.Equal(readSessionFile(t, a.jsonlPath(key)), before) {
		t.Fatalf("after the lost write: %v; want no latch, no journal, the old meta and the transcript untouched", lost)
	}

	a.checkLockForTest = nil
	next := openTwoStore(t, dir)
	if err := next.AddMessage(t.Context(), key, "user", "four"); err != nil {
		t.Fatalf("the next holder's AddMessage(four) = %v, want nil", err)
	}
	if got := readTwoStoreState(t, dir, key); !got.Marker || !twoStoreHas(got.Lines, "four") {
		t.Fatalf("after the next holder: %v; want its own latch and its row", got)
	}
}

// requireSessionKept: a lost deletion or prune removed nothing of the session,
// not even its lock file.
func requireSessionKept(t *testing.T, s *JSONLStore, dir, key string, lines ...string) {
	t.Helper()
	state := readTwoStoreState(t, dir, key)
	if !slices.Equal(state.Lines, lines) || !twoStoreExists(s.metaPath(key)) {
		t.Fatalf("after the lost removal: %v; want lines %v and the meta kept", state, lines)
	}
	if !twoStoreExists(sessionlock.Path(dir, sanitizeKey(key))) {
		t.Fatal("the lost removal removed the session's lock file")
	}
}

// An owner's delete proves the lock once before its removals. Check 1 is the
// delete's recovery of a pending journal.
func TestLostLockStopsAnOwnersDelete(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		dir := t.TempDir()
		key := "agent:main:direct:lost-delete"
		a := openTwoStore(t, dir)
		addTwoStore(t, a, key, "one", "two")
		checks := failSessionLockCheck(t, a, 2)

		if err := a.DeleteSession(t.Context(), key); !errors.Is(err, ErrSessionLockLost) || *checks != 2 {
			t.Fatalf("DeleteSession = %v after %d Checks, want ErrSessionLockLost at the second", err, *checks)
		}
		requireSessionKept(t, a, dir, key, "one", "two")
	})
	t.Run("public-artifacts-of-an-absent-source", func(t *testing.T) {
		dir := t.TempDir()
		key := "agent:main:direct:lost-delete-absent"
		a := openTwoStore(t, dir)
		projector := &publicCleanupFixture{}
		a.SetPublicHistoryProjector(projector)
		checks := failSessionLockCheck(t, a, 2)

		if err := a.DeleteSession(t.Context(), key); !errors.Is(err, ErrSessionLockLost) || *checks != 2 {
			t.Fatalf("DeleteSession = %v after %d Checks, want ErrSessionLockLost at the second", err, *checks)
		}
		if len(projector.called) != 0 {
			t.Fatalf("the lost delete removed public artifacts of %v", projector.called)
		}
	})
}

// A deletion intent proves the lock after its validation, before any unlink.
// Checks 1 to 3 are the delete's recovery, the intent's creation and the
// recovery that applies it.
func TestLostLockStopsADeletionIntent(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-intent"
	a := openTwoStore(t, dir)
	projector := &publicCleanupFixture{}
	a.SetPublicHistoryProjector(projector)
	addTwoStore(t, a, key, "one", "two")
	checks := failSessionLockCheck(t, a, 4)

	if err := a.DeleteSession(t.Context(), key); !errors.Is(err, ErrSessionLockLost) || *checks != 4 {
		t.Fatalf("DeleteSession = %v after %d Checks, want ErrSessionLockLost at the fourth", err, *checks)
	}
	requireSessionKept(t, a, dir, key, "one", "two")
	if !twoStoreExists(a.historyRecoveryPath(key)) || len(projector.called) != 0 {
		t.Fatalf("after the lost intent: journal kept=%v, public cleanups %v; want the intent kept for the next holder",
			twoStoreExists(a.historyRecoveryPath(key)), projector.called)
	}

	next := openTwoStore(t, dir)
	next.SetPublicHistoryProjector(&publicCleanupFixture{})
	if err := next.DeleteSession(t.Context(), key); err != nil {
		t.Fatalf("the next holder's DeleteSession = %v, want nil", err)
	}
	if twoStoreExists(next.metaPath(key)) || twoStoreExists(next.jsonlPath(key)) ||
		twoStoreExists(next.historyRecoveryPath(key)) {
		t.Fatal("the next holder did not finish the deletion")
	}
}

// A prune proves the lock once before its removals.
func TestLostLockStopsAPrune(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-prune"
	a := openTwoStore(t, dir)
	if err := a.UpsertSessionMeta(t.Context(), key, nil, nil); err != nil {
		t.Fatal(err)
	}
	checks := failSessionLockCheck(t, a, 1)

	removed, err := a.PruneEmptySessions(time.Minute, time.Now().Add(time.Hour), nil)
	if !errors.Is(err, ErrSessionLockLost) || len(removed) != 0 || *checks != 1 {
		t.Fatalf("PruneEmptySessions = %v, %v after %d Checks; want ErrSessionLockLost at the first, nothing removed",
			removed, err, *checks)
	}
	requireSessionKept(t, a, dir, key)
}

// The store runs sessionlock's real Check: its fsync failing, as after a lost
// lease, comes back from the write as ErrSessionLockLost wrapping EIO, before
// the meta, and nothing is undone. Checks 1 and 2 are the journal's and the end
// of beginHistoryWrite's; the third, the meta's, fails.
func TestFailedFsyncOfTheRealCheckStopsTheWriteBeforeItsMeta(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-fsync"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	syncs := 0
	locktest.Sync = func(f *os.File) error {
		if syncs++; syncs == 3 {
			return syscall.EIO
		}
		return f.Sync()
	}
	t.Cleanup(func() { locktest.Sync = nil })

	err := a.AddMessage(t.Context(), key, "user", "three")
	if !errors.Is(err, ErrSessionLockLost) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("AddMessage(three) = %v, want ErrSessionLockLost wrapping EIO", err)
	}
	lost := readTwoStoreState(t, dir, key)
	if lost.MetaCount != 2 || !lost.Journal || lost.Decision || !twoStoreHas(lost.Lines, "three") || syncs != 3 {
		t.Fatalf("after the failed fsync: %v after %d syncs; want the old meta, the journal, three kept, 3 syncs",
			lost, syncs)
	}

	locktest.Sync = nil
	requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two")
}

// The prune's hook runs only under a proven lock: the caller's hook removes
// what it holds of the session (ethos drops its conversation record).
func TestLostLockStopsAPruneBeforeItsHook(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-prune-hook"
	a := openTwoStore(t, dir)
	if err := a.UpsertSessionMeta(t.Context(), key, nil, nil); err != nil {
		t.Fatal(err)
	}
	failSessionLockCheck(t, a, 1)
	hooked := 0

	removed, err := a.PruneEmptySessions(time.Minute, time.Now().Add(time.Hour), func(SessionMeta) error {
		hooked++
		return nil
	})
	if !errors.Is(err, ErrSessionLockLost) || len(removed) != 0 || hooked != 0 {
		t.Fatalf("PruneEmptySessions = %v, %v with %d hook calls; want ErrSessionLockLost before the hook",
			removed, err, hooked)
	}
	requireSessionKept(t, a, dir, key)
}

// captureLog sends the standard logger to the returned buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// The cron prune drops a run it could not remove from its count, and logs it:
// a run whose lock was lost shows up once per sweep, by its age or by the cap.
func TestPruneCronRunsLogsTheRunsItCouldNotRemove(t *testing.T) {
	run := func(n int) string { return fmt.Sprintf("agent_cron-job-%08x-0000-4000-8000-000000000000", n) }
	for name, c := range map[string]struct {
		runs int
		age  time.Duration
	}{
		"age": {runs: 1, age: 30 * 24 * time.Hour},
		"cap": {runs: maxCronRunsPerJob + 1, age: cronRunSettleAge + time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTwoStore(t, dir)
			for i := range c.runs {
				writeSession(t, dir, run(i), c.age+time.Duration(i)*time.Minute)
			}
			s.checkLockForTest = func(*sessionlock.Lock) error { return sessionlock.ErrLost }
			logged := captureLog(t)

			removed, err := s.PruneCronRuns(14*24*time.Hour, time.Now())
			if err != nil || removed != 0 {
				t.Fatalf("PruneCronRuns = %d, %v; want nothing removed", removed, err)
			}
			if !strings.Contains(logged.String(), ErrSessionLockLost.Error()) ||
				!strings.Contains(logged.String(), run(c.runs-1)) {
				t.Fatalf("log = %q; want the run it could not remove and why", logged.String())
			}
		})
	}
}
