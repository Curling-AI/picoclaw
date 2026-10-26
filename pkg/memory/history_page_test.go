package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func pagedStore(t *testing.T, count int) (*JSONLStore, string) {
	t.Helper()
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "synthetic-paged-history"
	messages := make([]providers.Message, count)
	for i := range messages {
		messages[i] = providers.Message{
			Role:    "user",
			Content: fmt.Sprintf("synthetic-%d %s", i, strings.Repeat("x", 1024)),
		}
	}
	if operationErr := s.SetHistory(t.Context(), key, messages); operationErr != nil {
		t.Fatal(operationErr)
	}
	return s, key
}

func TestHistoryPagesBoundIOAndResumeAfterRestart(t *testing.T) {
	for _, count := range []int{546, 1000, 1001, 1752} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, key := pagedStore(t, count)
			request := HistoryPageRequest{Key: key}
			seen := make(map[string]bool)
			position := int64(0)
			pages, bytesRead := 0, 0
			for {
				page, err := s.ReadHistoryPage(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(page)
				if err != nil || len(encoded) > HistoryPageBytes || page.LinesRead > HistoryPageMessages ||
					len(page.Entries) > HistoryPageMessages ||
					page.StartPosition != position {
					t.Fatal("unbounded or discontinuous source page", len(encoded), page.LinesRead, err)
				}
				for _, entry := range page.Entries {
					if seen[entry.ID] || entry.Position != position {
						t.Fatal("duplicate identity or position", entry.Position)
					}
					seen[entry.ID] = true
					position++
				}
				pages++
				bytesRead += page.BytesRead
				if pages == 2 {
					retry, err := s.ReadHistoryPage(t.Context(), request)
					if err != nil || retry.ResumeCursor != page.ResumeCursor ||
						retry.Entries[0].ID != page.Entries[0].ID {
						t.Fatal("interrupted batch changed identity", err)
					}
					s, err = NewJSONLStore(s.dir)
					if err != nil {
						t.Fatal(err)
					}
				}
				request.Cursor = page.ResumeCursor
				if page.Complete {
					break
				}
			}
			if position != int64(count) || pages != (count+HistoryPageMessages-1)/HistoryPageMessages {
				t.Fatal("incomplete source coverage", position, pages)
			}
			info, err := os.Stat(s.jsonlPath(key))
			if err != nil || bytesRead > int(info.Size())+pages*historyReadBufferBytes {
				t.Fatal("IO exceeded bounded lookahead", bytesRead, err)
			}
			t.Logf(
				"messages=%d pages=%d sourceBytes=%d readBytes=%d scannedLines=%d",
				count,
				pages,
				info.Size(),
				bytesRead,
				count,
			)
		})
	}
}

func TestHistoryPageIdentitySurvivesLegacyCompactionAndEdits(t *testing.T) {
	s, key := pagedStore(t, 0)
	if operationErr := s.writeMeta(key, SessionMeta{Count: 2}); operationErr != nil {
		t.Fatal(operationErr)
	}
	line := []byte("{\"role\":\"user\",\"content\":\"identical legacy occurrence\"}\n")
	if operationErr := os.WriteFile(
		s.jsonlPath(key),
		append(append([]byte{}, line...), line...),
		0o600,
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	initial, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(initial.Entries) != 2 || initial.Entries[0].ID == initial.Entries[1].ID {
		t.Fatal("legacy occurrences did not acquire separate IDs", err)
	}
	if operationErr := s.TruncateHistory(t.Context(), key, 1); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.Compact(t.Context(), key); operationErr != nil {
		t.Fatal(operationErr)
	}
	compacted, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || compacted.Generation != initial.Generation || compacted.Entries[0].ID != initial.Entries[1].ID ||
		compacted.SourcePrefix != 1 {
		t.Fatal("compaction lost lineage, identity or prefix", err)
	}
	_, cursorErr := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key, Cursor: initial.ResumeCursor})
	if !errors.Is(cursorErr, ErrHistoryCursor) {
		t.Fatal("old physical offset survived compaction", cursorErr)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(history[0])
	if err != nil || strings.Contains(string(encoded), "history_id") {
		t.Fatal("persistence ID leaked to a provider message", err)
	}
	history[0].Content = "edited content"
	// An edit creates a new occurrence; untouched entries retain their IDs.
	history[0].HistoryID = ""
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	edited, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || edited.Generation == compacted.Generation || edited.SourcePrefix != 1 ||
		edited.Entries[0].ID == compacted.Entries[0].ID {
		t.Fatal("edit did not invalidate the old generation", err)
	}
}

