package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestContentLineageUnavailableGCStillReturnsDurableReply(t *testing.T) {
	for _, loss := range []string{"witness", "unavailable", "unknown-version"} {
		for _, lostOrder := range []bool{false, true} {
			t.Run(loss+map[bool]string{false: "/valid-order", true: "/lost-order"}[lostOrder], func(t *testing.T) {
				al, store, dir, key := contentPendingLoop(t, false)
				pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.AcknowledgeHistoryCompaction(t.Context(), key, memory.ContentArchiveAck{
					DomainID: pending.DomainID, MutationID: pending.MutationID,
					BeforeState: pending.BeforeState, AfterState: pending.AfterState,
					Archived: pending.Dropped, ArchiveCommitID: uuid.NewString(),
				}); err != nil {
					t.Fatal(err)
				}
				meta, err := store.GetSessionMeta(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				ackDigest, sequence := meta.ContentIdentity.Last.AckDigest, meta.HistorySequence
				if loss == "witness" {
					if err = os.Remove(filepath.Join(dir, key+".jsonl.content-domain")); err != nil {
						t.Fatal(err)
					}
				} else {
					if loss == "unavailable" {
						meta.ContentIdentity.Unavailable = true
					} else {
						meta.ContentIdentity.Version++
					}
					data, encodeErr := json.Marshal(meta)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					if err = os.WriteFile(filepath.Join(dir, key+".meta.json"), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if lostOrder {
					if err = os.Remove(filepath.Join(dir, key+".jsonl.order-witness")); err != nil {
						t.Fatal(err)
					}
				}
				original, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				store, err = memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := session.NewJSONLBackend(store)
				al.registry.GetDefaultAgent().Sessions = backend
				response, turnErr := al.ProcessDirectWithChannel(
					t.Context(),
					"next user question",
					key,
					"test",
					"direct",
				)
				history := backend.GetHistory(key)
				if len(history) != 5 || history[3].Content != "next user question" || history[4].Content != "test" {
					t.Fatal("proof loss stopped durable content writes", history)
				}
				if turnErr != nil || response != "test" {
					t.Fatalf("optional GC hid an already durable reply: response=%q error=%v", response, turnErr)
				}
				physical, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
				if err != nil || !bytes.HasPrefix(physical, original) {
					t.Fatal("unproved GC removed original physical rows", err)
				}
				after, err := store.GetSessionMeta(t.Context(), key)
				if err != nil || after.ContentIdentity.DomainID != pending.DomainID || after.Skip != meta.Skip ||
					after.ContentIdentity.Last.AckDigest != ackDigest {
					t.Fatal("unproved Save reset domain, skip or ACK evidence", err)
				}
				wantSequence := sequence + 2
				if lostOrder {
					wantSequence = sequence
				}
				if after.HistorySequence != wantSequence || after.HistoryStreamID != meta.HistoryStreamID {
					t.Fatal("Save rebuilt or reset order evidence", after.HistorySequence, wantSequence)
				}
				frame, err := backend.ReadHistoryContentFrame(t.Context(), key)
				if err != nil || frame.IdentityStatus != memory.ContentIdentityUnavailable || frame.DomainID != "" {
					t.Fatal("successful Save reattested unavailable content proof", frame, err)
				}
				for _, entry := range frame.Entries {
					if entry.Ref != nil || (lostOrder && entry.ReceiptBinding != nil) {
						t.Fatal("successful Save invented a content ref or lost order receipt")
					}
				}
				if _, err = backend.ReadPendingHistoryCompaction(
					t.Context(),
					key,
				); !errors.Is(
					err,
					memory.ErrContentIdentityUnavailable,
				) {
					t.Fatal("proof status stopped exposing unavailability", err)
				}
				if _, err = backend.InitializeHistoryContentIdentity(
					t.Context(),
					key,
					frame.Observation,
				); !errors.Is(
					err,
					memory.ErrContentIdentityUnavailable,
				) {
					t.Fatal("successful Save permitted automatic replacement identity", err)
				}
			})
		}
	}
}
