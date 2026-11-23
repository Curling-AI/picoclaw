//go:build unix

package memory

// Taking a session's lock adopts what other processes left in the session:
// sessions/ is revalidated before the first read under the lock, and a journal
// or decision another writer left is resolved before the meta is read. The
// journal itself is created exclusively, so a journal the stat did not see is
// never replaced.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A crash between the removal of a journal and of its decision leaves the
// decision alone. The next writer retires it under the lock, before it writes.
func TestOrphanDecisionWithoutJournalIsRetiredUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:orphan-decision"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir) // B's sweep runs now, before the orphan exists
	decision := []byte(`{"journal":"0000","decision":"old"}`)
	if err := os.WriteFile(a.historyDecisionPath(key), decision, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.AddMessage(t.Context(), key, "user", "three-B"); err != nil {
		t.Fatalf("AddMessage = %v; want the orphan decision retired and the write done", err)
	}
	if twoStoreExists(b.historyDecisionPath(key)) {
		t.Fatal("the orphan decision is still on disk after B's write")
	}
	page, err := twoStorePage(t, b, key)
	if err != nil || !page.Complete || len(page.Entries) != 3 {
		t.Fatalf("page = %d entries, complete %v, %v; want a complete page of three", len(page.Entries),
			page.Complete, err)
	}
}

// An orphan journal whose recovery fails (its cleanup needs the public
// projector this store lacks) fails the write: nothing is written, and the
// journal stays for a store that can recover it.
func TestOrphanRecoveryFailureFailsTheWrite(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:orphan-cleanup"
	a := openTwoStore(t, dir)
	a.SetPublicHistoryProjector(&publicCleanupFixture{fail: true})
	addTwoStore(t, a, key, "one")
	b := openTwoStore(t, dir) // no projector: it cannot run the cleanup
	if err := a.DeleteSession(t.Context(), key); !errors.Is(err, syscall.EIO) {
		t.Fatalf("precondition: A's delete = %v; want its cleanup to fail and keep the journal", err)
	}
	journal, err := os.ReadFile(a.historyRecoveryPath(key))
	if err != nil {
		t.Fatal("precondition: the orphan journal must exist", err)
	}
	before := readTwoStoreState(t, dir, key)

	err = b.AddMessage(t.Context(), key, "user", "two-B")
	if !errors.Is(err, errPublicHistoryCleanupUnavailable) {
		t.Fatalf("AddMessage = %v; want the orphan's recovery failure", err)
	}
	after, readErr := os.ReadFile(b.historyRecoveryPath(key))
	if readErr != nil || !bytes.Equal(after, journal) {
		t.Fatalf("journal after B's write: %v; want A's orphan kept byte for byte", readErr)
	}
	if state := readTwoStoreState(t, dir, key); state.String() != before.String() {
		t.Fatalf("B's failed write changed the session:\nbefore %v\nafter  %v", before, state)
	}
}

// hideUntilRevalidated stands in for the NFS client's cache of absent names
// (K5): each path, present or not, becomes a symlink to itself, so a stat or a
// read of it fails with ELOOP, until s revalidates sessions/, which puts every
// path back as it was. It returns the count of revalidations.
func hideUntilRevalidated(t *testing.T, s *JSONLStore, paths ...string) *int {
	t.Helper()
	aside := t.TempDir()
	moved := make(map[string]string, len(paths))
	for index, path := range paths {
		if twoStoreExists(path) {
			moved[path] = filepath.Join(aside, strconv.Itoa(index))
			if err := os.Rename(path, moved[path]); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Base(path), path); err != nil {
			t.Fatal(err)
		}
	}
	revalidations := 0
	s.sessionsRevalidatedForTest = func() error {
		revalidations++
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			if moved[path] != "" {
				if err := os.Rename(moved[path], path); err != nil {
					return err
				}
			}
		}
		paths = nil
		return nil
	}
	return &revalidations
}

// Taking the lock revalidates sessions/ before it reads anything there: the
// meta, the recovery marker, the order witness, the journal and the decision
// that another pod wrote are seen by the first read under the lock.
func TestTakingTheLockRevalidatesSessionsBeforeTheFirstRead(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:revalidate"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	revalidations := hideUntilRevalidated(t, b,
		b.metaPath(key), b.historyRecoveryRequiredPath(key), b.historyOrderWitnessPath(key),
		b.historyRecoveryPath(key), b.historyDecisionPath(key))

	if err := b.AddMessage(t.Context(), key, "user", "three-B"); err != nil {
		t.Fatalf("AddMessage = %v; want every read under the lock after the revalidation", err)
	}
	state := readTwoStoreState(t, dir, key)
	if *revalidations != 1 || strings.Join(state.Lines, ",") != "one,two,three-B" || state.Marker {
		t.Fatalf("revalidations = %d, state %v; want one revalidation and B's row after A's", *revalidations, state)
	}
}

