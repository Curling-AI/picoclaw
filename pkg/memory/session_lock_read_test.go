//go:build unix

package memory

// Reads under the per-session file lock: a plain read takes the lock only to
// write (recover, latch), a proof read always takes it, and the owner's delete
// reads the aliases from the meta on disk under it.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A plain read does not wait for another process's lock. When it must first
// recover a journal this process left pending (as a failed journal cleanup
// does), it takes the session's lock to write, and gives up like a write while
// another process holds it.
func TestPlainReadTakesTheLockOnlyToRecover(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:plain-read"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, key, "one")
	withSessionLockWait(t, 50*time.Millisecond)
	now := time.Now()
	withSessionLockClock(t, func() time.Time { return now })
	foreign := holdForeignSessionLock(t, dir, key)

	if got, err := s.GetHistory(t.Context(), key); err != nil || len(got) != 1 {
		t.Fatalf("GetHistory with nothing to recover = %v, %v; want the history without the lock", got, err)
	}
	s.markHistoryPending(key)
	if _, err := s.GetHistory(t.Context(), key); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("GetHistory that must recover under another holder's lock = %v, want ErrSessionBusy", err)
	}
	if err := foreign.Release(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(sessionBusyBackoff)
	got, err := s.GetHistory(t.Context(), key)
	if err != nil || len(got) != 1 || got[0].Content != "one" {
		t.Fatalf("GetHistory after the lock is free = %v, %v; want the recovered history", got, err)
	}
	s.metaMu.RLock()
	pending := s.historyPending[sanitizeKey(key)]
	s.metaMu.RUnlock()
	if pending {
		t.Fatal("the read returned without recovering the pending journal")
	}
}

// Proof reads decide from the meta and may set the latch, so they take the
// session's lock even with nothing to recover, and wait for another process.
func TestProofReadsTakeTheSessionLock(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:proof-read"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, key, "one")
	withSessionLockWait(t, 50*time.Millisecond)
	holdForeignSessionLock(t, dir, key)

	if _, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ReadHistoryPage under another holder's lock = %v, want ErrSessionBusy", err)
	}
	withSessionLockClock(t, func() time.Time { return time.Now().Add(sessionBusyBackoff) })
	if _, err := s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ReadHistoryWithReceipts under another holder's lock = %v, want ErrSessionBusy", err)
	}
}

// The owner's delete reads the aliases under the session's lock: a journal this
// process left pending is recovered there instead of hiding them, so the
// alias's conversation goes with the session.
func TestDeleteSessionRecoversBeforeReadingTheAliases(t *testing.T) {
	dir := t.TempDir()
	canonical, alias := "agent:main:direct:delete-canonical", "agent:main:direct:delete-alias"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, alias, "one")
	if err := s.UpsertSessionMeta(t.Context(), canonical, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	s.markHistoryPending(canonical)

	if err := s.DeleteSession(t.Context(), canonical); err != nil {
		t.Fatalf("DeleteSession = %v", err)
	}
	for _, path := range []string{s.metaPath(canonical), s.jsonlPath(alias), s.metaPath(alias)} {
		if twoStoreExists(path) {
			t.Fatalf("%s survived the delete; the alias must go with the session", path)
		}
	}
}

// Another process names a new alias after the delete removed the ones it read.
// Rereading the meta on disk under the session's lock, the delete fails and
// keeps the canonical meta, the only list of the aliases; a retry removes both.
func TestDeleteSessionKeepsTheMetaWhenAnAliasAppears(t *testing.T) {
	dir := t.TempDir()
	canonical := "agent:main:direct:appear-canonical"
	first, late := "agent:main:direct:appear-first", "agent:main:direct:appear-late"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, first, "one")
	addTwoStore(t, b, late, "two")
	if err := a.UpsertSessionMeta(t.Context(), canonical, nil, []string{first}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.sessionFlockForTest = func(name string, held bool) {
		if name != sanitizeKey(canonical) || held {
			return
		}
		// Third take of the canonical lock: aliases read, pair delete done.
		if calls++; calls == 3 {
			if err := b.UpsertSessionMeta(t.Context(), canonical, nil, []string{first, late}); err != nil {
				t.Error(err)
			}
		}
	}

	err := a.DeleteSession(t.Context(), canonical)
	if !errors.Is(err, errAliasesAddedDuringDelete) || !strings.Contains(err.Error(), late) {
		t.Fatalf("DeleteSession with an alias named meanwhile = %v, want errAliasesAddedDuringDelete", err)
	}
	if !twoStoreExists(a.metaPath(canonical)) || !twoStoreExists(a.jsonlPath(late)) {
		t.Fatal("the canonical meta must stay while it names an alias that was not removed")
	}
	a.sessionFlockForTest = nil
	if err := a.DeleteSession(t.Context(), canonical); err != nil {
		t.Fatalf("retried DeleteSession = %v", err)
	}
	for _, path := range []string{a.metaPath(canonical), a.jsonlPath(first), a.jsonlPath(late)} {
		if twoStoreExists(path) {
			t.Fatalf("%s survived the retried delete", path)
		}
	}
}

