package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestSetHistoryAppendsWithoutRewritingTheCapturedPrefix(t *testing.T) {
	s, key := pagedStore(t, 1752)
	request := HistoryPageRequest{Key: key}
	var before HistoryPage
	for {
		page, err := s.ReadHistoryPage(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if page.Complete {
			before = page
			break
		}
		request.Cursor = page.ResumeCursor
	}
	prefix, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	run := uuid.NewString()
	history = append(history, providers.Message{Role: "assistant", Content: "confirmed late response"})
	if operationErr := s.SetHistory(providers.WithHistoryRun(t.Context(), run), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	after, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: before.ResumeCursor, Refresh: true},
	)
	if err != nil || !after.Complete || len(after.Entries) != 1 || after.Entries[0].Position != 1752 ||
		after.Generation != before.Generation ||
		after.FileGeneration != before.FileGeneration ||
		after.Entries[0].Message.HistoryRunID != run {
		t.Fatal("normal history append invalidated bounded continuation", err)
	}
	stored, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(prefix, stored[:len(prefix)]) {
		t.Fatal("append rewrote the stored prefix", err)
	}
	providerJSON, err := json.Marshal(after.Entries[0].Message)
	if err != nil || strings.Contains(string(providerJSON), run) ||
		strings.Contains(string(providerJSON), "history_id") {
		t.Fatal("capture provenance leaked to a provider", err)
	}
}

func TestHistoryReplacementDistinguishesMetadataFromAnEdit(t *testing.T) {
	s, key := pagedStore(t, 6)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	history[5].ModelName = "synthetic-model-metadata"
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	metadata, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || metadata.Generation != before.Generation || metadata.FileGeneration == before.FileGeneration ||
		metadata.Entries[5].ID != before.Entries[5].ID {
		t.Fatal("audit/model enrichment was treated as a transcript edit", err)
	}
	history[3].Content = "edited turn"
	run := uuid.NewString()
	if operationErr := s.SetHistory(providers.WithHistoryRun(t.Context(), run), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	edited, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || edited.Generation == metadata.Generation || edited.ParentGeneration != metadata.Generation ||
		edited.RetainedUntil != 3 ||
		edited.MutationRunID != run ||
		edited.Entries[3].Message.HistoryRunID != run {
		t.Fatal("edit lost its generation boundary or execution provenance", err)
	}
	if operationErr := s.SetHistory(t.Context(), key, history[:2]); operationErr != nil {
		t.Fatal(operationErr)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(removed.Entries) != 2 || removed.ParentGeneration != edited.Generation ||
		removed.RetainedUntil != 2 ||
		removed.Entries[0].ID != before.Entries[0].ID {
		t.Fatal("removed messages reappeared after restart", err)
	}
}
