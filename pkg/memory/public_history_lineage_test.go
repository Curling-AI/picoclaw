package memory

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryUsesProducerRefsAndRetiresOnlyRevisedSupplements(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, runA, runB := "sk_v1_public_lineage", uuid.NewString(), uuid.NewString()
	for _, message := range []providers.Message{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer A"}} {
		if err = store.AddFullMessage(providers.WithHistoryRun(t.Context(), runA), key, message); err != nil {
			t.Fatal(err)
		}
	}
	initializedContent(t, store, key)
	if err = store.RecordPublicMedia(t.Context(), key, runA, "[A](artifact:version-a.pdf)"); err != nil {
		t.Fatal(err)
	}
	appended, err := store.AppendHistoryContent(
		providers.WithHistoryRun(t.Context(), runB),
		key,
		providers.Message{Role: "assistant", Content: "answer B"},
	)
	if err != nil || appended.Entry.Ref == nil {
		t.Fatal("qualified append lost producer identity", err)
	}
	if err = store.RecordPublicMedia(t.Context(), key, runB, "[B](artifact:version-b.pdf)"); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(before.Entries) != 5 {
		t.Fatal("unexpected initial public source", before, err)
	}
	if !samePublicRef(before.Entries[3].Ref, appended.Entry.Ref) || before.Entries[3].RunID != runB ||
		!samePublicRef(before.Entries[4].AnchorRef, appended.Entry.Ref) {
		t.Fatal("display source reconstructed ref/run instead of using the producer")
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]ContentEntry(nil), frame.Entries...)
	entries[2].Message.Content = "edited B"
	plan := ContentMutationPlan{
		MutationID: uuid.NewString(), IntentDigest: contentDigest([]byte("edit B")),
		DomainID: frame.DomainID, BeforeState: frame.StateToken, Kind: ContentMutationEdit,
		Edit: &ContentMutationEditIntent{Entries: entries, Summary: frame.Summary},
	}
	mutation, err := store.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(after.Entries) != 4 || after.Entries[2].Message.Content != before.Entries[2].Message.Content ||
		after.Entries[3].Message.Content != "edited B" {
		t.Fatal("edit lost unrelated media or retained obsolete media", after, err)
	}
	oldRef, newRef := before.Entries[3].Ref, after.Entries[3].Ref
	if oldRef == nil || newRef == nil || oldRef.OccurrenceID != newRef.OccurrenceID ||
		oldRef.RevisionID == newRef.RevisionID {
		t.Fatal("checked edit lost occurrence lineage or reused its revision")
	}
	if err = store.AcknowledgeHistoryContentMutation(
		t.Context(),
		key,
		typedAppliedAck(mutation.Transition),
	); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CommitHistoryContentMutation(t.Context(), key, plan); err != nil {
		t.Fatal("checked edit replay did not remain idempotent", err)
	}
	model, err := store.GetHistory(t.Context(), key)
	if err != nil || len(model) != 3 || model[2].Content != "edited B" {
		t.Fatal("public supplements entered model context", model, err)
	}
}

func TestPublicHistoryImportRetrySurvivesProducerIdentityActivation(t *testing.T) {
	store, request := publicImportBudgetFixture(t)
	request.Entries[0].Message.Content = "confirmed legacy supplement"
	if _, err := store.ImportPublicHistory(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, store, request.Key)
	before, err := store.ReadPublicHistory(t.Context(), request.Key)
	if err != nil || before.Entries[1].AnchorRef == nil {
		t.Fatal("activation did not bind the existing supplement", before, err)
	}
	after, err := store.ImportPublicHistory(t.Context(), request)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("identity activation made an identical import retry conflict", after, err)
	}
}

func TestPublicHistoryProvesPrefixBeforeRetentionAndPhysicalGCWithoutSQL(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_public_gc"
	for _, message := range []providers.Message{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}, {Role: "assistant", Content: "retained"}} {
		if err = store.AddFullMessage(t.Context(), key, message); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, store, key)
	before, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	transition, err := store.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		SummaryInputs: refs[:2],
		Retained:      refs[2:],
		Summary:       "summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	ack := ContentArchiveAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID, BeforeState: transition.BeforeState,
		AfterState: transition.AfterState, Archived: transition.Dropped, ArchiveCommitID: "local-public-source-proof",
	}
	public, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(store.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	corruptPublic := func() {
		t.Helper()
		if err = os.WriteFile(store.publicHistoryPath(key), []byte("corrupt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restorePublic := func() {
		t.Helper()
		if err = os.WriteFile(store.publicHistoryPath(key), public, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	corruptPublic()
	if err = store.AcknowledgeHistoryCompaction(t.Context(), key, ack); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("retention was released without intact public coverage", err)
	}
	restorePublic()
	if err = store.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal(err)
	}
	corruptPublic()
	if err = store.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("GC trusted an earlier ACK after public bytes were lost", err)
	}
	retained, err := os.ReadFile(store.jsonlPath(key))
	if err != nil || string(retained) != string(physical) {
		t.Fatal("refused GC discarded the original source", err)
	}
	restorePublic()
	if err = store.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(reopened.jsonlPath(key), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"role":"assistant","content":"unconfirmed tail"}`); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil || !reflect.DeepEqual(after.Entries, before.Entries) {
		t.Fatal("restart/GC/tail changed confirmed display content", after, err)
	}
	fromVolume, err := ReadPublicHistoryFS(t.Context(), os.DirFS(store.dir), key)
	if err != nil || !reflect.DeepEqual(fromVolume, after) {
		t.Fatal("read-only volume disagreed with the pod's confirmed frontier", fromVolume, err)
	}
	if _, err = ReadPublicHistoryFS(t.Context(), os.DirFS(store.dir), "sk_v1_other_owner"); !os.IsNotExist(err) {
		t.Fatal("read-only source crossed its owner", err)
	}
}
