//go:build unix

package memory

// Tests of the per-session file lock in the store's write path. Two
// JSONLStore instances, or a sessionlock held by the test, stand in for the
// two pods of one assistant sharing sessions/: flock(2) conflicts between two
// open file descriptions even inside one process.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

func withSessionLockWait(t *testing.T, wait time.Duration) {
	t.Helper()
	previous := sessionLockWait
	sessionLockWait = wait
	t.Cleanup(func() { sessionLockWait = previous })
}

func withSessionLockStrict(t *testing.T, strict bool) {
	t.Helper()
	previous := sessionLockStrict
	sessionLockStrict = strict
	t.Cleanup(func() { sessionLockStrict = previous })
}

func withSessionLockClock(t *testing.T, now func() time.Time) {
	t.Helper()
	previous := sessionLockNow
	sessionLockNow = now
	t.Cleanup(func() { sessionLockNow = previous })
}

// holdForeignSessionLock takes the session's lock the way another process does.
func holdForeignSessionLock(t *testing.T, dir, key string) *sessionlock.Lock {
	t.Helper()
	lock, err := sessionlock.Acquire(dir, sanitizeKey(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	return lock
}

func requireNoSessionFiles(t *testing.T, s *JSONLStore, key string) {
	t.Helper()
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
		if twoStoreExists(path) {
			t.Fatalf("%s was written without the session lock", filepath.Base(path))
		}
	}
}

// A write against a lock another process holds waits, gives up, and leaves the
// disk as it was.
func TestWriteGivesUpOnASessionAnotherStoreHolds(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:busy"
	s := openTwoStore(t, dir)
	withSessionLockWait(t, 50*time.Millisecond)
	holdForeignSessionLock(t, dir, key)

	err := s.AddMessage(t.Context(), key, "user", "one")
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("AddMessage under another holder's lock = %v, want ErrSessionBusy", err)
	}
	if !strings.Contains(err.Error(), sanitizeKey(key)) {
		t.Fatalf("ErrSessionBusy does not name the session: %v", err)
	}
	requireNoSessionFiles(t, s, key)
}

// After a write timed out on a session, writes to that session fail at once for
// sessionBusyBackoff instead of queueing for the full wait again; other
// sessions are unaffected, and after the backoff a write waits for the lock again.
func TestBusySessionFailsFastDuringTheBackoff(t *testing.T) {
	dir := t.TempDir()
	key, other := "agent:main:direct:backoff", "agent:main:direct:backoff-other"
	s := openTwoStore(t, dir)
	withSessionLockWait(t, 50*time.Millisecond)
	now := time.Now()
	withSessionLockClock(t, func() time.Time { return now })
	foreign := holdForeignSessionLock(t, dir, key)

	if err := s.AddMessage(t.Context(), key, "user", "one"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("first write = %v, want ErrSessionBusy after the wait", err)
	}
	if err := foreign.Release(); err != nil {
		t.Fatal(err)
	}
	// The lock is free now: only the backoff can refuse this write.
	if err := s.AddMessage(t.Context(), key, "user", "two"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("write inside the backoff = %v, want ErrSessionBusy without trying the lock", err)
	}
	if err := s.AddMessage(t.Context(), other, "user", "other"); err != nil {
		t.Fatalf("write to another session during the backoff = %v, want nil", err)
	}
	requireNoSessionFiles(t, s, key)

	now = now.Add(sessionBusyBackoff)
	if err := s.AddMessage(t.Context(), key, "user", "three"); err != nil {
		t.Fatalf("write after the backoff = %v, want nil", err)
	}
	if got := strings.Join(twoStoreContents(t, s, key), ","); got != "three" {
		t.Fatalf("history = %q, want only the write after the backoff", got)
	}
}

