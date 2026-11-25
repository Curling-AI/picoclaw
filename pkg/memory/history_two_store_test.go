package memory

// Two JSONLStore instances on one directory stand in for the leaving pod (A)
// and the new pod (B) of a rollout sharing sessions/ on EFS. Each test asserts
// the safe outcome; before the per-session file lock it failed, because the
// two stores corrupted each other's history.
// A writer is paused with the private writeFile seam; "death" is
// runtime.Goexit inside that seam, which runs only deferred unlock/close.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Boot: B's constructor sweep skips A's live journal; A's write completes.
func TestTwoStoreConstructorSweepSkipsLiveWrite(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:boot-sweep"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	before := readTwoStoreState(t, dir, key)
	t.Logf("before A write:        %v", before)

	pause := armTwoStorePause(a, a.metaPath(key), false)
	call := runTwoStore(func() error { return a.AddMessage(t.Context(), key, "user", "three-A") })
	waitTwoStore(t, pause.reached, "A before its meta publish")
	paused := readTwoStoreState(t, dir, key)
	t.Logf("A paused before meta:  %v", paused)
	if !paused.Journal || !twoStoreHas(paused.Lines, "three-A") || paused.MetaBytes != before.MetaBytes {
		t.Fatal("precondition: A must have its journal and synced row on disk with OLD meta", paused)
	}

	b := openTwoStore(t, dir) // the new pod's boot: NewJSONLStore runs the sweep
	swept := readTwoStoreState(t, dir, key)
	t.Logf("after B constructor:   %v", swept)
	if !swept.Journal || swept.Decision || !twoStoreHas(swept.Lines, "three-A") {
		t.Fatal("expected B to skip A's live write: journal and three-A stay, no decision", swept)
	}

	pause.resume <- false
	waitTwoStore(t, call.done, "A append to finish")
	t.Logf("A AddMessage(three-A) returned: %v", call.err)
	if call.err != nil {
		t.Fatal("expected A's append to succeed", call.err)
	}
	if _, err := a.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "user", Content: "four-A-receipt"},
	); err != nil {
		t.Fatal("expected A's receipt append to succeed", err)
	}
	if err := a.AddMessage(t.Context(), key, "user", "five-A"); err != nil {
		t.Fatal("expected A's next append to succeed", err)
	}
	after := readTwoStoreState(t, dir, key)
	t.Logf("after A next appends:  %v", after)

	fresh := openTwoStore(t, dir)
	contents := twoStoreContents(t, fresh, key)
	page, pageErr := twoStorePage(t, fresh, key)
	t.Logf("fresh store GetHistory: %v", contents)
	t.Logf("fresh store ReadHistoryPage: entries=%d complete=%v err=%v", len(page.Entries), page.Complete, pageErr)
	t.Logf("B GetHistory: %v", twoStoreContents(t, b, key))
	if after.Marker || strings.Join(contents, ",") != "one,two,three-A,four-A-receipt,five-A" ||
		pageErr != nil || !page.Complete {
		t.Fatal("expected all rows kept, no latch and a complete page", contents, after, pageErr)
	}
}

// Hot reload or projector: B, already open, runs the same sweep while A is
// mid-write and skips A's live journal.
func TestTwoStoreReloadSweepSkipsLiveJournal(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:reload-sweep"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir)

	var bWrites []string
	b.writeFile = func(path string, data []byte, mode os.FileMode) error {
		bWrites = append(bWrites, filepath.Base(path))
		return fileutil.WriteFileAtomic(path, data, mode)
	}

	pause := armTwoStorePause(a, a.metaPath(key), false)
	call := runTwoStore(func() error { return a.AddMessage(t.Context(), key, "user", "three-A") })
	waitTwoStore(t, pause.reached, "A before its meta publish")
	t.Logf("A paused before meta:  %v", readTwoStoreState(t, dir, key))

	if err := b.recoverPendingHistoryWrites(); err != nil { // ApplyConfig / projector path
		t.Fatal(err)
	}
	swept := readTwoStoreState(t, dir, key)
	t.Logf("B writes during sweep: %v", bWrites)
	t.Logf("after B sweep:         %v", swept)

	pause.resume <- false
	waitTwoStore(t, call.done, "A append to finish")
	published := readTwoStoreState(t, dir, key)
	t.Logf("A AddMessage(three-A) returned: %v", call.err)
	t.Logf("after A meta publish:  %v", published)

	nextErr := a.AddMessage(t.Context(), key, "user", "four-A")
	t.Logf("A AddMessage(four-A) returned: %v", nextErr)
	after := readTwoStoreState(t, dir, key)
	t.Logf("after A next append:   %v", after)

	if len(bWrites) != 0 || !swept.Journal || !twoStoreHas(swept.Lines, "three-A") {
		t.Fatal("expected B's sweep to skip A's live journal without writing", bWrites, swept)
	}
	if call.err != nil || published.MetaBytes != published.JSONLBytes {
		t.Fatal("expected A to publish a meta that matches its file", call.err, published)
	}
	if nextErr != nil || after.Marker {
		t.Fatal("expected A's next append to succeed without a latch", nextErr, after)
	}
}

