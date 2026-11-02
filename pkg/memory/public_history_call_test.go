package memory

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

func TestPublicHistoryCallProofSurvivesCompactionButNeverAnEdit(t *testing.T) {
	const call = "pc-00112233445566778899aabb"
	dir, key, run := t.TempDir(), "agent:web:call-proof", uuid.NewString()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err = store.AddFullMessage(
		ctx,
		key,
		providers.Message{
			Role:          "assistant",
			Content:       "first",
			LLMCall:       &providers.LLMCall{RequestID: call},
			HistoryCallID: call,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err = store.AddFullMessage(
		ctx,
		key,
		providers.Message{Role: "assistant", Content: "neighbor", LLMCall: &providers.LLMCall{RequestID: call}},
	); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, store, key)
	before, err := store.ReadPublicHistory(ctx, key)
	if err != nil || len(before.Entries) != 2 || before.Entries[0].CallID != call || before.Entries[0].RunID != run ||
		before.Entries[0].Ref == nil || before.Entries[1].CallID != "" ||
		!messageutil.ValidSourceContentDigest(before.Entries[0].SourceDigest) {
		t.Fatalf("producer occurrence lacks call proof: %+v %v", before, err)
	}
	if err = store.Compact(ctx, key); err != nil {
		t.Fatal(err)
	}
	store, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadPublicHistory(ctx, key)
	if err != nil || after.Entries[0].CallID != call || !samePublicRef(after.Entries[0].Ref, before.Entries[0].Ref) ||
		after.Entries[0].SourceDigest != before.Entries[0].SourceDigest {
		t.Fatalf("compaction/restart changed producer proof: %+v %v", after, err)
	}
	frame, err := store.ReadHistoryContentFrame(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]ContentEntry(nil), frame.Entries...)
	entries[0].Message.Content = "edited"
	plan := ContentMutationPlan{
		MutationID:   uuid.NewString(),
		IntentDigest: contentDigest([]byte("edit first")),
		DomainID:     frame.DomainID,
		BeforeState:  frame.StateToken,
		Kind:         ContentMutationEdit,
		Edit:         &ContentMutationEditIntent{Entries: entries, Summary: frame.Summary},
	}
	mutation, err := store.CommitHistoryContentMutation(ctx, key, plan)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := store.ReadPublicHistory(ctx, key)
	if err != nil || edited.Entries[0].CallID != "" || samePublicRef(edited.Entries[0].Ref, before.Entries[0].Ref) ||
		edited.Entries[0].SourceDigest == before.Entries[0].SourceDigest {
		t.Fatalf("edit reused the producer call proof: %+v %v", edited, err)
	}
	if err = store.AcknowledgeHistoryContentMutation(ctx, key, typedAppliedAck(mutation.Transition)); err != nil {
		t.Fatal(err)
	}
	frame, err = store.ReadHistoryContentFrame(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	entries = append([]ContentEntry(nil), frame.Entries...)
	entries[1].Message.Content = "edited neighbor"
	plan = ContentMutationPlan{
		MutationID:   uuid.NewString(),
		IntentDigest: contentDigest([]byte("edit neighbor")),
		DomainID:     frame.DomainID,
		BeforeState:  frame.StateToken,
		Kind:         ContentMutationEdit,
		Edit:         &ContentMutationEditIntent{Entries: entries, Summary: frame.Summary},
	}
	mutation, err = store.CommitHistoryContentMutation(ctx, key, plan)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.ReadPublicHistory(ctx, key)
	if err != nil || repeated.Entries[0].CallID != "" ||
		repeated.Entries[0].SourceDigest != edited.Entries[0].SourceDigest {
		t.Fatalf("unchanged rewrite restored retired call proof: %+v %v", repeated, err)
	}
	if err = store.AcknowledgeHistoryContentMutation(ctx, key, typedAppliedAck(mutation.Transition)); err != nil {
		t.Fatal(err)
	}
	entry := PublicHistoryEntry{
		ID:          uuid.NewString(),
		ImportID:    "legacy:forged",
		AnchorID:    edited.Entries[0].ID,
		AnchorRef:   edited.Entries[0].Ref,
		AnchorOrder: edited.Entries[0].Order,
		CallID:      call,
		Message:     providers.Message{Role: "assistant", Content: "import"},
	}
	_, err = store.ImportPublicHistory(
		ctx,
		PublicHistoryImport{
			Key:        key,
			Generation: repeated.Generation,
			Revision:   repeated.Revision,
			AfterID:    entry.AnchorID,
			Entries:    []PublicHistoryEntry{entry},
		},
	)
	if !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("import forged producer call proof: %v", err)
	}
}
