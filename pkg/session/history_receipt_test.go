package session_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestJSONLBackendReceiptReadPreservesPrivateProofAndProviderHistoryDoesNot(t *testing.T) {
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := session.NewJSONLBackend(store)
	key := "agent:main:backend-receipt"
	receipt, err := backend.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "confirmed"})
	if err != nil || !receipt.Order.Valid() {
		t.Fatal(receipt, err)
	}
	read, err := backend.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 1 || read[0].Order != receipt.Order || read[0].Message.HistoryID != receipt.Message.HistoryID {
		t.Fatal("backend lost the private receipt", read, err)
	}
	projected := backend.GetHistory(key)
	if len(projected) != 1 || projected[0].HistoryID != "" || projected[0].HistoryRunID != "" ||
		projected[0].HistoryOrderVersion != 0 || projected[0].HistoryStreamID != "" || projected[0].HistorySequence != 0 {
		t.Fatal("provider history retained persistence provenance", projected)
	}
	encoded, err := json.Marshal(projected)
	if err != nil || strings.Contains(string(encoded), receipt.Order.StreamID) || strings.Contains(string(encoded), "history_") {
		t.Fatal("private proof leaked into provider serialization", string(encoded), err)
	}
}

func TestJSONLBackendReceiptUnsupportedNeverFabricatesProof(t *testing.T) {
	backend := session.NewJSONLBackend(fullDiskStore{})
	receipt, err := backend.AppendHistoryReceipt(t.Context(), "unsupported", providers.Message{Role: "assistant", Content: "unconfirmed"})
	if !errors.Is(err, memory.ErrHistoryUnsupported) || receipt.Order.Valid() {
		t.Fatal("unsupported store fabricated a receipt", receipt, err)
	}
	if _, err := backend.ReadHistoryWithReceipts(t.Context(), "unsupported"); !errors.Is(err, memory.ErrHistoryUnsupported) {
		t.Fatal("unsupported store claimed ordering evidence", err)
	}
}
