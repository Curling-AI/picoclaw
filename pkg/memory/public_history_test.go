package memory

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryKeepsConfirmedContentAcrossCompactionRestartAndEdit(t *testing.T) {
	dir := t.TempDir()
	store, storeErr := NewJSONLStore(dir)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	key := "agent:web:public-history"
	run := uuid.NewString()
	input := []providers.Message{
		{Role: "user", Content: "[SENDER id=person]\nShow a report", HistoryRunID: run},
		{
			Role:         "assistant",
			Content:      "Reading",
			ToolCalls:    []providers.ToolCall{{ID: "call", Name: "read_file"}},
			HistoryRunID: run,
		},
		{Role: "tool", Content: "report data", ToolCallID: "call", HistoryRunID: run},
		{Role: "assistant", Content: "```openui\nText(\"Report\")\n```", HistoryRunID: run},
	}
	for _, message := range input {
		if operationErr := store.AddFullMessage(
			providers.WithHistoryRun(t.Context(), run),
			key,
			message,
		); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	original, storeErr := store.ReadPublicHistory(t.Context(), key)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if !original.Complete || len(original.Entries) != 4 {
		t.Fatalf("unexpected initial coverage: %+v", original)
	}
	anchor := original.Entries[3]
	imported := PublicHistoryEntry{
		ID:          uuid.NewString(),
		ImportID:    "snapshot:media:1",
		RunID:       anchor.RunID,
		AnchorID:    anchor.ID,
		AnchorOrder: anchor.Order,
		Message:     providers.Message{Role: "assistant", Content: "[report](artifact:content-hash.pdf)"},
	}
	request := PublicHistoryImport{
		Key:        key,
		Generation: original.Generation,
		Revision:   original.Revision,
		AfterID:    anchor.ID,
		Entries:    []PublicHistoryEntry{imported},
	}
	result, storeErr := store.ImportPublicHistory(t.Context(), request)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	retry, storeErr := store.ImportPublicHistory(t.Context(), request)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if !reflect.DeepEqual(result, retry) {
		t.Fatal("import retry changed confirmed content")
	}
	if operationErr1 := store.TruncateHistory(t.Context(), key, 2); operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	if operationErr2 := store.Compact(t.Context(), key); operationErr2 != nil {
		t.Fatal(operationErr2)
	}
	reopened, storeErr := NewJSONLStore(dir)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	after, storeErr := reopened.ReadPublicHistory(t.Context(), key)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if !reflect.DeepEqual(after.Entries, result.Entries) {
		t.Fatal("compaction/restart lost public content")
	}
	model, storeErr := reopened.GetHistory(t.Context(), key)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if len(model) != 2 || strings.Contains(model[1].Content, "content-hash") {
		t.Fatalf("supplement changed model context: %+v", model)
	}
	model[1].Content = "Edited answer"
	if operationErr3 := reopened.SetHistory(t.Context(), key, model); operationErr3 != nil {
		t.Fatal(operationErr3)
	}
	edited, storeErr := reopened.ReadPublicHistory(t.Context(), key)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if len(edited.Entries) != 4 || edited.Entries[3].Message.Content != "Edited answer" {
		t.Fatalf("edit retained obsolete supplement: %+v", edited.Entries)
	}
	if edited.Entries[0].ID != original.Entries[0].ID || edited.Entries[3].ID != anchor.ID ||
		edited.Entries[3].Order == anchor.Order {
		t.Fatal("edit lost occurrence lineage or reused content authority")
	}
	if _, operationErr4 := reopened.ImportPublicHistory(
		t.Context(),
		request,
	); !errors.Is(
		operationErr4,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("stale import resurrected edited media: %v", operationErr4)
	}
}

func TestPublicHistoryImportRejectsAmbiguousIdentityAndPrivateFields(t *testing.T) {
	store, _ := NewJSONLStore(t.TempDir())
	key := "agent:web:import"
	if operationErr5 := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "answer"},
	); operationErr5 != nil {
		t.Fatal(operationErr5)
	}
	history, historyErr := store.ReadPublicHistory(t.Context(), key)
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	anchor := history.Entries[0]
	entry := PublicHistoryEntry{
		ID:          uuid.NewString(),
		ImportID:    "legacy:one",
		AnchorID:    anchor.ID,
		AnchorOrder: anchor.Order,
		Message:     providers.Message{Role: "assistant", Content: "supplement"},
	}
	request := PublicHistoryImport{
		Key:        key,
		Generation: history.Generation,
		Revision:   history.Revision,
		AfterID:    anchor.ID,
		Entries:    []PublicHistoryEntry{entry},
	}
	request.Entries[0].AnchorOrder.Sequence++
	if _, operationErr6 := store.ImportPublicHistory(
		t.Context(),
		request,
	); !errors.Is(
		operationErr6,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("accepted wrong parent proof: %v", operationErr6)
	}
	request.Entries[0] = entry
	request.Entries[0].Message.ReasoningContent = "private reasoning"
	if _, operationErr7 := store.ImportPublicHistory(
		t.Context(),
		request,
	); !errors.Is(
		operationErr7,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("accepted private import: %v", operationErr7)
	}
	request.Entries[0] = entry
	if _, operationErr8 := store.ImportPublicHistory(t.Context(), request); operationErr8 != nil {
		t.Fatal(operationErr8)
	}
	request.Entries[0].Message.Content = "changed retry"
	if _, operationErr9 := store.ImportPublicHistory(
		t.Context(),
		request,
	); !errors.Is(
		operationErr9,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("accepted conflicting retry: %v", operationErr9)
	}
	request.Entries = make([]PublicHistoryEntry, PublicHistoryImportMessages+1)
	if _, operationErr10 := store.ImportPublicHistory(
		t.Context(),
		request,
	); !errors.Is(
		operationErr10,
		ErrHistoryCapacity,
	) {
		t.Fatalf("accepted over-budget import: %v", operationErr10)
	}
}

