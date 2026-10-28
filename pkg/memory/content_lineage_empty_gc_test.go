package memory

import (
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentLineageCoverageEmptyGCRetainsConfirmationAndOrderUntilExplicitClear(t *testing.T) {
	for _, loseOrder := range []bool{false, true} {
		t.Run(map[bool]string{false: "proved Order", true: "lost Order"}[loseOrder], func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_empty_gc_intent"
			before, _, ack := pendingCoverageContent(t, s, key, []providers.Message{
				{Role: "user", Content: "[System] private steering"},
				{Role: "system", Content: "private instructions"},
			}, 0)
			policy := legacyPublicContentPolicy()
			if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, policy); err != nil {
				t.Fatal(err)
			}
			confirmed, err := s.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if loseOrder {
				if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Compact(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			empty, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || empty.DomainID != before.DomainID || len(empty.Entries) != 0 {
				t.Fatal("physical empty GC became an explicit clear", empty, err)
			}
			current, err := s.GetSessionMeta(t.Context(), key)
			if err != nil || current.ContentIdentity.Last == nil ||
				current.ContentIdentity.Last.AckDigest != confirmed.ContentIdentity.Last.AckDigest ||
				current.HistoryStreamID != confirmed.HistoryStreamID || current.HistorySequence != confirmed.HistorySequence {
				t.Fatal("empty GC discarded durable confirmation or reset Order", current, err)
			}
			if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, policy); err != nil {
				t.Fatal("lost ACK response could not replay after empty GC and reopen", err)
			}
			appended, err := s.AppendHistoryContent(t.Context(), key, providers.Message{
				Role:     "assistant",
				Content:  "new durable content",
				Delivery: &providers.Delivery{Origin: "cron:job:after-empty-gc"},
			})
			if err != nil || appended.Entry.Ref == nil || appended.DomainID != before.DomainID ||
				appended.BeforeState != empty.StateToken {
				t.Fatal("append after physical GC changed domain or lost its exact interval", appended, err)
			}
			if loseOrder {
				meta, readErr := s.GetSessionMeta(t.Context(), key)
				if readErr != nil || appended.Receipt != nil || appended.Entry.ReceiptBinding != nil ||
					meta.HistoryStreamID != confirmed.HistoryStreamID || meta.HistorySequence != confirmed.HistorySequence ||
					!meta.HistoryRecoveryRequired {
					t.Fatal("append after GC invented lost Order", appended, meta, readErr)
				}
			} else if appended.Receipt == nil || appended.Receipt.Order.StreamID != confirmed.HistoryStreamID ||
				appended.Receipt.Order.Sequence != confirmed.HistorySequence+1 {
				t.Fatal("append after empty GC restarted a valid Order stream", appended)
			}
			if err = s.SetHistory(t.Context(), key, nil); err != nil {
				t.Fatal("explicit legacy clear lost its existing contract", err)
			}
			cleared, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || cleared.DomainID == before.DomainID || cleared.DomainID == "" ||
				len(cleared.Entries) != 0 {
				t.Fatal("explicit clear did not retire the prior content domain", cleared, err)
			}
			if err = s.AcknowledgeHistoryCompactionCoverage(
				t.Context(),
				key,
				ack,
				policy,
			); !errors.Is(
				err,
				ErrContentArchiveAck,
			) {
				t.Fatal("old confirmation revived a cleared domain", err)
			}
		})
	}
}
