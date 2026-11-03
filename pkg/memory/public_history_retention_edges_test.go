package memory

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestOptionalPublicVerificationRetainsLegacyAndPreparingSource(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "preparing"}[preparing], func(t *testing.T) {
			store, key := legacyPublicFixture(t, 3)
			if preparing {
				if _, err := store.PreparePublicHistory(t.Context(), key, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.TruncateHistory(t.Context(), key, 1); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			store.SetPublicHistoryProjector(
				unavailablePublicVerifier{err: &os.PathError{Op: "open", Path: "gone.pdf", Err: os.ErrNotExist}},
			)
			if err = store.CompactForSessionSave(t.Context(), key); err != nil {
				t.Fatal("unavailable optional artifact proof failed a committed turn", err)
			}
			if err = store.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("explicit compaction accepted unavailable artifact proof", err)
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || string(before) != string(after) {
				t.Fatal("unavailable artifact proof retired the source", err)
			}
		})
	}
}

func TestPublicHistoryPreparingDoesNotAcknowledgeUnprovenRetention(t *testing.T) {
	store, key := newTestStore(t), "sk_v1_preparing_retention"
	for _, text := range []string{"one", "two", "three"} {
		if err := store.AddFullMessage(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: text},
		); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, store, key)
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryVersion, meta.PublicHistoryRevision, meta.PublicHistoryBytes, meta.PublicHistoryCount = 0, 0, 0, 0
	meta.PublicHistoryGeneration, meta.PublicHistoryDigest, meta.PublicHistoryComplete = "", "", false
	if err = store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(store.publicHistoryPath(key)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PreparePublicHistory(t.Context(), key, ""); err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	transition, err := store.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		SummaryInputs: refs[:2], Retained: refs[2:], Summary: "summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	ack := ContentArchiveAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID, BeforeState: transition.BeforeState,
		AfterState: transition.AfterState, Archived: transition.Dropped, ArchiveCommitID: "archive-proof",
	}
	if err = store.AcknowledgeHistoryCompaction(t.Context(), key, ack); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("preparation acknowledged retention before public coverage exists", err)
	}
	if _, err = store.ReadPendingHistoryCompaction(t.Context(), key); err != nil {
		t.Fatal("refused ACK retired the pending source", err)
	}
	preparation, err := store.PreparePublicHistory(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; !preparation.Ready && step < 3; step++ {
		preparation, err = store.PreparePublicHistory(t.Context(), key, preparation.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !preparation.Ready {
		t.Fatal("source did not finish bounded preparation")
	}
	if err = store.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal("ready public coverage did not release the verified prefix", err)
	}
}