func TestPublicHistoryExcludesPrivateContextAndRollsBackFailedCommit(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONLStore(dir)
	key := "agent:web:private"
	for _, message := range []providers.Message{{Role: "user", Content: "[System: async:spawn] private"}, {Role: "system", Content: "credentials"}, {Role: "assistant", Content: "public", ReasoningContent: "secret", SystemParts: []providers.ContentBlock{{Type: "text", Text: "private"}}}} {
		if operationErr11 := store.AddFullMessage(t.Context(), key, message); operationErr11 != nil {
			t.Fatal(operationErr11)
		}
	}
	history, historyErr12 := store.ReadPublicHistory(t.Context(), key)
	if historyErr12 != nil {
		t.Fatal(historyErr12)
	}
	if len(history.Entries) != 1 || history.Entries[0].Message.ReasoningContent != "" ||
		len(history.Entries[0].Message.SystemParts) != 0 {
		t.Fatal("private model data entered public transcript")
	}
	oldPublic, historyErr12 := os.ReadFile(store.publicHistoryPath(key))
	if historyErr12 != nil {
		t.Fatal(historyErr12)
	}
	fail := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && fail {
			fail = false
			return errors.New("controlled metadata failure")
		}
		return os.WriteFile(path, data, mode)
	}
	if operationErr13 := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "must not appear"},
	); operationErr13 == nil {
		t.Fatal("expected failed commit")
	}
	store.writeFile = nil
	reopened, historyErr12 := NewJSONLStore(dir)
	if historyErr12 != nil {
		t.Fatal(historyErr12)
	}
	after, historyErr12 := reopened.ReadPublicHistory(t.Context(), key)
	if historyErr12 != nil {
		t.Fatal(historyErr12)
	}
	currentPublic, historyErr12 := os.ReadFile(store.publicHistoryPath(key))
	if historyErr12 != nil {
		t.Fatal(historyErr12)
	}
	if !reflect.DeepEqual(after.Entries, history.Entries) || string(oldPublic) != string(currentPublic) {
		t.Fatal("failed write published ghost content")
	}
}

func TestPublicHistoryMediaRetainsVersionWithoutAuditAndDeletesWithConversation(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONLStore(dir)
	key := "agent:web:media"
	run := uuid.NewString()
	if operationErr14 := store.AddFullMessage(
		providers.WithHistoryRun(t.Context(), run),
		key,
		providers.Message{Role: "user", Content: "report"},
	); operationErr14 != nil {
		t.Fatal(operationErr14)
	}
	content := "[report](artifact:immutable-v1.pdf)"
	if operationErr15 := store.RecordPublicMedia(t.Context(), key, run, content); operationErr15 != nil {
		t.Fatal(operationErr15)
	}
	if operationErr16 := store.RecordPublicMedia(t.Context(), key, run, content); operationErr16 != nil {
		t.Fatal(operationErr16)
	}
	if operationErr17 := store.AddFullMessage(
		providers.WithHistoryRun(t.Context(), run),
		key,
		providers.Message{Role: "assistant", Content: "Done"},
	); operationErr17 != nil {
		t.Fatal(operationErr17)
	}
	model, _ := store.GetHistory(t.Context(), key)
	if len(model) != 2 {
		t.Fatal("media changed model history")
	}
	if operationErr18 := store.TruncateHistory(t.Context(), key, 1); operationErr18 != nil {
		t.Fatal(operationErr18)
	}
	if operationErr19 := store.Compact(t.Context(), key); operationErr19 != nil {
		t.Fatal(operationErr19)
	}
	reopened, _ := NewJSONLStore(dir)
	history, historyErr20 := reopened.ReadPublicHistory(t.Context(), key)
	if historyErr20 != nil {
		t.Fatal(historyErr20)
	}
	if len(history.Entries) != 3 || history.Entries[1].Message.Content != content ||
		history.Entries[1].AnchorID != history.Entries[0].ID {
		t.Fatalf("media identity/version missing after run expiry/restart: %+v", history)
	}
	if operationErr21 := reopened.DeleteSession(t.Context(), key); operationErr21 != nil {
		t.Fatal(operationErr21)
	}
	if _, operationErr22 := os.Stat(reopened.publicHistoryPath(key)); !os.IsNotExist(operationErr22) {
		t.Fatalf("deletion kept public content: %v", operationErr22)
	}
	if operationErr23 := reopened.AddFullMessage(
		t.Context(),
		key,
		providers.Message{Role: "user", Content: "new"},
	); operationErr23 != nil {
		t.Fatal(operationErr23)
	}
	fresh, historyErr20 := reopened.ReadPublicHistory(t.Context(), key)
	if historyErr20 != nil || len(fresh.Entries) != 1 {
		t.Fatalf("deleted content resurrected: %+v %v", fresh, historyErr20)
	}
}
