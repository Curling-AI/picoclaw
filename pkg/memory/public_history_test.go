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
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:web:public-history"
	run := uuid.NewString()
	input := []providers.Message{
		{Role: "user", Content: "[SENDER id=person]\nShow a report", HistoryRunID: run},
		{Role: "assistant", Content: "Reading", ToolCalls: []providers.ToolCall{{ID: "call", Name: "read_file"}}, HistoryRunID: run},
		{Role: "tool", Content: "report data", ToolCallID: "call", HistoryRunID: run},
		{Role: "assistant", Content: "```openui\nText(\"Report\")\n```", HistoryRunID: run},
	}
	for _, message := range input {
		if err := store.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, message); err != nil {
			t.Fatal(err)
		}
	}
	original, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Complete || len(original.Entries) != 4 {
		t.Fatalf("unexpected initial coverage: %+v", original)
	}
	anchor := original.Entries[3]
	imported := PublicHistoryEntry{ID: uuid.NewString(), ImportID: "snapshot:media:1", RunID: anchor.RunID, AnchorID: anchor.ID, AnchorOrder: anchor.Order, Message: providers.Message{Role: "assistant", Content: "[report](artifact:content-hash.pdf)"}}
	request := PublicHistoryImport{Key: key, Generation: original.Generation, Revision: original.Revision, AfterID: anchor.ID, Entries: []PublicHistoryEntry{imported}}
	result, err := store.ImportPublicHistory(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.ImportPublicHistory(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, retry) {
		t.Fatal("import retry changed confirmed content")
	}
	if err := store.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Entries, result.Entries) {
		t.Fatal("compaction/restart lost public content")
	}
	model, err := reopened.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(model) != 2 || strings.Contains(model[1].Content, "content-hash") {
		t.Fatalf("supplement changed model context: %+v", model)
	}
	model[1].Content = "Edited answer"
	if err := reopened.SetHistory(t.Context(), key, model); err != nil {
		t.Fatal(err)
	}
	edited, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Entries) != 4 || edited.Entries[3].Message.Content != "Edited answer" {
		t.Fatalf("edit retained obsolete supplement: %+v", edited.Entries)
	}
	if edited.Entries[0].ID != original.Entries[0].ID || edited.Entries[3].ID != anchor.ID || edited.Entries[3].Order == anchor.Order {
		t.Fatal("edit lost occurrence lineage or reused content authority")
	}
	if _, err := reopened.ImportPublicHistory(t.Context(), request); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("stale import resurrected edited media: %v", err)
	}
}

func TestPublicHistoryImportRejectsAmbiguousIdentityAndPrivateFields(t *testing.T) {
	store, _ := NewJSONLStore(t.TempDir())
	key := "agent:web:import"
	if err := store.AddFullMessage(t.Context(), key, providers.Message{Role: "assistant", Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	anchor := history.Entries[0]
	entry := PublicHistoryEntry{ID: uuid.NewString(), ImportID: "legacy:one", AnchorID: anchor.ID, AnchorOrder: anchor.Order, Message: providers.Message{Role: "assistant", Content: "supplement"}}
	request := PublicHistoryImport{Key: key, Generation: history.Generation, Revision: history.Revision, AfterID: anchor.ID, Entries: []PublicHistoryEntry{entry}}
	request.Entries[0].AnchorOrder.Sequence++
	if _, err := store.ImportPublicHistory(t.Context(), request); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("accepted wrong parent proof: %v", err)
	}
	request.Entries[0] = entry
	request.Entries[0].Message.ReasoningContent = "private reasoning"
	if _, err := store.ImportPublicHistory(t.Context(), request); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("accepted private import: %v", err)
	}
	request.Entries[0] = entry
	if _, err := store.ImportPublicHistory(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Entries[0].Message.Content = "changed retry"
	if _, err := store.ImportPublicHistory(t.Context(), request); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("accepted conflicting retry: %v", err)
	}
	request.Entries = make([]PublicHistoryEntry, PublicHistoryImportMessages+1)
	if _, err := store.ImportPublicHistory(t.Context(), request); !errors.Is(err, ErrHistoryCapacity) {
		t.Fatalf("accepted over-budget import: %v", err)
	}
}

func TestPublicHistoryExcludesPrivateContextAndRollsBackFailedCommit(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONLStore(dir)
	key := "agent:web:private"
	for _, message := range []providers.Message{{Role: "user", Content: "[System: async:spawn] private"}, {Role: "system", Content: "credentials"}, {Role: "assistant", Content: "public", ReasoningContent: "secret", SystemParts: []providers.ContentBlock{{Type: "text", Text: "private"}}}} {
		if err := store.AddFullMessage(t.Context(), key, message); err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Entries) != 1 || history.Entries[0].Message.ReasoningContent != "" || len(history.Entries[0].Message.SystemParts) != 0 {
		t.Fatal("private model data entered public transcript")
	}
	oldPublic, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && fail {
			fail = false
			return errors.New("controlled metadata failure")
		}
		return os.WriteFile(path, data, mode)
	}
	if err := store.AddFullMessage(t.Context(), key, providers.Message{Role: "assistant", Content: "must not appear"}); err == nil {
		t.Fatal("expected failed commit")
	}
	store.writeFile = nil
	reopened, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	currentPublic, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
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
	if err := store.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, providers.Message{Role: "user", Content: "report"}); err != nil {
		t.Fatal(err)
	}
	content := "[report](artifact:immutable-v1.pdf)"
	if err := store.RecordPublicMedia(t.Context(), key, run, content); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(t.Context(), key, run, content); err != nil {
		t.Fatal(err)
	}
	if err := store.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, providers.Message{Role: "assistant", Content: "Done"}); err != nil {
		t.Fatal(err)
	}
	model, _ := store.GetHistory(t.Context(), key)
	if len(model) != 2 {
		t.Fatal("media changed model history")
	}
	if err := store.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	reopened, _ := NewJSONLStore(dir)
	history, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Entries) != 3 || history.Entries[1].Message.Content != content || history.Entries[1].AnchorID != history.Entries[0].ID {
		t.Fatalf("media identity/version missing after run expiry/restart: %+v", history)
	}
	if err := reopened.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reopened.publicHistoryPath(key)); !os.IsNotExist(err) {
		t.Fatalf("deletion kept public content: %v", err)
	}
	if err := reopened.AddFullMessage(t.Context(), key, providers.Message{Role: "user", Content: "new"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := reopened.ReadPublicHistory(t.Context(), key)
	if err != nil || len(fresh.Entries) != 1 {
		t.Fatalf("deleted content resurrected: %+v %v", fresh, err)
	}
}
