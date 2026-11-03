package memory

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type transientPublicProjector struct{ failing bool }

func (p *transientPublicProjector) Commit(message providers.Message) (PublicHistoryProjectionResult, error) {
	if p.failing {
		return PublicHistoryProjectionResult{}, errors.New("temporary projection failure")
	}
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p *transientPublicProjector) Legacy(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p *transientPublicProjector) Verify(_ context.Context, message providers.Message) error {
	return nil
}

func TestPublicHistoryTransientProjectionHasBoundedExplicitRecovery(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_transient_projection"
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "confirmed before failure"); operationErr != nil {
		t.Fatal(operationErr)
	}
	projector := &transientPublicProjector{failing: true}
	s.SetPublicHistoryProjector(projector)
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "confirmed during failure"); operationErr != nil {
		t.Fatal("projection discarded canonical content", operationErr)
	}
	model, err := s.GetHistory(t.Context(), key)
	if err != nil || len(model) != 2 {
		t.Fatal("lost canonical source", model, err)
	}
	meta, err := s.readMeta(key)
	if err != nil || meta.PublicHistoryVersion < 0 || meta.PublicHistoryPreparation == nil {
		t.Fatal("projection failure became terminal", meta, err)
	}
	source, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	preparation, err := s.PreparePublicHistory(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; !preparation.Ready && step < 3; step++ {
		preparation, err = s.PreparePublicHistory(t.Context(), key, preparation.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || !preparation.Ready || len(history.Entries) != 2 {
		t.Fatal("explicit recovery failed or duplicated confirmed prefix", history, err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || string(source) != string(after) {
		t.Fatal("recovery rewrote canonical source", err)
	}
}

func TestPublicHistoryStructuralRecoveryKeepsCompactedPrefixAndSupplementOrder(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_structural_projection_recovery"
	run := newHistoryID()
	for _, text := range []string{"compacted", "retained"} {
		if err := store.AddMessage(providers.WithHistoryRun(t.Context(), run), key, "assistant", text); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordPublicMedia(t.Context(), key, run, "[version](artifact:confirmed.pdf)"); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	projector := &transientPublicProjector{failing: true}
	store.SetPublicHistoryProjector(projector)
	if err = store.AddMessage(t.Context(), key, "assistant", "later edit"); err != nil {
		t.Fatal(err)
	}
	projector.failing = false
	active, err := store.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	active[1].Content = "changed"
	if err = store.SetHistory(t.Context(), key, active); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(after.Entries) != 4 {
		t.Fatal("structural recovery lost confirmed history", after, err)
	}
	for index := range before.Entries {
		if after.Entries[index].ID != before.Entries[index].ID ||
			after.Entries[index].Message.Content != before.Entries[index].Message.Content {
			t.Fatal("structural recovery reordered or discarded prefix/supplement", before, after)
		}
	}
	if after.Entries[3].Message.Content != "changed" {
		t.Fatal("requested edit lost")
	}
}