// Earlier window: B sweeps after A's journal but before A's bytes. The
// journal stays, so a failure of A's meta publish still rolls A's own row back.
func TestTwoStoreSweepBeforeAppendKeepsAJournal(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:early-sweep"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	before := readTwoStoreState(t, dir, key)

	pause := armTwoStorePause(a, a.historyRecoveryPath(key), true)
	journalPause := a.writeFile
	sentinel := errors.New("injected meta publish failure")
	var metaFailed atomic.Bool
	a.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == a.metaPath(key) && pause.fired.Load() && metaFailed.CompareAndSwap(false, true) {
			return sentinel // only A's first meta publish fails, as in the control
		}
		return journalPause(path, data, mode)
	}
	call := runTwoStore(func() error { return a.AddMessage(t.Context(), key, "user", "three-A") })
	waitTwoStore(t, pause.reached, "A after its journal")
	paused := readTwoStoreState(t, dir, key)
	t.Logf("A paused after journal: %v", paused)

	_ = openTwoStore(t, dir)
	swept := readTwoStoreState(t, dir, key)
	t.Logf("after B constructor:    %v", swept)

	pause.resume <- false
	waitTwoStore(t, call.done, "A append to finish")
	failed := readTwoStoreState(t, dir, key)
	t.Logf("A AddMessage(three-A) with failing meta publish returned: %v", call.err)
	t.Logf("after A failure path:   %v", failed)

	fresh := openTwoStore(t, dir)
	page, pageErr := twoStorePage(t, fresh, key)
	t.Logf("fresh store GetHistory: %v", twoStoreContents(t, fresh, key))
	t.Logf("fresh store ReadHistoryPage: entries=%d complete=%v err=%v", len(page.Entries), page.Complete, pageErr)

	if !paused.Journal || !swept.Journal || swept.JSONLBytes != before.JSONLBytes {
		t.Fatal("expected B to leave A's journal alone before A wrote its row", paused, swept)
	}
	if !errors.Is(call.err, sentinel) || twoStoreHas(failed.Lines, "three-A") || failed.Journal || pageErr != nil {
		t.Fatal("expected A's failed append to roll its own row back through its journal", call.err, failed, pageErr)
	}
}

// Earlier-window control: without B's sweep, the same failing meta publish
// rolls A's own row back through its journal and the session stays proven.
func TestTwoStoreControlFailedPublishRollsBackWithJournal(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:early-sweep-control"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	sentinel := errors.New("injected meta publish failure")
	failed := false
	a.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == a.metaPath(key) && !failed {
			failed = true
			return sentinel
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	err := a.AddMessage(t.Context(), key, "user", "three-A")
	state := readTwoStoreState(t, dir, key)
	a.writeFile = nil
	fresh := openTwoStore(t, dir)
	page, pageErr := twoStorePage(t, fresh, key)
	t.Logf("A AddMessage(three-A) with failing meta publish returned: %v", err)
	t.Logf("after A failure path:   %v", state)
	t.Logf("fresh store ReadHistoryPage: entries=%d complete=%v err=%v", len(page.Entries), page.Complete, pageErr)
	if !errors.Is(err, sentinel) || twoStoreHas(state.Lines, "three-A") || state.Journal || pageErr != nil {
		t.Fatal("control: A's own journal should roll its row back", err, state, pageErr)
	}
}

// A dies after its journal and a partial append. B, already open, recovers
// that orphan under the session lock before it writes its own journal.
func TestTwoStoreOpenStoreRecoversOrphanBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:open-store-orphan"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	b := openTwoStore(t, dir) // B's sweep runs now, before the orphan exists
	b.ListSessionMetas()      // WarmSessionListing at B's boot
	before := readTwoStoreState(t, dir, key)
	t.Logf("before A write:        %v", before)

	killAWithTornSuffix(t, a, key)
	orphan, err := os.ReadFile(b.historyRecoveryPath(key))
	if err != nil {
		t.Fatal("precondition: orphan journal must exist", err)
	}
	dead := readTwoStoreState(t, dir, key)
	t.Logf("after A death:         %v", dead)

	overwritten := recordJournalOverwrite(b, key)
	bErr := b.AddMessage(t.Context(), key, "user", "five-B")
	afterB := readTwoStoreState(t, dir, key)
	t.Logf("B AddMessage(five-B) returned: %v", bErr)
	t.Logf(
		"journal B found at its beginHistoryWrite == A's orphan: %v (%dB)",
		bytes.Equal(*overwritten, orphan),
		len(*overwritten),
	)
	t.Logf("after B append:        %v", afterB)

	fresh := openTwoStore(t, dir)
	contents := twoStoreContents(t, fresh, key)
	page, pageErr := twoStorePage(t, fresh, key)
	t.Logf("fresh store GetHistory: %v", contents)
	t.Logf("fresh store ReadHistoryPage: entries=%d complete=%v err=%v", len(page.Entries), page.Complete, pageErr)

	if bErr != nil || bytes.Equal(*overwritten, orphan) {
		t.Fatal("expected B to recover A's orphan before writing its own journal", bErr, afterB)
	}
	if strings.Join(afterB.Lines, ",") != "one,two,five-B" || afterB.Marker {
		t.Fatal("expected A's rows rolled back and no latch", afterB)
	}
	if pageErr != nil || !page.Complete || len(page.Entries) != 3 {
		t.Fatal("expected a complete page of three entries", page, pageErr)
	}
}

