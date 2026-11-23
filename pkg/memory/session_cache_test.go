//go:build unix

package memory

// The cache rule: the meta cache serves plain reads only. Taking a session's
// lock drops what this process cached of it, the turn's one reread at its
// start does the same without the lock, and a listing never waits for another
// process to read a session.

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Taking a session's lock drops what this process cached of the session: the
// meta, the order witness and a marker it saw absent, since another process
// may have changed them. A latch it saw set stays: a latch is never undone.
func TestTakingTheSessionLockForgetsTheSessionCache(t *testing.T) {
	dir := t.TempDir()
	key, latched := "agent:main:direct:cache-forget", "agent:main:direct:cache-latched"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, key, "one")
	addTwoStore(t, b, latched, "one")
	b.ListSessionMetas()
	var meta SessionMeta
	if err := markHistoryRecoveryRequiredLocked(t, a, key, &meta); err != nil {
		t.Fatal(err)
	}
	if err := markHistoryRecoveryRequiredLocked(t, b, latched, &meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.historyRecoveryRequiredPath(latched)); err != nil {
		t.Fatal(err)
	}

	unlock := holdSessionLockForTest(t, b, key)
	b.metaMu.RLock()
	_, metaCached := b.metaCache[sanitizeKey(key)]
	_, witnessCached := b.historyWitnesses[sanitizeKey(key)]
	b.metaMu.RUnlock()
	marked, err := b.historyRecoveryMarkerState(key)
	unlock()
	if metaCached || witnessCached || err != nil || !marked {
		t.Fatalf("under B's lock: meta cached=%v, witness cached=%v, A's latch seen=%v (%v); want the cache dropped",
			metaCached, witnessCached, marked, err)
	}
	defer holdSessionLockForTest(t, b, latched)()
	if marked, err := b.historyRecoveryMarkerState(latched); err != nil || !marked {
		t.Fatalf("B's own latch after taking the lock = %v, %v; want it kept", marked, err)
	}
}

// Plain reads serve the meta this process cached, which another process may
// have written past. The turn's reread at its start reads the meta from disk,
// and only the meta: the marker and the order witness stay cached. It does not
// wait for the session's lock.
func TestRefreshSessionMetaRereadsTheDisk(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:turn-start"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b.ListSessionMetas()
	if err := a.SetSummary(t.Context(), key, "summary-A"); err != nil {
		t.Fatal(err)
	}
	if err := a.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	if summary, err := b.GetSummary(t.Context(), key); err != nil || summary != "" {
		t.Fatalf("precondition: B's plain read serves its cache, got %q, %v", summary, err)
	}
	trapFileReads(t, b.historyRecoveryRequiredPath(key), b.historyOrderWitnessPath(key))
	withSessionLockWait(t, 5*time.Second)
	holdForeignSessionLock(t, dir, key)

	start := time.Now()
	if err := b.RefreshSessionMeta(t.Context(), key); err != nil {
		t.Fatalf("RefreshSessionMeta = %v; want it to read the meta alone", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the reread waited %v for another process's lock", took)
	}
	summary, err := b.GetSummary(t.Context(), key)
	if err != nil || summary != "summary-A" {
		t.Fatalf("GetSummary after the reread = %q, %v; want A's summary", summary, err)
	}
	history, err := b.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "two" {
		t.Fatalf("GetHistory after the reread = %v, %v; want A's truncation", history, err)
	}
}

// A witness that no longer matches the meta the turn's reread found (another
// process deleted and recreated the session) makes the reread redo under the
// session's lock, where the whole cache is dropped: no false latch.
func TestRefreshSessionMetaRedoesUnderTheLockWhenTheWitnessMoved(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:turn-moved"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, key, "one")
	b.ListSessionMetas()
	if err := a.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	addTwoStore(t, a, key, "fresh")
	var locked []string
	b.sessionFlockForTest = func(name string, held bool) {
		if held {
			locked = append(locked, name)
		}
	}

	if err := b.RefreshSessionMeta(t.Context(), key); err != nil {
		t.Fatalf("RefreshSessionMeta = %v", err)
	}
	if len(locked) != 1 || twoStoreExists(b.historyRecoveryRequiredPath(key)) {
		t.Fatalf("locks taken = %v, latch written = %v; want one redo under the lock and no latch",
			locked, twoStoreExists(b.historyRecoveryRequiredPath(key)))
	}
	history, err := b.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "fresh" {
		t.Fatalf("GetHistory after the reread = %v, %v; want the recreated session", history, err)
	}
}

// trapFileReads puts at each path a symlink to itself, so any stat or read of
// it fails with ELOOP.
func trapFileReads(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if !twoStoreExists(path) && !strings.HasSuffix(path, ".recovery-required") {
			t.Fatalf("precondition: %s is missing", filepath.Base(path))
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(path), path); err != nil {
			t.Fatal(err)
		}
	}
}

// A listing reads like a sweep: a session it must recover first, while another
// process holds that session's lock, is left out at once and counted in one
// log line, instead of waiting for the lock.
func TestListingDoesNotWaitForASessionItMustRecover(t *testing.T) {
	dir := t.TempDir()
	held, free := "agent:main:direct:list-held", "agent:main:direct:list-free"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, held, "one")
	addTwoStore(t, s, free, "one")
	s.markHistoryPending(held)
	withSessionLockWait(t, 5*time.Second)
	holdForeignSessionLock(t, dir, held)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	start := time.Now()
	metas := s.ListSessionMetas()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the listing waited %v for another process's lock", took)
	}
	keys := make([]string, 0, len(metas))
	for _, meta := range metas {
		keys = append(keys, meta.Key)
	}
	if strings.Join(keys, ",") != free {
		t.Fatalf("listing = %v, want only %s", keys, free)
	}
	if got := strings.Count(logs.String(), "1 sessions skipped: locked by another process"); got != 1 {
		t.Fatalf("skip log lines = %d, want 1; log:\n%s", got, logs.String())
	}
}
