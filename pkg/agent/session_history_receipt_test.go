package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type interleavedReceiptBackend struct {
	*session.JSONLBackend
	interleaved bool
}

func (backend *interleavedReceiptBackend) AppendHistoryReceipt(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.CommittedHistoryMessage, error) {
	receipt, err := backend.JSONLBackend.AppendHistoryReceipt(ctx, key, message)
	if err != nil || backend.interleaved {
		return receipt, err
	}
	backend.interleaved = true
	// B reaches the real source before A's observer is called. A must keep its
	// receipt; re-reading the source tail here would falsely observe B.
	_, err = backend.JSONLBackend.AppendHistoryReceipt(ctx, key, providers.Message{Role: "assistant", Content: "B"})
	return receipt, err
}

type receiptBeforeIngestManager struct {
	noopContextManager
	observed *bool
	t        *testing.T
}

func (manager *receiptBeforeIngestManager) Ingest(context.Context, *IngestRequest) error {
	if !*manager.observed {
		manager.t.Error("ingestion ran before the synchronous receipt observer")
	}
	return nil
}

func TestSessionHistoryReceiptObserverUsesExactCommittedMessageBeforeIngest(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &interleavedReceiptBackend{JSONLBackend: session.NewJSONLBackend(store)}
	agent.Sessions = backend
	key := "agent:main:receipt-observer"
	observed := false
	al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
	var actual []memory.CommittedHistoryMessage
	al.SetSessionHistoryReceiptObserver(func(sessionKey string, committed []memory.CommittedHistoryMessage) {
		if sessionKey != key {
			t.Error("wrong observer session", sessionKey)
		}
		observed = true
		actual = committed
	})
	if operationErr := al.writeMirrored(
		agent,
		key,
		[]providers.Message{{Role: "assistant", Content: "A"}},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	read, err := backend.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || len(actual) != 1 || actual[0].Message.Content != "A" ||
		actual[0].Order != read[0].Order || actual[0].Order.Sequence != 1 || read[1].Message.Content != "B" || read[1].Order.Sequence != 2 {
		t.Fatal("observer used B's tail instead of A's committed receipt", actual, read, err)
	}
	al.SetSessionHistoryReceiptObserver(nil)
}

func TestSessionHistoryReceiptObserverContainsOnlyRealSuccessfulBatchMembers(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := session.NewJSONLBackend(store)
	agent.Sessions = backend
	key := "agent:main:receipt-partial-batch"
	var actual []memory.CommittedHistoryMessage
	al.SetSessionHistoryReceiptObserver(
		func(_ string, committed []memory.CommittedHistoryMessage) { actual = committed },
	)
	invalidDate := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err = al.writeMirrored(agent, key, []providers.Message{
		{Role: "assistant", Content: "a"},
		{Role: "assistant", Content: "b", CreatedAt: &invalidDate},
		{Role: "assistant", Content: "c"},
	})
	if err == nil {
		t.Fatal("real serialization failure was hidden")
	}
	read, err := backend.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || len(actual) != 2 {
		t.Fatal("failed b changed the committed batch membership", actual, read, err)
	}
	if actual[0].Message.Content != "a" || actual[1].Message.Content != "c" ||
		actual[0].Order != read[0].Order || actual[1].Order != read[1].Order ||
		actual[0].Order.Sequence != 1 || actual[1].Order.Sequence != 2 {
		t.Fatal("failed b acquired a receipt, or successful a/c was lost", actual, read, err)
	}
}

func TestSessionHistoryReceiptObserverLegacySignalFollowsSave(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "saved"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			storage := t.TempDir()
			if fail {
				storage = filepath.Join(storage, "not-a-directory")
				if err := os.WriteFile(storage, []byte("block"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			agent.Sessions = session.NewSessionManager(storage)
			called := false
			const key = "legacy-receipt-notification"
			al.SetSessionHistoryReceiptObserver(func(_ string, committed []memory.CommittedHistoryMessage) {
				called = true
				if committed != nil {
					t.Error("legacy fallback fabricated committed message receipts", committed)
				}
				persisted := session.NewSessionManager(storage).GetHistory(key)
				if len(persisted) != 1 || persisted[0].Content != "legacy" {
					t.Error("legacy callback preceded durable Save", persisted)
				}
			})
			err := al.writeMirrored(agent, key, []providers.Message{{Role: "assistant", Content: "legacy"}})
			if fail && (err == nil || called) {
				t.Fatal("failed legacy Save was announced", err, called)
			}
			if !fail && (err != nil || !called) {
				t.Fatal("saved legacy history lost its amendment signal", err, called)
			}
		})
	}
}
