package memory

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentMutationPinsPublicViewInTheSameSourceCommit(t *testing.T) {
	for _, kind := range []string{"current", "generation", "revision", "imported_after_observation"} {
		t.Run(kind, func(t *testing.T) {
			store, key, before := typedMutationSource(t)
			page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			plan := typedPrefixPlan(before, 2)
			plan.PublicView = &ContentMutationPublicView{Generation: page.Generation, Revision: page.Revision}
			switch kind {
			case "generation":
				plan.PublicView.Generation = uuid.NewString()
			case "revision":
				plan.PublicView.Revision++
			case "imported_after_observation":
				anchor := page.Entries[0]
				_, importErr := store.ImportPublicHistory(t.Context(), PublicHistoryImport{
					Key: key, Generation: page.Generation,
					Revision: page.Revision, AfterID: anchor.ID, Entries: []PublicHistoryEntry{
						{
							ID:          uuid.NewString(),
							ImportID:    "legacy:observed-after",
							AnchorID:    anchor.ID,
							AnchorRef:   anchor.Ref,
							AnchorOrder: anchor.Order,
							Message:     providers.Message{Role: "assistant", Content: "legacy supplement"},
						},
					},
				})
				if importErr != nil {
					t.Fatal(importErr)
				}
			}
			result, err := store.CommitHistoryContentMutation(t.Context(), key, plan)
			if kind == "current" {
				if err != nil || result.Status != ContentMutationPending || len(result.Transition.After) != 2 {
					t.Fatal("current source view was refused", result, err)
				}
				return
			}
			if !errors.Is(err, ErrContentStateChanged) || result.Transition.Version != 0 {
				t.Fatal("a stale public precondition selected a destructive cut", result, err)
			}
			current, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || current.StateToken != before.StateToken || len(current.Entries) != 4 {
				t.Fatal("failed public precondition changed the native source", current, err)
			}
		})
	}
}
