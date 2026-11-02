package memory

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
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
			LLMCall:       &providers.LLMCall{RequestID: "pc-ffeeddccbbaa998877665544"},
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
		before.Entries[0].Ref == nil || before.Entries[1].CallID != "" {
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
	if err != nil || after.Entries[0].CallID != call || !samePublicRef(after.Entries[0].Ref, before.Entries[0].Ref) {
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
	if err != nil || edited.Entries[0].CallID != "" || samePublicRef(edited.Entries[0].Ref, before.Entries[0].Ref) {
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
	if err != nil || repeated.Entries[0].CallID != "" {
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

func TestPublicHistoryCallSurvivesPhysicalCompactionBeforeIdentityInitialization(t *testing.T) {
	const call = "pc-00112233445566778899aabb"
	store, key := newTestStore(t), "agent:web:call-before-identity"
	if err := store.AddMessage(t.Context(), key, "user", "prefix"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{
			Role:          "assistant",
			Content:       "retained reply",
			HistoryCallID: call,
			LLMCall:       &providers.LLMCall{RequestID: call},
		},
	); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(before.Entries) != 2 || before.Entries[1].CallID != call {
		t.Fatal("fixture lacks native producer provenance", before, err)
	}
	if err = store.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(after.Entries) != 2 || after.Entries[1].ID != before.Entries[1].ID ||
		after.Entries[1].CallID != call ||
		after.Entries[1].Order != before.Entries[1].Order {
		t.Fatal("physical compaction lost native provenance of its retained occurrence", after, err)
	}
	initializedContent(t, store, key)
	promoted, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || promoted.Entries[1].Ref == nil || !promoted.Entries[1].Ref.Valid() ||
		promoted.Entries[1].CallID != call {
		t.Fatal("later content identity could not bind the native producer occurrence", promoted, err)
	}
}

func TestPublicHistoryDisplayRewriteDoesNotTransferCallToEditsOrAnotherOccurrence(t *testing.T) {
	for _, change := range []string{"edit", "occurrence", "generation"} {
		t.Run(change, func(t *testing.T) {
			const call = "pc-00112233445566778899aabb"
			store, key := newTestStore(t), "agent:web:call-retirement"
			if err := store.AddFullMessage(
				t.Context(),
				key,
				providers.Message{
					Role:          "assistant",
					Content:       "native reply",
					HistoryCallID: call,
					LLMCall:       &providers.LLMCall{RequestID: call},
				},
			); err != nil {
				t.Fatal(err)
			}
			expectedCount := 1
			if change == "generation" {
				if appendErr := store.AddMessage(t.Context(), key, "user", "aborted next prompt"); appendErr != nil {
					t.Fatal(appendErr)
				}
				expectedCount = 2
			}
			before, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil || len(before.Entries) != expectedCount || before.Entries[0].CallID != call {
				t.Fatal("fixture lacks native producer provenance", before, err)
			}
			messages, err := store.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			messages[0].HistoryCallID = call
			if change == "generation" {
				messages = messages[:1]
			} else if change == "edit" {
				messages[0].Content = "edited reply"
			} else {
				messages[0].HistoryID = uuid.NewString()
				changedAt := messages[0].CreatedAt.Add(time.Second)
				messages[0].CreatedAt = &changedAt
			}
			if err = store.SetHistory(t.Context(), key, messages); err != nil {
				t.Fatal(err)
			}
			after, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil || len(after.Entries) != 1 || after.Entries[0].ID != messages[0].HistoryID ||
				after.Entries[0].CallID != "" {
				t.Fatal("rewrite transferred a previous call to changed content or another occurrence", after, err)
			}
		})
	}
}
