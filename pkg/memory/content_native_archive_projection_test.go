package memory

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestNativeArchiveVerifiesTransitiveAndBoundarySupplements(t *testing.T) {
	for _, link := range []string{"anchor", "boundary"} {
		t.Run(link, func(t *testing.T) {
			store, key, frame := nativeArchiveSource(t)
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			anchor := history.Entries[0]
			first := PublicHistoryEntry{
				ID:          uuid.NewString(),
				ImportID:    "legacy:first",
				AnchorID:    anchor.ID,
				AnchorRef:   anchor.Ref,
				AnchorOrder: anchor.Order,
				Message:     providers.Message{Role: "assistant", Content: "safe supplement"},
			}
			history, err = store.ImportPublicHistory(
				t.Context(),
				PublicHistoryImport{
					Key:        key,
					Generation: history.Generation,
					Revision:   history.Revision,
					BeforeID:   anchor.ID,
					Entries:    []PublicHistoryEntry{first},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			second := PublicHistoryEntry{
				ID:       uuid.NewString(),
				ImportID: "legacy:second",
				Message:  providers.Message{Role: "assistant", Content: "[missing](artifact:gone.pdf)"},
			}
			if link == "anchor" {
				second.AnchorID = first.ID
			}
			_, err = store.ImportPublicHistory(
				t.Context(),
				PublicHistoryImport{
					Key:        key,
					Generation: history.Generation,
					Revision:   history.Revision,
					BeforeID:   first.ID,
					Entries:    []PublicHistoryEntry{second},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CommitHistoryCompaction(t.Context(), key, nativeArchivePlan(frame))
			if !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("unverified linked supplement released archive", err)
			}
			pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
			after, frameErr := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || frameErr != nil || pending.Version != 0 || after.StateToken != before.StateToken {
				t.Fatal("unverified supplement published a pending cut", pending, err, frameErr)
			}
		})
	}
}

func TestNativeContentFrameDoesNotBorrowStaleProjection(t *testing.T) {
	store, key, _ := nativeArchiveSource(t)
	stale := providers.Message{Role: "assistant", Content: "confirmed", ModelName: "old model"}
	if operationErr := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{
			Role:                 "assistant",
			Content:              "confirmed",
			ModelName:            "new model",
			HistoryPublicMessage: &stale,
		},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityKnown {
		t.Fatal("stale optional display blocked raw frame", frame, err)
	}
	if frame.Entries[len(frame.Entries)-1].Message.HistoryPublicMessage != nil {
		t.Fatal("stale projection was borrowed by frame")
	}
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Summary:       "summary",
		SummaryInputs: refs,
	}
	if _, err := store.CommitHistoryCompaction(t.Context(), key, plan); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("stale projection released raw archive coverage", err)
	}
}

func TestNativeArchiveWithoutProjectorRefusesUnversionedRelativeArtifacts(t *testing.T) {
	for _, test := range []struct {
		content string
		local   bool
	}{
		{"[report](report.pdf)", true},
		{"[deck](scripts/deck.html)", true},
		{"[remote](https://example.com/report.pdf)", false},
		{"[section](#section)", false},
	} {
		t.Run(test.content, func(t *testing.T) {
			store, key, _ := nativeArchiveSource(t)
			if appendErr := store.AddMessage(t.Context(), key, "assistant", test.content); appendErr != nil {
				t.Fatal(appendErr)
			}
			frame, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			plan := ContentCompactionPlan{
				MutationID:    uuid.NewString(),
				DomainID:      frame.DomainID,
				BeforeState:   frame.StateToken,
				Summary:       "summary",
				SummaryInputs: frameRefs(frame),
			}
			_, err = store.CommitHistoryCompaction(t.Context(), key, plan)
			if test.local && !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("raw relative artifact received native archive ACK without immutable bytes", err)
			}
			if !test.local && err != nil {
				t.Fatal("nonlocal link blocked native archive", err)
			}
		})
	}
}
