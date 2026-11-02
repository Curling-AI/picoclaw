package memory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func publicImportBudgetFixture(t *testing.T) (*JSONLStore, PublicHistoryImport) {
	t.Helper()
	store, storeErr := NewJSONLStore(t.TempDir())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	key := "agent:web:budget"
	if operationErr := store.AddMessage(t.Context(), key, "user", "anchor"); operationErr != nil {
		t.Fatal(operationErr)
	}
	history, storeErr := store.ReadPublicHistory(t.Context(), key)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	anchor := history.Entries[0]
	entry := PublicHistoryEntry{
		ID:          uuid.NewString(),
		ImportID:    uuid.NewString(),
		AnchorID:    anchor.ID,
		AnchorOrder: anchor.Order,
		Message:     providers.Message{Role: "assistant"},
	}
	return store, PublicHistoryImport{
		Key:        key,
		Generation: history.Generation,
		Revision:   history.Revision,
		AfterID:    anchor.ID,
		Entries:    []PublicHistoryEntry{entry},
	}
}

func TestPublicHistoryBudgetsMeasurePublicMessageAndCompleteBatch(t *testing.T) {
	t.Run("message exact and excess", func(t *testing.T) {
		store, request := publicImportBudgetFixture(t)
		baseline, _ := json.Marshal(request.Entries[0].Message)
		request.Entries[0].Message.Content = strings.Repeat("x", PublicHistoryMessageBytes-len(baseline))
		public, _ := json.Marshal(request.Entries[0].Message)
		private, _ := json.Marshal(request.Entries[0])
		if len(public) != PublicHistoryMessageBytes || len(private) <= PublicHistoryMessageBytes {
			t.Fatal("invalid exact public/private boundary fixture")
		}
		if _, operationErr1 := store.ImportPublicHistory(t.Context(), request); operationErr1 != nil {
			t.Fatalf("1MiB public message rejected due to private envelope: %v", operationErr1)
		}
		request.Entries[0].Message.Content += "x"
		if _, operationErr2 := store.ImportPublicHistory(
			t.Context(),
			request,
		); !errors.Is(
			operationErr2,
			ErrHistoryCapacity,
		) {
			t.Fatalf("accepted oversized public message: %v", operationErr2)
		}
	})
	t.Run("50 and51 entries", func(t *testing.T) {
		store, request := publicImportBudgetFixture(t)
		prototype := request.Entries[0]
		request.Entries = make([]PublicHistoryEntry, PublicHistoryImportMessages)
		for index := range request.Entries {
			entry := prototype
			entry.ID = uuid.NewString()
			entry.ImportID = entry.ID
			entry.Message.Content = "row"
			request.Entries[index] = entry
		}
		if _, operationErr3 := store.ImportPublicHistory(t.Context(), request); operationErr3 != nil {
			t.Fatalf("50 rows rejected: %v", operationErr3)
		}
		request.Entries = append(request.Entries, prototype)
		if _, operationErr4 := store.ImportPublicHistory(
			t.Context(),
			request,
		); !errors.Is(
			operationErr4,
			ErrHistoryCapacity,
		) {
			t.Fatalf("51 rows accepted: %v", operationErr4)
		}
	})
	t.Run("2MiB complete operation", func(t *testing.T) {
		store, request := publicImportBudgetFixture(t)
		prototype := request.Entries[0]
		request.Entries = make([]PublicHistoryEntry, 3)
		for index := range request.Entries {
			entry := prototype
			entry.ID = uuid.NewString()
			entry.ImportID = entry.ID
			entry.Message.Content = strings.Repeat("x", 600<<10)
			request.Entries[index] = entry
		}
		payload, _ := json.Marshal(request)
		request.Entries[2].Message.Content += strings.Repeat("x", PublicHistoryOperationBytes-len(payload))
		payload, _ = json.Marshal(request)
		if len(payload) != PublicHistoryOperationBytes {
			t.Fatal("invalid complete-operation boundary fixture")
		}
		if _, operationErr5 := store.ImportPublicHistory(t.Context(), request); operationErr5 != nil {
			t.Fatalf("2MiB operation rejected: %v", operationErr5)
		}
		request.Entries[2].Message.Content += "x"
		if _, operationErr6 := store.ImportPublicHistory(
			t.Context(),
			request,
		); !errors.Is(
			operationErr6,
			ErrHistoryCapacity,
		) {
			t.Fatalf("oversized complete operation accepted: %v", operationErr6)
		}
	})
}
