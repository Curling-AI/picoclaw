package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func pendingContent(t *testing.T, s *JSONLStore, key string) (ContentFrame, ContentCompactionPlan, ContentTransition) {
	t.Helper()
	for range 4 {
		if err := s.AddMessage(t.Context(), key, "user", "same"); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Retained:      refs[2:],
		SummaryInputs: refs[:2],
		Summary:       "summarized exact prefix",
	}
	transition, err := s.CommitHistoryCompaction(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	return frame, plan, transition
}

func TestContentLineagePendingRejectsClearUntilArchiveConfirmation(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_clear_pending"
	_, _, transition := pendingContent(t, s, key)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetHistory(t.Context(), key, nil); !errors.Is(err, ErrContentLineagePending) {
		t.Fatal("clear discarded an unarchived transition", err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("clear removed durable unarchived rows", err)
	}
	wrong := ContentArchiveAck{
		DomainID:        transition.DomainID,
		MutationID:      uuid.NewString(),
		BeforeState:     transition.BeforeState,
		AfterState:      transition.AfterState,
		Archived:        transition.Dropped,
		ArchiveCommitID: uuid.NewString(),
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, wrong); !errors.Is(err, ErrContentArchiveAck) {
		t.Fatal("another mutation released retention", err)
	}
}

func TestContentLineageOrderLossDoesNotLoseIndependentIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_order_loss"
	if err = s.AddMessage(t.Context(), key, "assistant", "before"); err != nil {
		t.Fatal(err)
	}
	before := initializedContent(t, s, key)
	if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
		t.Fatal(err)
	}
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || after.IdentityStatus != ContentIdentityKnown || *after.Entries[0].Ref != *before.Entries[0].Ref ||
		after.Entries[0].ReceiptBinding != nil {
		t.Fatal("lost order changed content identity or restored receipts", after, err)
	}
	entry, err := store.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{
			Role:     "assistant",
			Content:  "new",
			Delivery: &providers.Delivery{Origin: "cron:job:run-2", At: time.Now()},
		},
	)
	if err != nil || entry.Receipt != nil || entry.Entry.ReceiptBinding != nil || entry.Entry.Ref == nil ||
		!entry.Entry.Ref.Valid() ||
		entry.Entry.Message.Delivery.Origin != "cron:job:run-2" {
		t.Fatal("content append lost independent identity or forged order", entry, err)
	}
	if _, err = store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("independent identity restored order receipt API", err)
	}
}

func TestContentLineageLostMetadataNeverCreatesReplacementDomain(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_identity_loss"
	if err = s.AddMessage(t.Context(), key, "assistant", "durable"); err != nil {
		t.Fatal(err)
	}
	frame := initializedContent(t, s, key)
	meta, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentIdentity = nil
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	lost, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || lost.IdentityStatus != ContentIdentityUnavailable || lost.DomainID != "" ||
		len(lost.Entries) != 1 ||
		lost.Entries[0].Ref != nil {
		t.Fatal("lost identity was relabeled as an empty/new domain", lost, frame, err)
	}
	if _, err = s.InitializeHistoryContentIdentity(
		t.Context(),
		key,
		lost.Observation,
	); !errors.Is(
		err,
		ErrContentIdentityUnavailable,
	) {
		t.Fatal("known lost domain was automatically replaced", err)
	}
	if err = s.AddMessage(t.Context(), key, "assistant", "append remains available"); err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.GetHistory(t.Context(), key)
	if err != nil || len(ordinary) != 2 || !strings.Contains(ordinary[1].Content, "available") {
		t.Fatal("identity loss discarded content", ordinary, err)
	}
}

func TestContentLineageEmptyBaselineAcceptsFirstExactAppend(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_empty_baseline"
	frame := initializedContent(t, s, key)
	origin := &providers.Delivery{Origin: "cron:job:run-A", At: time.Now()}
	result, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "first", Delivery: origin},
	)
	if err != nil || result.Entry.Ref == nil || result.Entry.Ref.DomainID != frame.DomainID ||
		result.Entry.Message.Content != "first" {
		t.Fatal("empty content baseline broke its first append", result, err)
	}
	origin.Origin = "caller mutated later"
	if result.Entry.Message.Delivery.Origin != "cron:job:run-A" {
		t.Fatal("exact committed result retained a mutable caller pointer")
	}
}

func TestContentLineageCheckedGetterPreservesAliasIOAndSummaryOnlyBoundaries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_boundaries"
	if err = s.AddMessage(t.Context(), key, "assistant", "content"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertSessionMeta(t.Context(), key, nil, []string{"old-alias"}); err != nil {
		t.Fatal(err)
	}
	frame := initializedContent(t, s, key)
	for _, alias := range []string{"old-alias", " " + key} {
		if _, err = s.ReadHistoryContentFrame(t.Context(), alias); !errors.Is(err, ErrHistoryOwnershipUnavailable) {
			t.Fatal("checked content accepted alias/padding", alias, err)
		}
	}
	path := s.jsonlPath(key)
	if err = os.Rename(path, path+".private-test-backup"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadHistoryContentFrame(
		t.Context(),
		key,
	); err == nil ||
		errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("content I/O was reclassified as success or identity loss", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".private-test-backup", path); err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{
		MutationID:  uuid.NewString(),
		DomainID:    frame.DomainID,
		BeforeState: frame.StateToken,
		Retained:    refs,
		Summary:     "summary only, no drops",
	}
	transition, err := s.CommitHistoryCompaction(t.Context(), key, plan)
	if err != nil || len(transition.Dropped) != 0 || transition.AfterState == frame.StateToken {
		t.Fatal("summary-only projection required numeric archive growth", transition, err)
	}
	post, err := s.ReadHistoryCompactionFrame(t.Context(), key, transition.MutationID, ContentAfter)
	if err != nil || post.Summary != plan.Summary || len(post.Entries) != 1 || *post.Entries[0].Ref != refs[0] {
		t.Fatal("summary-only projection changed occurrence", post, err)
	}
	ack := ContentArchiveAck{
		DomainID:        transition.DomainID,
		MutationID:      transition.MutationID,
		BeforeState:     transition.BeforeState,
		AfterState:      transition.AfterState,
		ArchiveCommitID: uuid.NewString(),
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal("durably qualified summary-only projection could not be confirmed", err)
	}
}
