package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func typedMutationSource(t *testing.T) (*JSONLStore, string, ContentFrame) {
	t.Helper()
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_typed_content"
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, role := range []string{"user", "assistant", "user", "assistant"} {
		if err = s.AddFullMessage(
			t.Context(),
			key,
			providers.Message{Role: role, Content: "same", CreatedAt: &at},
		); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SetSummary(t.Context(), key, "preserved summary"); err != nil {
		t.Fatal(err)
	}
	return s, key, initializedContent(t, s, key)
}

func typedPrefixPlan(frame ContentFrame, count int) ContentMutationPlan {
	return ContentMutationPlan{
		MutationID: uuid.NewString(), IntentDigest: contentDigest([]byte("owned resend admission")),
		DomainID: frame.DomainID, BeforeState: frame.StateToken, Kind: ContentMutationRetainPrefix,
		RetainPrefix: &ContentMutationRetainPrefixIntent{Retained: frameRefs(frame)[:count]},
	}
}

func typedAppliedAck(transition ContentMutationTransition) ContentMutationAppliedAck {
	return ContentMutationAppliedAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID, IntentDigest: transition.IntentDigest,
		Kind: transition.Kind, BeforeState: transition.BeforeState, AfterState: transition.AfterState,
		Before: append([]ContentRef(nil), transition.Before...), After: append([]ContentRef(nil), transition.After...),
		AppliedCommitID: uuid.NewString(),
	}
}

func TestContentMutationRetainPrefixFreezesPairAndReplaysAppliedWithoutPreimage(t *testing.T) {
	s, key, before := typedMutationSource(t)
	old, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	plan := typedPrefixPlan(before, 2)
	result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil || result.Status != ContentMutationPending || result.AppliedAck != nil {
		t.Fatal("checked prefix cut did not commit", result, err)
	}
	got, err := s.GetHistory(t.Context(), key)
	if err != nil || !reflect.DeepEqual(got, old[:2]) {
		t.Fatal("resend did not preserve the full exact prefix", got, err)
	}
	for _, side := range []ContentTransitionSide{ContentBefore, ContentAfter} {
		frame, readErr := s.ReadHistoryContentMutationFrame(t.Context(), key, plan.MutationID, side)
		count := 4
		if side == ContentAfter {
			count = 2
		}
		if readErr != nil || len(frame.Entries) != count || frame.Summary != before.Summary ||
			!sameContentRefs(frameRefs(frame), frameRefs(before)[:count]) {
			t.Fatal("lost immutable mutation side", side, frame, readErr)
		}
		for _, entry := range frame.Entries {
			if entry.ReceiptBinding != nil || entry.Message.HistoryID != "" || entry.Message.HistorySequence != 0 {
				t.Fatal("frozen pair reattested order", side, entry)
			}
		}
	}
	appended, err := s.AppendHistoryContent(t.Context(), key, providers.Message{Role: "user", Content: "replacement"})
	if err != nil || appended.Entry.Ref == nil || appended.Entry.ReceiptBinding == nil ||
		appended.Entry.Ref.OccurrenceID == before.Entries[2].Ref.OccurrenceID ||
		appended.Entry.ReceiptBinding.Order.Sequence <= old[3].HistorySequence {
		t.Fatal("replacement reused occurrence/order of removed prompt", appended, err)
	}
	if err = s.AddMessage(t.Context(), key, "assistant", "replacement reply"); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.ReadHistoryContentMutationFrame(t.Context(), key, plan.MutationID, ContentAfter)
	if err != nil || len(frozen.Entries) != 2 || frozen.StateToken != result.Transition.AfterState {
		t.Fatal("a later append substituted the frozen after view", frozen, err)
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	preimage := meta.ContentIdentity.Pending.Mutation.Before.Image
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil || !reflect.DeepEqual(replayed, result) {
		t.Fatal("cold retry did not replay the exact committed operation", replayed, err)
	}
	if err = reopened.AcknowledgeHistoryCompaction(t.Context(), key, ContentArchiveAck{
		DomainID: before.DomainID, MutationID: plan.MutationID, BeforeState: result.Transition.BeforeState,
		AfterState: result.Transition.AfterState, Archived: result.Transition.Before, ArchiveCommitID: uuid.NewString(),
	}); !errors.Is(err, ErrContentArchiveAck) {
		t.Fatal("archive ACK released a prefix-discard mutation", err)
	}
	ack := typedAppliedAck(result.Transition)
	if err = reopened.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.dir, contentPreimageName(key, preimage.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("qualified application did not retire its exact preimage", err)
	}
	if _, err = reopened.ReadHistoryContentMutationFrame(
		t.Context(),
		key,
		plan.MutationID,
		ContentBefore,
	); !errors.Is(
		err,
		ErrContentMutationNotRetained,
	) {
		t.Fatal("closed mutation still claimed a retained payload", err)
	}
	reopened, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := reopened.ReadLatestHistoryContentMutation(t.Context(), key)
	if err != nil || applied.Status != ContentMutationApplied || applied.AppliedAck == nil ||
		!reflect.DeepEqual(*applied.AppliedAck, ack) || !reflect.DeepEqual(applied.Transition, result.Transition) {
		t.Fatal("Last replay depended on GCed bytes or lost exact ACK", applied, err)
	}
	if err = reopened.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
		t.Fatal("cold ACK replay reopened retired bytes", err)
	}
	beforeRetry, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	appliedAgain, err := reopened.CommitHistoryContentMutation(t.Context(), key, plan)
	afterRetry, readErr := os.ReadFile(s.jsonlPath(key))
	if err != nil || readErr != nil || !reflect.DeepEqual(appliedAgain, applied) ||
		!bytes.Equal(beforeRetry, afterRetry) {
		t.Fatal("response-loss retry cut the source twice", appliedAgain, err, readErr)
	}
}