// When readMeta fails for another reason than the lock, the aliases still come
// from the meta file: the alias's conversation goes with the session's.
func TestDeleteSessionTakesTheAliasesFromDiskWhenTheMetaReadFails(t *testing.T) {
	dir := t.TempDir()
	canonical, alias := "agent:main:direct:unreadable-canonical", "agent:main:direct:unreadable-alias"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, alias, "one")
	if err := s.UpsertSessionMeta(t.Context(), canonical, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	// An order witness this process cannot read: readMeta fails, and the meta
	// file still names the alias.
	witness := s.historyOrderWitnessPath(canonical)
	if err := os.Symlink(filepath.Base(witness), witness); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetSessionMeta(t.Context(), canonical); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("precondition: readMeta = %v; want it to fail", err)
	}

	if err := s.DeleteSession(t.Context(), canonical); err != nil {
		t.Fatalf("DeleteSession = %v; want the aliases taken from the meta file", err)
	}
	if twoStoreExists(s.jsonlPath(alias)) {
		t.Fatal("the alias the meta file names survived the delete")
	}
}

// A journal of the canonical session that cannot be recovered fails the delete
// when it takes the session's lock, before it reads the aliases or removes
// anything: the canonical meta, the only list of the aliases, stays with them.
func TestDeleteSessionStopsAtAJournalItCannotRecover(t *testing.T) {
	dir := t.TempDir()
	canonical, alias := "agent:main:direct:undecodable-canonical", "agent:main:direct:undecodable-alias"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, alias, "one")
	if err := s.UpsertSessionMeta(t.Context(), canonical, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.historyRecoveryPath(canonical), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := s.DeleteSession(t.Context(), canonical)
	if err == nil || !strings.Contains(err.Error(), "decode history recovery") {
		t.Fatalf("DeleteSession = %v; want the recovery error", err)
	}
	if !twoStoreExists(s.jsonlPath(alias)) || !twoStoreExists(s.metaPath(canonical)) {
		t.Fatal("the delete removed files past a journal it could not recover")
	}
}

// The reread of the aliases before the canonical meta goes fails closed: a
// meta file that cannot be read (other than missing) fails the delete, and the
// canonical meta, the only list of the aliases, stays.
func TestDeleteSessionFailsClosedWhenTheAliasesCannotBeReread(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without permissions")
	}
	dir := t.TempDir()
	key := "agent:main:direct:reread-fails"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, key, "one")
	t.Cleanup(func() { _ = os.Chmod(s.metaPath(key), 0o644) })
	calls := 0
	s.sessionFlockForTest = func(name string, held bool) {
		if name != sanitizeKey(key) || held {
			return
		}
		// The second take of the key's lock is the delete's own, after the aliases were read.
		if calls++; calls == 2 {
			if err := os.Chmod(s.metaPath(key), 0); err != nil {
				t.Error(err)
			}
		}
	}

	err := s.DeleteSession(t.Context(), key)
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "read the aliases") {
		t.Fatalf("DeleteSession with an unreadable meta = %v; want it to fail on the aliases", err)
	}
	if !twoStoreExists(s.metaPath(key)) || !twoStoreExists(s.jsonlPath(key)) {
		t.Fatal("the session must stay when its aliases cannot be read")
	}
}