func TestHistoryPageRejectsUncommittedRewriteLineage(t *testing.T) {
	s, key := pagedStore(t, 2)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a crash after metadata commits but before the JSONL rename.
	meta.HistoryFileGeneration = newHistoryID()
	meta.HistoryFileEpoch++
	if operationErr := s.writeMeta(key, meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, operationErr := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr,
		ErrHistoryCursor,
	) {
		t.Fatal("uncommitted replacement was reported as authoritative", operationErr)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 {
		t.Fatal("interrupted capture removed the original transcript", err)
	}
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !after.Complete || after.Entries[0].ID != before.Entries[0].ID {
		t.Fatal("committed replacement lost stable identity", err)
	}
}

func TestHistoryPagePinsCeilingAndRejectsForeignOrOverBudgetReads(t *testing.T) {
	s, key := pagedStore(t, 1)
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "late confirmed message"); operationErr != nil {
		t.Fatal(operationErr)
	}
	pinned, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key, Cursor: page.ResumeCursor})
	if err != nil || len(pinned.Entries) != 0 || !pinned.Complete {
		t.Fatal("append expanded a pinned import", err)
	}
	refreshed, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: page.ResumeCursor, Refresh: true},
	)
	if err != nil || len(refreshed.Entries) != 1 || refreshed.Entries[0].Position != 1 ||
		refreshed.Generation != page.Generation {
		t.Fatal("bounded tail continuation failed", err)
	}
	if operationErr := s.SetHistory(
		t.Context(),
		"other-source",
		[]providers.Message{{Role: "user", Content: "other"}},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	if _, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: "other-source", Cursor: page.ResumeCursor},
	); !errors.Is(
		err,
		ErrHistoryCursor,
	) {
		t.Fatal("foreign cursor accepted", err)
	}
	if operationErr := s.AddMessage(
		t.Context(),
		key,
		"assistant",
		strings.Repeat("x", HistoryMessageBytes+1),
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	partial, operationErr := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if operationErr != nil || partial.Complete || len(partial.Entries) != 2 {
		t.Fatal("valid prefix lost before over-budget row", operationErr)
	}
	if _, operationErr := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: partial.ResumeCursor},
	); !errors.Is(
		operationErr,
		ErrHistoryCapacity,
	) {
		t.Fatal("oversized row silently skipped", operationErr)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, operationErr := s.ReadHistoryPage(canceled, HistoryPageRequest{Key: key}); operationErr == nil {
		t.Fatal("canceled read continued")
	}
}

func TestHistoryPageSkipsLegacyInvalidAndThoughtLines(t *testing.T) {
	s, key := pagedStore(t, 0)
	raw := []byte(
		"{\"role\":\"user\",\"content\":\"before\"}\n\n{broken\n{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"private thought\"}\n{\"role\":\"assistant\",\"content\":\"after\"}\n",
	)
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Count: 4}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.EndPosition != 4 || len(page.Entries) != 2 || page.Entries[1].Position != 3 ||
		page.Entries[1].Message.Content != "after" {
		t.Fatalf("legacy filtering lost physical coverage: %+v", page)
	}
}

func TestHistoryPageEnforcesSerializedPageByteLimit(t *testing.T) {
	s, key := pagedStore(t, 0)
	var messages []providers.Message
	for i := 0; i < 6; i++ {
		messages = append(messages, providers.Message{Role: "user", Content: strings.Repeat("x", 200<<10)})
	}
	if err := s.SetHistory(t.Context(), key, messages); err != nil {
		t.Fatal(err)
	}
	request := HistoryPageRequest{Key: key}
	count := 0
	for attempt := 0; attempt < 6; attempt++ {
		page, err := s.ReadHistoryPage(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(page)
		if err != nil || len(data) > HistoryPageBytes {
			t.Fatalf("page exceeded byte budget: %d %v", len(data), err)
		}
		count += len(page.Entries)
		if page.Complete {
			break
		}
		request.Cursor = page.ResumeCursor
	}
	if count != 6 {
		t.Fatal("byte boundary lost source entries", count)
	}
}

func TestHistoryCursorRejectsAnotherKeyEvenWithIdenticalLineage(t *testing.T) {
	s, key := pagedStore(t, 2)
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.writeMeta("other", meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	data, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.jsonlPath("other"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: "other", Cursor: page.ResumeCursor},
	); !errors.Is(
		err,
		ErrHistoryCursor,
	) {
		t.Fatal("foreign key accepted with identical lineage", err)
	}
}
