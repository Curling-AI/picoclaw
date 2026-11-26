//go:build unix

package memory

// A holder that lost its lease must not create a journal, a deletion intent or
// an order witness: each lands on a session another holder has written since,
// and a stale journal or intent fails every later holder's recovery.

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

// leaseLoss plays A losing its lease at one instant: the lock file goes, as
// the server forgets A's lock, and B takes the lock on a new file and runs its
// write. From then on every Check of A fails.
type leaseLoss struct {
	lost bool
	bErr error
	// trigger makes the loss happen now, if it has not yet.
	trigger func()
}

// loseLeaseAt makes A's lease go at A's nth Check (never when nth is 0) or
// right before A writes one of paths, whichever comes first.
func loseLeaseAt(
	t *testing.T, a *JSONLStore, dir, key string, nth int, bWrite func() error, paths ...string,
) *leaseLoss {
	t.Helper()
	loss := &leaseLoss{}
	loss.trigger = func() {
		if loss.lost {
			return
		}
		loss.lost = true
		if err := os.Remove(sessionlock.Path(dir, sanitizeKey(key))); err != nil {
			loss.bErr = err
			return
		}
		loss.bErr = bWrite()
	}
	checks := 0
	a.checkLockForTest = func(*sessionlock.Lock) error {
		if checks++; nth > 0 && checks >= nth {
			loss.trigger()
		}
		if loss.lost {
			return fmt.Errorf("%w: %w", sessionlock.ErrLost, syscall.EIO)
		}
		return nil
	}
	a.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if slices.Contains(paths, path) {
			loss.trigger()
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	t.Cleanup(func() { a.checkLockForTest, a.writeFile, a.contentStorageFailure = nil, nil, nil })
	return loss
}

func requireLeaseLost(t *testing.T, loss *leaseLoss, err error) {
	t.Helper()
	if !loss.lost || loss.bErr != nil {
		t.Fatalf("precondition: B must write after A's lease is lost (lost=%v, B = %v)", loss.lost, loss.bErr)
	}
	if !errors.Is(err, ErrSessionLockLost) {
		t.Fatalf("A's write after its lease was lost = %v, want ErrSessionLockLost", err)
	}
}

// The lease goes after A's reads, right before its journal would be created.
// A's journal would record an OLD that B's write has replaced, and recovery
// matches it to neither OLD nor NEW: the Check before the creation stops it.
func TestLostLeaseBeforeTheJournalLeavesNoStaleJournal(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-journal"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir)
	loss := loseLeaseAt(t, a, dir, key, 1, func() error {
		return b.AddMessage(t.Context(), key, "user", "three-B")
	}, a.historyRecoveryPath(key))

	err := a.AddMessage(t.Context(), key, "user", "three-A")
	requireLeaseLost(t, loss, err)
	if state := readTwoStoreState(t, dir, key); state.Journal ||
		!slices.Equal(state.Lines, []string{"one", "two", "three-B"}) {
		t.Fatalf("after A's lost write: %v; want B's rows and no journal of A's", state)
	}

	a.checkLockForTest, a.writeFile = nil, nil
	requireNextHolderOwnsTheSession(t, a, dir, key, "one", "two", "three-B")
}

// The lease goes after A's delete captured the files, right before its intent
// would be created. The intent would bind identities B's write has changed,
// and its recovery would fail every later holder: the Check stops it.
func TestLostLeaseBeforeTheDeletionIntentLeavesNoStaleIntent(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-intent-create"
	a := openTwoStore(t, dir)
	a.SetPublicHistoryProjector(&publicCleanupFixture{})
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir)
	b.SetPublicHistoryProjector(&publicCleanupFixture{})
	loss := loseLeaseAt(t, a, dir, key, 0, func() error {
		return b.AddMessage(t.Context(), key, "user", "three-B")
	})
	a.contentStorageFailure = func(_, phase string) error {
		if phase == "delete-intent-create" {
			loss.trigger()
		}
		return nil
	}

	err := a.DeleteSession(t.Context(), key)
	requireLeaseLost(t, loss, err)
	if state := readTwoStoreState(t, dir, key); state.Journal ||
		!slices.Equal(state.Lines, []string{"one", "two", "three-B"}) {
		t.Fatalf("after A's lost delete: %v; want B's rows and no intent of A's", state)
	}

	a.checkLockForTest, a.writeFile, a.contentStorageFailure = nil, nil, nil
	next := openTwoStore(t, dir)
	next.SetPublicHistoryProjector(&publicCleanupFixture{})
	if err := next.AddMessage(t.Context(), key, "user", "four"); err != nil {
		t.Fatalf("the next holder's AddMessage(four) = %v, want nil", err)
	}
	if err := next.DeleteSession(t.Context(), key); err != nil {
		t.Fatalf("the next holder's DeleteSession = %v, want nil", err)
	}
	if twoStoreExists(next.metaPath(key)) || twoStoreExists(next.jsonlPath(key)) {
		t.Fatal("the next holder's delete left the session")
	}
}

// The lease goes after A's journal, right before A's first order witness. B
// recovers A's journal and makes the first ordered commit with its own
// witness; A's would replace it and void B's order proofs. The Check before
// the witness stops A, and A reports the loss, not an order failure.
func TestLostLeaseBeforeTheFirstWitnessKeepsTheNextHoldersWitness(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:lost-witness"
	a, b := openTwoStore(t, dir), openTwoStore(t, dir)
	var bWitness []byte
	loss := loseLeaseAt(t, a, dir, key, 2, func() error {
		err := b.AddMessage(t.Context(), key, "user", "one-B")
		bWitness, _ = os.ReadFile(b.historyOrderWitnessPath(key))
		return err
	}, a.historyOrderWitnessPath(key))

	err := a.AddMessage(t.Context(), key, "user", "one-A")
	requireLeaseLost(t, loss, err)
	witness, readErr := os.ReadFile(a.historyOrderWitnessPath(key))
	if readErr != nil || len(bWitness) == 0 || string(witness) != string(bWitness) {
		t.Fatalf("witness after A's lost write = %q (%v); want B's %q", witness, readErr, bWitness)
	}

	a.checkLockForTest, a.writeFile = nil, nil
	next := openTwoStore(t, dir)
	if err := next.AddMessage(t.Context(), key, "user", "two"); err != nil {
		t.Fatalf("the next holder's AddMessage(two) = %v, want nil", err)
	}
	if _, err := next.AppendHistoryReceipt(
		t.Context(), key, providers.Message{Role: "user", Content: "three"},
	); err != nil {
		t.Fatalf("the next holder's receipt append = %v, want B's order proofs intact", err)
	}
	if state := readTwoStoreState(t, dir, key); state.Marker ||
		!slices.Equal(state.Lines, []string{"one-B", "two", "three"}) {
		t.Fatalf("after the next holder: %v; want B's rows, its own and no latch", state)
	}
}