func TestContentMutationPrefixZeroAllPreserveDomainSummaryAndHighwater(t *testing.T) {
	for _, keep := range []int{0, 4} {
		t.Run(string(rune('0'+keep)), func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			old, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, keep))
			if err != nil {
				t.Fatal(err)
			}
			current, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || current.DomainID != frame.DomainID || current.Summary != frame.Summary ||
				len(current.Entries) != keep || !sameContentRefs(frameRefs(current), frameRefs(frame)[:keep]) {
				t.Fatal("prefix intent became clear or suffix compaction", current, err)
			}
			appended, err := s.AppendHistoryContent(
				t.Context(),
				key,
				providers.Message{Role: "user", Content: "new occurrence"},
			)
			if err != nil || appended.Receipt == nil || appended.Receipt.Order.Sequence <= old.HistorySequence ||
				appended.DomainID != result.Transition.DomainID {
				t.Fatal("tail removal rebuilt or reset the genuine highwater", appended, err)
			}
		})
	}
}

func TestContentMutationEditSeparatesRevisionAndGenuineReadOrder(t *testing.T) {
	for _, mode := range []string{"core", "metadata", "core_with_order_loss", "core_with_warm_order_loss"} {
		t.Run(mode, func(t *testing.T) {
			s, key, before := typedMutationSource(t)
			oldBinding := *before.Entries[1].ReceiptBinding
			if mode == "core_with_order_loss" || mode == "core_with_warm_order_loss" {
				if err := os.Remove(s.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
				if mode == "core_with_order_loss" {
					s.metaMu.Lock()
					delete(s.metaCache, key)
					delete(s.historyWitnesses, key)
					s.metaMu.Unlock()
				}
			}
			entries := append([]ContentEntry(nil), before.Entries...)
			if mode == "metadata" {
				entries[1].Message.ModelName = "different audit metadata"
			} else {
				entries[1].Message.Content = "checked changed answer"
			}
			// Untrusted old receipt/private fields cannot bind the changed core.
			entries[1].Message.HistorySequence = 1
			entries[1].ReceiptBinding = &oldBinding
			plan := ContentMutationPlan{
				MutationID: uuid.NewString(), IntentDigest: contentDigest([]byte(mode)), DomainID: before.DomainID,
				BeforeState: before.StateToken, Kind: ContentMutationEdit,
				Edit: &ContentMutationEditIntent{Entries: entries, Summary: before.Summary},
			}
			result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil {
				t.Fatal("checked edit lost content availability", err)
			}
			current, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || current.Entries[1].Ref.OccurrenceID != before.Entries[1].Ref.OccurrenceID ||
				current.Entries[1].Ref.RevisionID == before.Entries[1].Ref.RevisionID ||
				*current.Entries[0].Ref != *before.Entries[0].Ref || *current.Entries[2].Ref != *before.Entries[2].Ref {
				t.Fatal("checked revision selected a duplicate by body or changed unselected refs", current, err)
			}
			binding := current.Entries[1].ReceiptBinding
			switch mode {
			case "core":
				if binding == nil || binding.SourceID != oldBinding.SourceID ||
					binding.Order.StreamID != oldBinding.Order.StreamID || binding.Order.Sequence <= 4 {
					t.Fatal("old read proof was attached to a changed core", binding)
				}
			case "metadata":
				if binding == nil || *binding != oldBinding {
					t.Fatal("metadata-only revision invented or invalidated genuine Order", binding)
				}
			case "core_with_order_loss", "core_with_warm_order_loss":
				if binding != nil {
					t.Fatal("checked edit reconstructed lost Order", binding)
				}
				if _, err = s.ReadHistoryWithReceipts(t.Context(), key); err == nil {
					t.Fatal("content edit reopened lost receipt proof")
				}
				stored, readErr := s.readRecoveryMeta(key)
				if readErr != nil || stored.HistorySequence != 4 {
					t.Fatal("checked edit advanced an unavailable counter", stored.HistorySequence, readErr)
				}
			}
			frozen, err := s.ReadHistoryContentMutationFrame(
				t.Context(),
				key,
				result.Transition.MutationID,
				ContentBefore,
			)
			if err != nil || frozen.Entries[1].Message.Content != "same" || frozen.Entries[1].ReceiptBinding != nil {
				t.Fatal("edit lost its overwritten raw preimage or reattested proof", frozen, err)
			}
		})
	}
}

func TestContentMutationRejectsStaleAndNonPrefixWithoutPartialWrite(t *testing.T) {
	for _, change := range []string{"append", "summary", "nonprefix", "revision", "kind", "intent"} {
		t.Run(change, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			plan := typedPrefixPlan(frame, 2)
			switch change {
			case "append":
				if err := s.AddMessage(t.Context(), key, "assistant", "concurrent append"); err != nil {
					t.Fatal(err)
				}
			case "summary":
				if err := s.SetSummary(t.Context(), key, "concurrent summary"); err != nil {
					t.Fatal(err)
				}
			case "nonprefix":
				plan.RetainPrefix.Retained = []ContentRef{*frame.Entries[1].Ref, *frame.Entries[2].Ref}
			case "revision":
				plan.RetainPrefix.Retained[1].RevisionID = uuid.NewString()
			case "kind":
				plan.Kind = ContentMutationEdit
			case "intent":
				plan.IntentDigest = "body equality is not a command identity"
			}
			before, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CommitHistoryContentMutation(
				t.Context(),
				key,
				plan,
			); !errors.Is(
				err,
				ErrContentStateChanged,
			) {
				t.Fatal("ambiguous/stale selection was accepted", err)
			}
			after, metaErr := os.ReadFile(s.metaPath(key))
			bodyAfter, bodyErr := os.ReadFile(s.jsonlPath(key))
			if metaErr != nil || bodyErr != nil || !bytes.Equal(before, after) || !bytes.Equal(body, bodyAfter) {
				t.Fatal("failed selection partially changed content/summary", metaErr, bodyErr)
			}
		})
	}
}