// Every write primitive of the store checks that this process holds the
// session's file lock: a method that skipped the lock writes nothing, and in
// the package's strict mode it panics.
func TestEveryStoreWriteHoldsTheSessionLock(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:unlocked"
	s := openTwoStore(t, dir)
	s.skipFileLockForTest = true
	withSessionLockStrict(t, false)

	// UpsertSessionMeta reaches writeSessionFile before any other primitive.
	if err := s.UpsertSessionMeta(t.Context(), key, nil, nil); !errors.Is(err, errSessionLockNotHeld) {
		t.Fatalf("UpsertSessionMeta without the lock = %v, want errSessionLockNotHeld", err)
	}
	if err := s.AddMessage(t.Context(), key, "user", "one"); !errors.Is(err, errSessionLockNotHeld) {
		t.Fatalf("AddMessage without the lock = %v, want errSessionLockNotHeld", err)
	}
	requireNoSessionFiles(t, s, key)

	sessionLockStrict = true
	defer func() {
		if recover() == nil {
			t.Fatal("strict mode let a write without the session lock through")
		}
		requireNoSessionFiles(t, s, key)
	}()
	_ = s.UpsertSessionMeta(t.Context(), key, nil, nil)
}

// The lock files live in sessions/.locks, which no scan of sessions/ treats as
// a session: listings, the journal sweep and the cron prune see only the
// conversations, and a lock file does not count as an existing source when a
// new session initializes its content identity.
func TestLocksFolderIsInvisibleToDirectoryScans(t *testing.T) {
	dir := t.TempDir()
	s := openTwoStore(t, dir)
	keys := []string{"agent:main:direct:scan-a", "agent:main:direct:scan-b"}
	for _, key := range keys {
		addTwoStore(t, s, key, "one")
		if !twoStoreExists(sessionlock.Path(dir, sanitizeKey(key))) {
			t.Fatal("precondition: the write must leave the session's lock file", key)
		}
	}

	if metas := s.ListSessionMetas(); len(metas) != len(keys) {
		t.Fatalf("ListSessionMetas = %d sessions, want %d", len(metas), len(keys))
	}
	if err := s.recoverPendingHistoryWrites(); err != nil {
		t.Fatalf("recoverPendingHistoryWrites = %v", err)
	}
	removed, err := s.PruneCronRuns(time.Nanosecond, time.Now().Add(time.Hour))
	if err != nil || removed != 0 {
		t.Fatalf("PruneCronRuns = %d, %v; want no conversation removed", removed, err)
	}
	for _, key := range keys {
		if !twoStoreExists(s.metaPath(key)) {
			t.Fatal("a scan removed a conversation", key)
		}
	}

	fresh := "agent:main:direct:scan-new"
	lock, err := sessionlock.Acquire(dir, sanitizeKey(fresh), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeNewHistoryContentIdentity(t.Context(), fresh); err != nil {
		t.Fatalf("InitializeNewHistoryContentIdentity = %v", err)
	}
	if _, present, err := s.readContentDomain(fresh); err != nil || !present {
		t.Fatalf("content witness present=%v err=%v; a lock file must not count as an existing source", present, err)
	}
}

// The session's removal also removes its lock file, so .locks does not keep a
// file per deleted conversation or pruned cron run.
func TestDeletionRemovesTheSessionLockFile(t *testing.T) {
	t.Run("delete-session", func(t *testing.T) {
		dir := t.TempDir()
		s := openTwoStore(t, dir)
		key := "agent:main:direct:deleted"
		addTwoStore(t, s, key, "one")
		requireSessionLockFileRemoved(t, dir, key, func() error { return s.DeleteSession(t.Context(), key) })
	})
	t.Run("delete-session-with-intent", func(t *testing.T) {
		dir := t.TempDir()
		s := openTwoStore(t, dir)
		s.SetPublicHistoryProjector(&publicCleanupFixture{})
		key := "agent:main:direct:deleted-intent"
		addTwoStore(t, s, key, "one")
		requireSessionLockFileRemoved(t, dir, key, func() error { return s.DeleteSession(t.Context(), key) })
		if twoStoreExists(s.historyRecoveryPath(key)) {
			t.Fatal("precondition: the deletion intent must be retired")
		}
	})
	t.Run("prune-cron-run", func(t *testing.T) {
		dir := t.TempDir()
		s := openTwoStore(t, dir)
		run := "agent:cron-job1-" + uuid.NewString()
		addTwoStore(t, s, run, "one")
		ageSessionFiles(t, dir, run, 30*24*time.Hour)
		requireSessionLockFileRemoved(t, dir, run, func() error {
			removed, err := s.PruneCronRuns(7*24*time.Hour, time.Now())
			if err == nil && removed != 1 {
				t.Fatalf("PruneCronRuns removed %d runs, want 1", removed)
			}
			return err
		})
	})
}

func requireSessionLockFileRemoved(t *testing.T, dir, key string, remove func() error) {
	t.Helper()
	path := sessionlock.Path(dir, sanitizeKey(key))
	if !twoStoreExists(path) {
		t.Fatal("precondition: the session's lock file must exist")
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
	if twoStoreExists(filepath.Join(dir, sanitizeKey(key)+".jsonl")) {
		t.Fatal("precondition: the session must be removed")
	}
	if twoStoreExists(path) {
		t.Fatal("the session was removed but its lock file stayed")
	}
}

func ageSessionFiles(t *testing.T, dir, key string, age time.Duration) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), sanitizeKey(key)+".") {
			continue
		}
		if err := os.Chtimes(filepath.Join(dir, entry.Name()), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

// crossPairHooks pauses a after its first file lock until proceed closes, and
// reports when b is about to take its first file lock and when it holds one.
func crossPairHooks(a, b *JSONLStore) (aHolds, proceed, bTrying, bHolds chan struct{}) {
	aHolds, proceed = make(chan struct{}), make(chan struct{})
	var aFirst sync.Once
	a.sessionFlockForTest = func(_ string, held bool) {
		if held {
			aFirst.Do(func() { close(aHolds); <-proceed })
		}
	}
	bTrying, bHolds = make(chan struct{}), make(chan struct{})
	var bTry, bHold sync.Once
	b.sessionFlockForTest = func(_ string, held bool) {
		if held {
			bHold.Do(func() { close(bHolds) })
		} else {
			bTry.Do(func() { close(bTrying) })
		}
	}
	return aHolds, proceed, bTrying, bHolds
}

// Two stores lock the same alias pair from opposite ends. Both take the file
// locks in name order, so B waits on A's first lock instead of taking the
// other one, and neither waits on the other for the full sessionLockWait.
func TestPairLocksFollowNameOrder(t *testing.T) {
	dir := t.TempDir()
	first, second := "agent:main:direct:pair-a", "agent:main:direct:pair-b"
	if sanitizeKey(first) >= sanitizeKey(second) {
		t.Fatal("precondition: first must sort before second")
	}
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, second, "one")
	withSessionLockWait(t, 2*time.Second)
	aHolds, proceed, bTrying, bHolds := crossPairHooks(a, b)

	start := time.Now()
	callA := runTwoStore(func() error {
		_, err := a.PromoteAliasHistory(t.Context(), first, nil, []string{second})
		return err
	})
	waitTwoStore(t, aHolds, "A holding its first session lock")
	callB := runTwoStore(func() error {
		_, err := b.PromoteAliasHistory(t.Context(), second, nil, []string{first})
		return err
	})
	waitTwoStore(t, bTrying, "B trying its first session lock")
	select {
	case <-bHolds: // B took a lock A does not hold yet: the order is crossed
	case <-time.After(200 * time.Millisecond): // B waits on the lock A holds
	}
	close(proceed)
	waitTwoStore(t, callA.done, "A's promotion")
	waitTwoStore(t, callB.done, "B's promotion")
	if elapsed := time.Since(start); callA.err != nil || callB.err != nil || elapsed >= sessionLockWait {
		t.Fatalf(
			"A=%v B=%v after %v; want both done before the %v wait",
			callA.err, callB.err, elapsed, sessionLockWait,
		)
	}
}

