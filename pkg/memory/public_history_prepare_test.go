package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func legacyPublicFixture(t *testing.T, count int) (*JSONLStore, string) {
	t.Helper()
	store, storeErr := NewJSONLStore(t.TempDir())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	key := "agent:web:legacy-prepare"
	var data []byte
	for index := 0; index < count; index++ {
		line, lineErr := json.Marshal(providers.Message{Role: "assistant", Content: fmt.Sprintf("legacy-%d", index)})
		if lineErr != nil {
			t.Fatal(lineErr)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if count > 0 {
		if operationErr := os.WriteFile(store.jsonlPath(key), data, 0o600); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	meta, _ := json.Marshal(SessionMeta{Key: key, Count: count})
	if operationErr1 := os.WriteFile(store.metaPath(key), meta, 0o600); operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	return store, key
}

func TestPublicHistoryPreparesLegacySourceInBoundedSeekStepsWhileAppendContinues(t *testing.T) {
	store, key := legacyPublicFixture(t, 120)
	first, firstErr := store.PreparePublicHistory(t.Context(), key, "")
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if first.Ready || first.Count != 0 || first.Cursor == "" {
		t.Fatal("legacy source initialized without bounded preparation")
	}
	next, firstErr := store.PreparePublicHistory(t.Context(), key, first.Cursor)
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if next.Count != 50 || next.Ready {
		t.Fatalf("first step did not stop at50: %+v", next)
	}
	if _, operationErr2 := store.ReadPublicHistory(
		t.Context(),
		key,
	); !errors.Is(
		operationErr2,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("incomplete source became readable: %v", operationErr2)
	}
	if _, operationErr3 := store.PreparePublicHistory(
		t.Context(),
		key,
		first.Cursor,
	); !errors.Is(
		operationErr3,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("obsolete preparation cursor advanced twice: %v", operationErr3)
	}
	if operationErr4 := store.AddMessage(t.Context(), key, "assistant", "new confirmed append"); operationErr4 != nil {
		t.Fatal(operationErr4)
	}
	for step := 0; !next.Ready && step < 5; step++ {
		next, firstErr = store.PreparePublicHistory(t.Context(), key, next.Cursor)
		if firstErr != nil {
			t.Fatal(firstErr)
		}
	}
	if !next.Ready || !next.Complete || next.Count != 121 {
		t.Fatalf("preparation missed appended frontier: %+v", next)
	}
	history, firstErr := store.ReadPublicHistory(t.Context(), key)
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	for index := 0; index < 120; index++ {
		if history.Entries[index].Message.Content != fmt.Sprintf("legacy-%d", index) {
			t.Fatal("preparation reordered legacy messages")
		}
	}
	if history.Entries[120].Message.Content != "new confirmed append" {
		t.Fatal("new append was inserted inside old range")
	}
}

func TestPublicHistoryPreparationRequiresExistingScopeAndEmptyImportBoundary(t *testing.T) {
	store, key := legacyPublicFixture(t, 0)
	prepared, preparedErr := store.PreparePublicHistory(t.Context(), key, "")
	if preparedErr != nil {
		t.Fatal(preparedErr)
	}
	if !prepared.Ready || !prepared.Complete || prepared.Count != 0 {
		t.Fatalf("empty scope not proven: %+v", prepared)
	}
	entry := PublicHistoryEntry{
		ID:       uuid.NewString(),
		ImportID: "legacy:confirmed-first",
		Message:  providers.Message{Role: "assistant", Content: "legacy answer"},
	}
	result, preparedErr := store.ImportPublicHistory(
		t.Context(),
		PublicHistoryImport{
			Key:        key,
			Generation: prepared.Generation,
			Revision:   prepared.Revision,
			Entries:    []PublicHistoryEntry{entry},
		},
	)
	if preparedErr != nil {
		t.Fatal(preparedErr)
	}
	if len(result.Entries) != 1 || result.Entries[0].Order != (HistoryOrderProof{}) {
		t.Fatal("historical import invented producer receipt authority")
	}
	model, preparedErr := store.GetHistory(t.Context(), key)
	if preparedErr != nil || len(model) != 0 {
		t.Fatal("legacy import entered model context")
	}
	if operationErr5 := store.DeleteSession(t.Context(), key); operationErr5 != nil {
		t.Fatal(operationErr5)
	}
	if _, operationErr6 := store.PreparePublicHistory(t.Context(), key, ""); !os.IsNotExist(operationErr6) {
		t.Fatalf("preparation recreated deleted scope: %v", operationErr6)
	}
	if _, operationErr7 := store.ImportPublicHistory(
		t.Context(),
		PublicHistoryImport{
			Key:        key,
			Generation: prepared.Generation,
			Revision:   prepared.Revision,
			Entries:    []PublicHistoryEntry{entry},
		},
	); operationErr7 == nil {
		t.Fatal("old import resurrected deleted conversation")
	}
}

func TestPublicHistoryPreparationPreservesUncoveredPrefixAndInvalidatesAfterEdit(t *testing.T) {
	for _, operation := range []string{"compact", "edit"} {
		t.Run(operation, func(t *testing.T) {
			store, key := legacyPublicFixture(t, 80)
			first, firstErr8 := store.PreparePublicHistory(t.Context(), key, "")
			if firstErr8 != nil {
				t.Fatal(firstErr8)
			}
			next, firstErr8 := store.PreparePublicHistory(t.Context(), key, first.Cursor)
			if firstErr8 != nil {
				t.Fatal(firstErr8)
			}
			before, firstErr8 := os.ReadFile(store.publicHistoryPath(key))
			if firstErr8 != nil {
				t.Fatal(firstErr8)
			}
			if operation == "compact" {
				if operationErr9 := store.TruncateHistory(t.Context(), key, 20); operationErr9 != nil {
					t.Fatal(operationErr9)
				}
				if err := store.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
					t.Fatal("physical GC discarded an incompletely prepared prefix", err)
				}
			} else {
				if operationErr11 := store.SetHistory(
					t.Context(),
					key,
					[]providers.Message{{Role: "user", Content: "edited"}},
				); operationErr11 != nil {
					t.Fatal(operationErr11)
				}
			}
			if operation == "compact" {
				after, err := os.ReadFile(store.publicHistoryPath(key))
				if err != nil || string(before) != string(after) {
					t.Fatal("refused GC changed incomplete display evidence", err)
				}
				return
			}
			if _, operationErr12 := store.PreparePublicHistory(
				t.Context(),
				key,
				next.Cursor,
			); !errors.Is(
				operationErr12,
				ErrPublicHistoryCoverage,
			) {
				t.Fatalf("incompatible source resumed: %v", operationErr12)
			}
			after, firstErr8 := os.ReadFile(store.publicHistoryPath(key))
			if firstErr8 != nil || string(before) != string(after) {
				t.Fatal("invalidated preparation destroyed source evidence")
			}
		})
	}
}

func TestPublicHistoryPreparationAcceptsLargePublicMessageAndStripsNestedProviderSecrets(t *testing.T) {
	store, key := legacyPublicFixture(t, 0)
	message := providers.Message{
		Role:    "assistant",
		Content: strings.Repeat("x", 393<<10),
		ToolCalls: []providers.ToolCall{
			{
				ID: "call",
				Function: &providers.FunctionCall{
					Name:             "read_file",
					Arguments:        `{"path":"report"}`,
					ThoughtSignature: "private-function",
				},
				ExtraContent: &providers.ExtraContent{
					Google: &providers.GoogleExtra{ThoughtSignature: "private-google"},
				},
			},
		},
	}
	line, _ := json.Marshal(message)
	line = append(line, '\n')
	if operationErr13 := os.WriteFile(store.jsonlPath(key), line, 0o600); operationErr13 != nil {
		t.Fatal(operationErr13)
	}
	meta, _ := json.Marshal(SessionMeta{Key: key, Count: 1})
	if operationErr14 := os.WriteFile(store.metaPath(key), meta, 0o600); operationErr14 != nil {
		t.Fatal(operationErr14)
	}
	prepared, preparedErr15 := store.PreparePublicHistory(t.Context(), key, "")
	if preparedErr15 != nil {
		t.Fatal(preparedErr15)
	}
	done, preparedErr15 := store.PreparePublicHistory(t.Context(), key, prepared.Cursor)
	if preparedErr15 != nil || !done.Ready {
		t.Fatalf("large legacy public message not prepared: %+v %v", done, preparedErr15)
	}
	history, preparedErr15 := store.ReadPublicHistory(t.Context(), key)
	if preparedErr15 != nil {
		t.Fatal(preparedErr15)
	}
	encoded, _ := json.Marshal(history)
	if strings.Contains(string(encoded), "private-function") || strings.Contains(string(encoded), "private-google") ||
		history.Entries[0].Message.ToolCalls[0].Function.Arguments != `{"path":"report"}` {
		t.Fatal("nested provider metadata leaked or tool arguments changed")
	}
}

func TestPublicHistoryColdLegacyAppendStartsPreparationWithoutWholeSourceCopy(t *testing.T) {
	store, key := legacyPublicFixture(t, 120)
	if operationErr16 := store.AddMessage(t.Context(), key, "assistant", "new"); operationErr16 != nil {
		t.Fatal(operationErr16)
	}
	meta, metaErr := store.readMeta(key)
	if metaErr != nil {
		t.Fatal(metaErr)
	}
	if meta.PublicHistoryPreparation == nil || meta.PublicHistoryCount != 0 || meta.PublicHistoryBytes != 0 {
		t.Fatal("ordinary append materialized cold legacy history")
	}
	state, metaErr := store.PreparePublicHistory(t.Context(), key, "")
	if metaErr != nil {
		t.Fatal(metaErr)
	}
	for step := 0; !state.Ready && step < 5; step++ {
		state, metaErr = store.PreparePublicHistory(t.Context(), key, state.Cursor)
		if metaErr != nil {
			t.Fatal(metaErr)
		}
	}
	if !state.Ready || state.Count != 121 {
		t.Fatalf("cold append not incorporated: %+v", state)
	}
}
