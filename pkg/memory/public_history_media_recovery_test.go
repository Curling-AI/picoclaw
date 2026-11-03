package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func deferredMediaFixture(t *testing.T) (*JSONLStore, string, string, context.Context) {
	t.Helper()
	store, key, run := newTestStore(t), "agent:web:deferred-media", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err := store.AddMessage(ctx, key, "assistant", "projection gap"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[report](artifact:immutable.pdf)"); err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	return store, key, run, ctx
}

func TestPublicMediaPreparingLegacySourceAndLargeRawRowKeepAdvancing(t *testing.T) {
	store, key := legacyPublicFixture(t, 60)
	run := uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", strings.Repeat("x", PublicHistoryMessageBytes-2048)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[first](artifact:first.pdf)"); err != nil {
		t.Fatal("new native run in an unprepared legacy source lost media", err)
	}
	if err := store.AddMessage(ctx, key, "tool", "second native occurrence"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[first](artifact:first.pdf)"); err != nil {
		t.Fatal(err)
	}
	result := finishDeferredMedia(t, store, key)
	if len(result.Entries) != 64 || result.Entries[61].AnchorID != result.Entries[60].ID ||
		result.Entries[63].AnchorID != result.Entries[62].ID || result.Entries[61].ID == result.Entries[63].ID {
		t.Fatal(
			"legacy preparation lost ordering or conflated identical media from different native occurrences",
			len(result.Entries),
		)
	}
}