// A failed revalidation fails the acquisition: the write returns the error,
// writes nothing, and releases the lock.
func TestTakingTheLockFailsWhenTheRevalidationFails(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:revalidate-fails"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, key, "one")
	before := readTwoStoreState(t, dir, key)
	failure := errors.New("revalidation failed")
	s.sessionsRevalidatedForTest = func() error { return failure }

	if err := s.AddMessage(t.Context(), key, "user", "two"); !errors.Is(err, failure) {
		t.Fatalf("AddMessage = %v; want the revalidation failure", err)
	}
	if state := readTwoStoreState(t, dir, key); state.String() != before.String() {
		t.Fatalf("the failed write changed the session:\nbefore %v\nafter  %v", before, state)
	}
	s.sessionsRevalidatedForTest = nil
	addTwoStore(t, s, key, "two") // the lock was released
}

// killAAfterItsJournal makes A die right after its journal is durable, before
// it changes anything else in the session.
func killAAfterItsJournal(t *testing.T, a *JSONLStore, key string) []byte {
	t.Helper()
	pause := armTwoStorePause(a, a.historyRecoveryPath(key), true)
	call := runTwoStore(func() error { return a.AddMessage(t.Context(), key, "user", "three-A") })
	waitTwoStore(t, pause.reached, "A after its journal")
	pause.resume <- true // A dies here
	waitTwoStore(t, call.done, "A goroutine exit")
	journal, err := os.ReadFile(a.historyRecoveryPath(key))
	if call.returned || err != nil {
		t.Fatal("precondition: A must die and leave its journal", call.err, err)
	}
	return journal
}

// A journal name that cannot be checked fails the acquisition: the write
// returns the error and writes nothing, rather than write past a journal that
// may be there.
func TestTakingTheLockFailsWhenTheJournalCannotBeChecked(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:journal-unchecked"
	s := openTwoStore(t, dir)
	addTwoStore(t, s, key, "one")
	journal := s.historyRecoveryPath(key)
	if err := os.Symlink(filepath.Base(journal), journal); err != nil {
		t.Fatal(err)
	}
	before := readTwoStoreState(t, dir, key)

	if unlock, err := s.lockSession(key); !errors.Is(err, syscall.ELOOP) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("lockSession = %v; want the journal's stat error from the acquisition", err)
	}
	if err := s.AddMessage(t.Context(), key, "user", "two"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("AddMessage = %v; want the journal's stat error", err)
	}
	if state := readTwoStoreState(t, dir, key); state.String() != before.String() {
		t.Fatalf("the failed write changed the session:\nbefore %v\nafter  %v", before, state)
	}
}

// foreignJournalAtCreate leaves A's journal hidden from B's stat under the lock
// (another client created it and B's cache still hides it), and shows it only
// when B creates its own journal. Without linkWorks, link(2) fails with EPERM
// and B falls back to O_EXCL. It returns B, the count of B's creates, and
// whether A's journal was intact when B's first create met it.
func foreignJournalAtCreate(t *testing.T, dir, key string, linkWorks bool) (*JSONLStore, *int, *bool) {
	t.Helper()
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir)
	journal := killAAfterItsJournal(t, a, key)
	orphan := filepath.Join(t.TempDir(), "orphan")
	if err := os.Rename(b.historyRecoveryPath(key), orphan); err != nil {
		t.Fatal(err)
	}
	creates, intact := 0, false
	b.linkHistoryJournal = func(tmp, path string) error {
		creates++
		if creates == 1 { // A's journal shows up only now
			if err := os.Rename(orphan, path); err != nil {
				return err
			}
		}
		var err error = &os.LinkError{Op: "link", Old: tmp, New: path, Err: syscall.EPERM}
		if linkWorks {
			err = os.Link(tmp, path)
		}
		if creates == 1 {
			found, readErr := os.ReadFile(path)
			intact = readErr == nil && bytes.Equal(found, journal)
		}
		return err
	}
	return b, &creates, &intact
}