// Orphan control: the same orphan seen by a store opened after A's death is
// recovered OLD (A's unpublished rows removed) and the session stays proven.
func TestTwoStoreControlSweepRecoversOrphanOld(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:orphan-control"
	a := openTwoStore(t, dir)
	addTwoStore(t, a, key, "one", "two")
	pause := armTwoStorePause(a, a.metaPath(key), false)
	call := runTwoStore(func() error { return a.AddMessage(t.Context(), key, "user", "three-A") })
	waitTwoStore(t, pause.reached, "A before its meta publish")
	pause.resume <- true
	waitTwoStore(t, call.done, "A goroutine exit")
	c := openTwoStore(t, dir)
	if err := c.AddMessage(t.Context(), key, "user", "five-C"); err != nil {
		t.Fatal(err)
	}
	state := readTwoStoreState(t, dir, key)
	page, pageErr := twoStorePage(t, c, key)
	t.Logf("after C boot + append: %v", state)
	t.Logf("C ReadHistoryPage: entries=%d complete=%v err=%v", len(page.Entries), page.Complete, pageErr)
	if twoStoreHas(state.Lines, "three-A") || state.Marker || pageErr != nil {
		t.Fatal("control: a sweep after A's death should recover OLD without a latch", state, pageErr)
	}
}

// Stale cache without a content witness: B's append from its stale warm meta rereads
// the disk under the lock, so nothing latches and no row is lost.
func TestTwoStoreStaleCacheAppendRereadsDisk(t *testing.T) {
	r := twoStoreStaleCache(t, false, false)
	requireTwoStoreStale(t, r)
	if r.appendErr != nil || r.afterAppend.Marker || r.afterAppend.MetaRecovery || r.pageErr != nil ||
		len(r.page.Entries) != 4 {
		t.Fatal("expected B's append to reread the disk and stay proven", r.appendErr, r.afterAppend, r.pageErr)
	}
	if strings.Join(r.afterAppend.Lines, ",") != "one,two,three-A,four-B" {
		t.Fatal("unexpected rows", r.afterAppend.Lines)
	}
}

// Stale cache with a content witness: contentAppendMetadata rereads disk and adopts
// A's meta, so B's append alone neither latches nor loses rows. This test
// asserts the SAFE outcome: the append defect is absent on this path.
func TestTwoStoreStaleMetaAppendRebasedWithWitness(t *testing.T) {
	r := twoStoreStaleCache(t, true, false)
	requireTwoStoreStale(t, r)
	if r.appendErr != nil || r.afterAppend.Marker || r.afterAppend.MetaRecovery || r.pageErr != nil ||
		!r.page.Complete || len(r.page.Entries) != 4 ||
		strings.Join(r.afterAppend.Lines, ",") != "one,two,three-A,four-B" {
		t.Fatal("expected the witness path to rebase B's append on disk meta", r.appendErr, r.afterAppend, r.pageErr)
	}
}

// Stale cache, read only: a page read by B from its stale cache rereads the disk and
// writes no latch, with or without a content witness.
func TestTwoStoreStaleCachePageReadRereadsDisk(t *testing.T) {
	for _, witness := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-witness", true: "witness"}[witness], func(t *testing.T) {
			r := twoStoreStaleCache(t, witness, true)
			requireTwoStoreStale(t, r)
			if r.readErr != nil || r.afterRead.Marker || len(r.pageRead.Entries) != 3 ||
				strings.Join(r.afterRead.Lines, ",") != "one,two,three-A" {
				t.Fatal(
					"expected B's page read to reread the disk without latching",
					r.readErr,
					r.afterRead,
					r.pageRead,
				)
			}
			if r.pageErr != nil {
				t.Fatal("expected a fresh store to read the page", r.pageErr)
			}
		})
	}
}

// Stale cache, receipt path (delivery mirror, history context): AppendHistoryReceipt
// rereads the disk under the lock instead of probing B's stale cached meta.
func TestTwoStoreStaleCacheReceiptRereadsDisk(t *testing.T) {
	for _, witness := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-witness", true: "witness"}[witness], func(t *testing.T) {
			r := twoStoreStaleCacheWith(t, witness, false, true)
			requireTwoStoreStale(t, r)
			if r.appendErr != nil || r.afterAppend.Marker || r.pageErr != nil ||
				!twoStoreHas(r.afterAppend.Lines, "four-B") {
				t.Fatal(
					"expected B's receipt append to reread the disk and stay proven",
					r.appendErr,
					r.afterAppend,
					r.pageErr,
				)
			}
		})
	}
}