// On a case-insensitive filesystem (macOS by default) the lock files of two
// names that differ only in case are one file, as a key and its lowercased
// legacy alias are; APFS also folds Unicode normalization. The pair takes that
// file once instead of waiting on its own lock; afterwards each name's lock is
// free for another process.
func TestPairLocksShareTheFileOfNamesThatDifferInCase(t *testing.T) {
	for name, pair := range map[string][2]string{
		"case":          {"agent:main:direct:Case", "agent:main:direct:case"},
		"normalization": {"agent:main:direct:caf\u00e9", "agent:main:direct:cafe\u0301"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			canonical, alias := pair[0], pair[1]
			s := openTwoStore(t, dir)
			addTwoStore(t, s, alias, "one")
			withSessionLockWait(t, 2*time.Second)

			start := time.Now()
			if _, err := s.PromoteAliasHistory(t.Context(), canonical, nil, []string{alias}); err != nil {
				t.Fatalf("PromoteAliasHistory = %v after %v; the pair waited on its own lock", err, time.Since(start))
			}
			for _, key := range []string{canonical, alias} {
				lock, err := sessionlock.TryAcquire(dir, sanitizeKey(key))
				if err != nil {
					t.Fatalf("lock of %s after the promotion = %v, want free", key, err)
				}
				if err := lock.Release(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Only names that a case- or normalization-insensitive filesystem could fold
// into one file are compared on disk: two plain ASCII names that differ beyond
// case cost no stat, as on EFS, where sharing never happens.
func TestOnlyNamesThatMayFoldTogetherAreCompared(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"agent_main_direct_Case", "agent_main_direct_case", true},
		{"agent_main_direct_pair-a", "agent_main_direct_pair-b", false},
		{"agent_main_direct_caf\u00e9", "agent_main_direct_cafe\u0301", true},
	} {
		if got := mayShareLockFile(c.a, c.b); got != c.want {
			t.Errorf("mayShareLockFile(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// The empty key stays a session of its own, as before the lock (files .jsonl
// and .meta.json), under a lock with a reserved name that no sanitized key can take.
func TestEmptyKeyHasItsOwnLock(t *testing.T) {
	dir := t.TempDir()
	s := openTwoStore(t, dir)
	addTwoStore(t, s, "", "one")
	if got := strings.Join(twoStoreContents(t, s, ""), ","); got != "one" {
		t.Fatalf("history of the empty key = %q, want one", got)
	}
	if sanitizeKey(emptyKeyLockName) == emptyKeyLockName {
		t.Fatal("a sanitized key could take the empty key's lock name")
	}
	withSessionLockWait(t, 50*time.Millisecond)
	lock, err := sessionlock.Acquire(dir, emptyKeyLockName, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := s.AddMessage(t.Context(), "", "user", "two"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("empty key under another holder's lock = %v, want ErrSessionBusy", err)
	}
}

// SessionFileName maps every file of a session to the name its lock uses, and
// refuses the files of sessions/ that belong to no single session.
func TestSessionFileNameNamesEverySessionFile(t *testing.T) {
	name := sanitizeKey("agent:main:direct:x")
	for _, base := range []string{
		name + ".meta.json",
		name + ".jsonl",
		name + ".jsonl.commit.json",
		name + ".jsonl.commit.decision",
		name + ".jsonl.recovery-required",
		name + ".jsonl.order-witness",
		name + ".jsonl.content-domain",
		name + ".jsonl.read-recovery",
		name + ".jsonl.public",
		name + ".jsonl.public.index",
		name + ".jsonl.public.index.prepare",
		name + ".jsonl.public.index.building",
		name + ".jsonl.content-preimage." + uuid.NewString(),
		name + ".json",
	} {
		if got, ok := SessionFileName(base); !ok || got != name {
			t.Errorf("SessionFileName(%q) = %q, %v; want %q", base, got, ok, name)
		}
	}
	dotted := "a.jsonl.b"
	for _, base := range []string{
		dotted + ".jsonl", dotted + ".jsonl.commit.json", dotted + ".meta.json", dotted + ".json",
	} {
		if got, ok := SessionFileName(base); !ok || got != dotted {
			t.Errorf("SessionFileName(%q) = %q, %v; want %q", base, got, ok, dotted)
		}
	}
	for _, base := range []string{
		".pc-history-0123-rows-4567.idx",
		".pc-history-0123-blob-4567.blob",
		".tmp-12-34",
		"session-12.tmp",
		sessionlock.Dir,
		".jsonl",
		".meta.json",
		name + ".jsonlx",
		"sub/" + name + ".jsonl",
	} {
		if got, ok := SessionFileName(base); ok {
			t.Errorf("SessionFileName(%q) = %q, true; want not a session file", base, got)
		}
	}
}

// The prune does not wait for a run another store holds: it skips it, leaves
// its files, and returns at once.
func TestPruneSkipsARunAnotherStoreHolds(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	name := "agent_cron-job1-held"
	writeSession(t, dir, name, 20*24*time.Hour)
	holdForeignSessionLock(t, dir, name)

	start := time.Now()
	removed, err := s.PruneCronRuns(14*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("prune waited %v for a run another store holds", took)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	for _, suffix := range []string{".jsonl", ".meta.json"} {
		if !twoStoreExists(filepath.Join(dir, name+suffix)) {
			t.Fatalf("%s%s was removed under another store's lock", name, suffix)
		}
	}
}