// requireForeignJournalRedone checks that B's write met A's journal once, left
// it intact, recovered it, and then wrote its own row once.
func requireForeignJournalRedone(t *testing.T, dir, key string, creates int, intact bool) {
	t.Helper()
	state := readTwoStoreState(t, dir, key)
	if creates != 2 || !intact || strings.Join(state.Lines, ",") != "one,two,five-B" || state.Marker ||
		state.Journal {
		t.Fatalf("creates = %d, A's journal intact = %v, state %v; want A's journal met, recovered, "+
			"and B's row written once", creates, intact, state)
	}
	requireNoTempFiles(t, dir)
	page, err := twoStorePage(t, openTwoStore(t, dir), key)
	if err != nil || !page.Complete || len(page.Entries) != 3 {
		t.Fatalf("fresh page = %d entries, complete %v, %v", len(page.Entries), page.Complete, err)
	}
}

// A journal the stat under the lock did not see is never replaced: creating
// B's journal meets it, recovers it, and B's write starts again from the meta.
// Without link(2) the journal is created with O_EXCL, which meets it the same way.
func TestJournalCreateNeverReplacesAnotherWritersJournal(t *testing.T) {
	for _, linkWorks := range []bool{true, false} {
		t.Run(map[bool]string{true: "link", false: "exclusive-open"}[linkWorks], func(t *testing.T) {
			dir, key := t.TempDir(), "agent:main:direct:exclusive"
			b, creates, intact := foreignJournalAtCreate(t, dir, key, linkWorks)

			if err := b.AddMessage(t.Context(), key, "user", "five-B"); err != nil {
				t.Fatalf("AddMessage = %v; want A's journal recovered and B's write redone", err)
			}
			requireForeignJournalRedone(t, dir, key, *creates, *intact)
		})
	}
}

// The delivery mirror appends through the content appenders and drops a
// message whose append fails: they redo after a foreign journal like AddMessage.
func TestContentAppendRedoesAfterAForeignJournal(t *testing.T) {
	appends := map[string]func(*JSONLStore, *testing.T, string, providers.Message) error{
		"input": func(s *JSONLStore, t *testing.T, key string, message providers.Message) error {
			_, err := s.AppendHistoryContentInput(t.Context(), key, message)
			return err
		},
		"qualified": func(s *JSONLStore, t *testing.T, key string, message providers.Message) error {
			_, err := s.AppendHistoryContent(t.Context(), key, message)
			return err
		},
	}
	for name, appendContent := range appends {
		t.Run(name, func(t *testing.T) {
			dir, key := t.TempDir(), "agent:main:direct:content-"+name
			b, creates, intact := foreignJournalAtCreate(t, dir, key, true)

			if err := appendContent(b, t, key, providers.Message{Role: "assistant", Content: "five-B"}); err != nil {
				t.Fatalf("append = %v; want A's journal recovered and B's append redone", err)
			}
			requireForeignJournalRedone(t, dir, key, *creates, *intact)
		})
	}
}

// A death between the journal's link and the removal of its temporary file
// leaves a journal with two links. With the source gone (a clear of a session
// with no files yet), its recovery opens the journal as an artifact, which
// refuses a second link: taking the lock drops that temporary link first, so
// the session can be written and deleted again.
func TestJournalWithASecondLinkIsRecoveredWhenItsSourceIsGone(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:second-link"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	a.linkHistoryJournal = func(tmp, path string) error {
		if err := os.Link(tmp, path); err != nil {
			return err
		}
		runtime.Goexit() // A dies before it removes the temporary file
		return nil
	}
	call := runTwoStore(func() error { return a.ClearHistory(t.Context(), key) })
	waitTwoStore(t, call.done, "A goroutine exit")
	info, err := os.Stat(a.historyRecoveryPath(key))
	if call.returned || err != nil || info.Sys().(*syscall.Stat_t).Nlink != 2 ||
		twoStoreExists(a.metaPath(key)) || twoStoreExists(a.jsonlPath(key)) {
		t.Fatal("precondition: A must die and leave only a journal with two links", call.err, err)
	}

	unrelated := filepath.Join(dir, historyJournalTempPrefix+"another-writer")
	if err := os.WriteFile(unrelated, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.AddMessage(t.Context(), key, "user", "one-B"); err != nil {
		t.Fatalf("AddMessage = %v; want the journal recovered and the write done", err)
	}
	if !twoStoreExists(unrelated) {
		t.Fatal("the temporary file of another journal was removed")
	}
	if err := os.Remove(unrelated); err != nil {
		t.Fatal(err)
	}
	requireNoTempFiles(t, dir)
	if state := readTwoStoreState(t, dir, key); strings.Join(state.Lines, ",") != "one-B" || state.Journal {
		t.Fatalf("state %v; want B's row and no journal", state)
	}
	if err := b.DeleteSession(t.Context(), key); err != nil {
		t.Fatalf("DeleteSession = %v; want the session deleted", err)
	}
}

func requireNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, entry := range sessionDirEntries(t, dir) {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left in sessions/: %s", entry.Name())
		}
	}
}
