package memory

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func contentAPI(t *testing.T, s *JSONLStore) HistoryContentLineage {
	t.Helper()
	api, ok := any(s).(HistoryContentLineage)
	if !ok {
		t.Fatal("JSONL producer has no independent content lineage capability")
	}
	return api
}

func initializedContent(t *testing.T, s *JSONLStore, key string) ContentFrame {
	t.Helper()
	api := contentAPI(t, s)
	before, err := api.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := api.InitializeHistoryContentIdentity(t.Context(), key, before.Observation)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func frameRefs(frame ContentFrame) []ContentRef {
	refs := make([]ContentRef, len(frame.Entries))
	for i, entry := range frame.Entries {
		if entry.Ref != nil {
			refs[i] = *entry.Ref
		}
	}
	return refs
}

func TestContentLineageDuplicateOccurrencesAndAtomicCompaction(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_duplicates"
	for range 4 {
		if err = s.AddFullMessage(t.Context(), key, providers.Message{Role: "user", Content: "same"}); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	for i := range refs {
		if refs[i].OccurrenceID == "" {
			t.Fatal("missing identity")
		}
		for j := 0; j < i; j++ {
			if refs[i] == refs[j] {
				t.Fatal("duplicates share identity")
			}
		}
	}
	api := contentAPI(t, s)
	plan := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Retained:      refs[2:],
		SummaryInputs: refs[:2],
		Summary:       "exact prefix",
	}
	transition, err := api.CommitHistoryCompaction(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := api.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 2 || actual.Summary != "exact prefix" || !reflect.DeepEqual(frameRefs(actual), refs[2:]) {
		t.Fatal("summary/window/refs were not committed together", actual, len(ordinary))
	}
	if transition.BeforeState != frame.StateToken || transition.AfterState != actual.StateToken ||
		!reflect.DeepEqual(transition.Dropped, refs[:2]) {
		t.Fatal("wrong exact transition", transition)
	}
	frozen, err := api.ReadHistoryCompactionFrame(t.Context(), key, plan.MutationID, ContentBefore)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frameRefs(frozen), refs) || frozen.Summary != frame.Summary {
		t.Fatal("preimage lost", frozen)
	}
}

func TestContentLineageReadDoesNotRepairProofLoss(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_read_only"
	if err = s.AddMessage(t.Context(), key, "user", "durable"); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	api := contentAPI(t, s)
	frame, err := api.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	metaAfter, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || string(metadata) != string(metaAfter) {
		t.Fatal("getter wrote recovery/identity state")
	}
	if frame.IdentityStatus != ContentIdentityUninitialized || len(frame.Entries) != 1 || frame.Entries[0].Ref != nil ||
		frame.Entries[0].ReceiptBinding != nil {
		t.Fatal("getter invented identity or receipt", frame)
	}
	if frame.Entries[0].Message.HistoryID != "" || frame.Entries[0].Message.HistorySequence != 0 {
		t.Fatal("private proof leaked")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = api.ReadHistoryContentFrame(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation hidden", err)
	}
}

func TestContentLineageCASRejectsAppendAndNonSuffix(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_cas"
	for range 4 {
		if err = s.AddMessage(t.Context(), key, "user", "repeat"); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	api := contentAPI(t, s)
	bad := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Retained:      []ContentRef{refs[0], refs[3]},
		SummaryInputs: refs[1:3],
		Summary:       "must not commit",
	}
	if _, err = api.CommitHistoryCompaction(t.Context(), key, bad); err == nil {
		t.Fatal("non-contiguous selection accepted as a suffix")
	}
	if err = s.AddMessage(t.Context(), key, "assistant", "late"); err != nil {
		t.Fatal(err)
	}
	stale := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Retained:      refs[2:],
		SummaryInputs: refs[:2],
		Summary:       "stale",
	}
	if _, err = api.CommitHistoryCompaction(t.Context(), key, stale); !errors.Is(err, ErrContentStateChanged) {
		t.Fatal("late write did not invalidate CAS", err)
	}
	actual, err := api.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Entries) != 5 || actual.Summary != "" {
		t.Fatal("stale summary/cut discarded a late write", actual)
	}
}