func TestPublicMediaPreparingRacesWithItsExplicitPreparation(t *testing.T) {
	store, key, run, ctx := deferredMediaFixture(t)
	var group sync.WaitGroup
	failures := make(chan error, 2)
	group.Add(2)
	go func() {
		defer group.Done()
		for index := 0; index < 10; index++ {
			if err := store.RecordPublicMedia(ctx, key, run, "[report](artifact:immutable.pdf)"); err != nil {
				failures <- err
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		preparation, err := store.PreparePublicHistory(ctx, key, "")
		for index := 0; err == nil && !preparation.Ready && index < 10; index++ {
			preparation, err = store.PreparePublicHistory(ctx, key, preparation.Cursor)
			if errors.Is(err, ErrPublicHistoryCoverage) {
				preparation, err = store.PreparePublicHistory(ctx, key, "")
			}
		}
		if err != nil {
			failures <- fmt.Errorf("prepare media race: %w", err)
		}
	}()
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if result := finishDeferredMedia(t, store, key); len(result.Entries) != 3 {
		t.Fatal("race duplicated media", result)
	}
}

func finishDeferredMedia(t *testing.T, store *JSONLStore, key string) PublicHistory {
	t.Helper()
	preparation, err := store.PreparePublicHistory(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; !preparation.Ready && step < 10; step++ {
		preparation, err = store.PreparePublicHistory(t.Context(), key, preparation.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || !preparation.Ready {
		t.Fatal(result, err)
	}
	return result
}

func TestPublicMediaPreparationRollbackRestoresTheExactOldTailAndIndex(t *testing.T) {
	store, key, _, ctx := deferredMediaFixture(t)
	preparation, err := store.PreparePublicHistory(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(store.publicIndexPath(key))
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && fail {
			fail = false
			return errors.New("metadata publication interrupted")
		}
		return os.WriteFile(path, data, mode)
	}
	if _, err = store.PreparePublicHistory(ctx, key, preparation.Cursor); err == nil {
		t.Fatal("metadata failure was hidden")
	}
	store.writeFile = nil
	afterPublic, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	afterIndex, err := os.ReadFile(store.publicIndexPath(key))
	if err != nil || !bytes.Equal(public, afterPublic) || !bytes.Equal(index, afterIndex) {
		t.Fatal("rollback discarded or relabeled completed media", err)
	}
	if result := finishDeferredMedia(t, store, key); len(result.Entries) != 3 {
		t.Fatal("retry duplicated or lost the completed media", result)
	}
}

func TestPublicMediaPreparingSuffixRollbackRetiresOnlyTheRemovedOccurrence(t *testing.T) {
	store, key, run, ctx := deferredMediaFixture(t)
	if err := store.AddMessage(ctx, key, "tool", "later file"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[later](artifact:later.pdf)"); err != nil {
		t.Fatal(err)
	}
	model, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetHistory(ctx, key, model[:2]); err != nil {
		t.Fatal(err)
	}
	result := finishDeferredMedia(t, store, key)
	if len(result.Entries) != 3 || result.Entries[2].AnchorID != result.Entries[1].ID ||
		result.Entries[2].Message.Content != "[report](artifact:immutable.pdf)" {
		t.Fatal("suffix rollback lost the retained delivery or revived its removed neighbor", result)
	}
}

func TestPublicMediaPreparingStructuralRewritePreservesCanonicalOrdering(t *testing.T) {
	store, key, _, ctx := deferredMediaFixture(t)
	model, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	model[0].Content = "edited request"
	if err = store.SetHistory(ctx, key, model); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReadPublicHistory(ctx, key)
	if err != nil || len(result.Entries) != 3 || result.Entries[1].Message.Content != "projection gap" ||
		result.Entries[2].AnchorID != result.Entries[1].ID {
		t.Fatal("structural rewrite reordered deferred media before its native occurrence", result, err)
	}
}

func TestPublicMediaPreparingMissingDerivedIndexDoesNotLoseOwnedTail(t *testing.T) {
	store, key, run, ctx := deferredMediaFixture(t)
	if err := os.Remove(store.publicIndexPath(key)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, key, "tool", "second delivery"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[second](artifact:second.pdf)"); err != nil {
		t.Fatal(err)
	}
	result := finishDeferredMedia(t, store, key)
	if len(result.Entries) != 5 || result.Entries[2].AnchorID != result.Entries[1].ID ||
		result.Entries[4].AnchorID != result.Entries[3].ID {
		t.Fatal("missing derived index lost native/media ordering", result)
	}
}

func TestPublicMediaPreparingRetainedPrefixDoesNotBecomeAuthorityForNewDelivery(t *testing.T) {
	store, key, run, ctx := deferredMediaFixture(t)
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err := store.SetHistory(
		ctx,
		key,
		[]providers.Message{{Role: "user", Content: "restored model window"}},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, key, "tool", "completed current delivery"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(ctx, key, run, "[second](artifact:second.pdf)"); err != nil {
		t.Fatal("retained old projection blocked a current native delivery", err)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.Skip != 2 || meta.PublicHistoryComplete || meta.PublicHistoryPreparation == nil {
		t.Fatal("retained source was promoted to coverage", meta, err)
	}
	readMeta := meta
	readMeta.PublicHistoryPreparation = nil
	entries, err := store.readPublicEntries(key, readMeta)
	if err != nil || len(entries) != 3 || entries[2].AnchorID == entries[1].AnchorID {
		t.Fatal("delivery borrowed old retained authority", entries, err)
	}
	if err = store.Compact(ctx, key); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("new delivery authorized retained RAW GC", err)
	}
}

func TestPublicMediaPreparingPublishedPrefixKeepsAppendGenerationAndDeliveryOrder(t *testing.T) {
	store, key, run := newTestStore(t), "agent:web:prefix-media", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "published anchor"); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadPublicHistory(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err = store.AddMessage(
		providers.WithHistoryRun(ctx, uuid.NewString()),
		key,
		"assistant",
		"later gap",
	); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"[first](artifact:first.pdf)", "[second](artifact:second.pdf)", "[second](artifact:second.pdf)"} {
		if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.PublicHistoryGeneration != before.Generation {
		t.Fatal("append rotated the existing published frontier", meta, err)
	}
	projector.failing = false
	result := finishDeferredMedia(t, store, key)
	if len(result.Entries) != 4 || result.Entries[1].Message.Content != "[first](artifact:first.pdf)" ||
		result.Entries[2].Message.Content != "[second](artifact:second.pdf)" {
		t.Fatal("prefix deliveries were reversed or duplicated", result)
	}
}
