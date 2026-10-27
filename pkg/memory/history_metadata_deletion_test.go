package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestMetadataOnlySourceKeepsEmptyPagesAndRetiresDeletedCursors(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const key = "metadata-only"
	if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, nil); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !before.Complete || len(before.Entries) != 0 {
		t.Fatal("metadata-only source lost its empty page", before, err)
	}
	if operationErr := os.Remove(s.metaPath(key)); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, nil); operationErr != nil {
		t.Fatal(operationErr)
	}
	after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !after.Complete || len(after.Entries) != 0 || after.Generation == before.Generation {
		t.Fatal("deleted empty source reused its old identity", after, err)
	}
	if _, operationErr := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: before.ResumeCursor},
	); !errors.Is(
		operationErr,
		ErrHistoryCursor,
	) {
		t.Fatal("deleted empty cursor remained live", operationErr)
	}
}

func TestMetadataUpsertRecoversPendingJournalBeforeRecreatingDeletedSource(t *testing.T) {
	s, key := pagedStore(t, 2)
	if operationErr := s.SetSummary(t.Context(), key, "deleted private summary"); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := s.GetSessionMeta(t.Context(), key)
	if err != nil || before.Count != 2 || before.HistoryBytes == nil {
		t.Fatal("fixture needs an existing committed source", before, err)
	}
	offset := *before.HistoryBytes
	next := before
	next.Count++
	if operationErr := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: before, Next: next, AppendOffset: &offset},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	// A completed external removal can leave this writer's journal pending.
	// Exercise the live metadata precursor, not startup cleanup or concurrent DELETE.
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
		if operationErr := os.Remove(path); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	scope := []byte(`{"channel":"web"}`)
	if operationErr := s.UpsertSessionMeta(t.Context(), key, scope, []string{"fresh-alias"}); operationErr != nil {
		t.Fatal("metadata recreation did not resolve its pending journal", operationErr)
	}
	for _, content := range []string{"fresh first", "fresh second"} {
		if operationErr := s.AddMessage(t.Context(), key, "user", content); operationErr != nil {
			t.Error("append after metadata recreation failed", operationErr)
		}
	}
	history, historyErr := s.GetHistory(t.Context(), key)
	if historyErr != nil || len(history) != 2 || history[0].Content != "fresh first" ||
		history[1].Content != "fresh second" {
		t.Error("recreation restored deleted content or lost fresh appends", history, historyErr)
	}
	if operationErr := s.SetHistory(t.Context(), key, nil); operationErr != nil {
		t.Error("clear after metadata recreation failed", operationErr)
	}
	if operationErr := s.Close(); operationErr != nil {
		t.Fatal(operationErr)
	}
	restarted, openErr := NewJSONLStore(s.dir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer restarted.Close()
	if operationErr := restarted.AddMessage(t.Context(), key, "assistant", "after restart"); operationErr != nil {
		t.Error("append after restart retained the deleted journal checkpoint", operationErr)
	}
	finalHistory, finalHistoryErr := restarted.GetHistory(t.Context(), key)
	if finalHistoryErr != nil || len(finalHistory) != 1 || finalHistory[0].Content != "after restart" {
		t.Error("clear and restart did not retain only the final append", finalHistory, finalHistoryErr)
	}
	after, metaErr := restarted.GetSessionMeta(t.Context(), key)
	var afterScope map[string]string
	scopeErr := json.Unmarshal(after.Scope, &afterScope)
	if metaErr != nil || after.HistoryGeneration == before.HistoryGeneration || after.Count != 1 ||
		after.Skip != 0 || after.Summary != "" || scopeErr != nil || afterScope["channel"] != "web" ||
		len(after.Aliases) != 1 || after.Aliases[0] != "fresh-alias" {
		t.Error("recreation retained deleted metadata or lost fresh ownership", after, metaErr, scopeErr)
	}
	for _, path := range []string{s.historyRecoveryPath(key), s.historyDecisionPath(key)} {
		if _, operationErr := os.Stat(path); !errors.Is(operationErr, os.ErrNotExist) {
			t.Error("completed lifecycle retained the deleted source's journal", path, operationErr)
		}
	}
}

func TestMetadataUpsertDoesNotRetireAmbiguousSource(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "metadata survives"
		if replacement {
			name = "replacement already exists"
		}
		t.Run(name, func(t *testing.T) {
			s, key := pagedStore(t, 2)
			before, err := s.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if operationErr := os.Remove(s.jsonlPath(key)); operationErr != nil {
				t.Fatal(operationErr)
			}
			if replacement {
				if operationErr := os.Remove(s.metaPath(key)); operationErr != nil {
					t.Fatal(operationErr)
				}
				if operationErr := os.WriteFile(s.jsonlPath(key), nil, 0o600); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, nil); operationErr != nil {
				t.Fatal(operationErr)
			}
			after, err := s.GetSessionMeta(t.Context(), key)
			if err != nil || after.Count != before.Count || after.HistoryGeneration != before.HistoryGeneration ||
				after.HistoryBytes == nil || *after.HistoryBytes != *before.HistoryBytes {
				t.Fatal("metadata update inferred a confirmed source deletion", after, err)
			}
			if operationErr := s.AddMessage(
				t.Context(),
				key,
				"user",
				"must not attach",
			); !errors.Is(
				operationErr,
				ErrHistoryCursor,
			) {
				t.Fatal("ambiguous recreation bypassed the existing checkpoint guard", operationErr)
			}
			if data, operationErr := os.ReadFile(s.jsonlPath(key)); operationErr != nil || len(data) != 0 {
				t.Fatal("ambiguous recreation appended or restored old content", string(data), operationErr)
			}
		})
	}
}

func TestMetadataUpsertPreservesContentWhenOnlyMetadataWasRemoved(t *testing.T) {
	s, key := pagedStore(t, 2)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := os.Remove(s.metaPath(key)); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, []string{"kept-alias"}); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.AddMessage(t.Context(), key, "user", "next"); operationErr != nil {
		t.Fatal(operationErr)
	}
	after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(after.Entries) != 3 || after.Generation != before.Generation ||
		after.Entries[0].ID != before.Entries[0].ID || after.Entries[1].ID != before.Entries[1].ID {
		t.Fatal("metadata loss retired or lost a surviving source", after, err)
	}
	retained, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(retained, data) {
		t.Fatal("metadata upsert rewrote durable history", err)
	}
}

func TestMetadataUpsertKeepsASurvivingRecoveryMarker(t *testing.T) {
	s, key := pagedStore(t, 1)
	if operationErr := os.WriteFile(s.historyRecoveryRequiredPath(key), nil, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if _, operationErr := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr,
		ErrHistoryRecoveryRequired,
	) {
		t.Fatal(operationErr)
	}
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
		if operationErr := os.Remove(path); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, nil); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.AddMessage(t.Context(), key, "user", "new content without proof"); operationErr != nil {
		t.Fatal(operationErr)
	}
	reopened, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, operationErr := reopened.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr,
		ErrHistoryRecoveryRequired,
	) {
		t.Fatal("metadata update discarded a surviving durable fence", operationErr)
	}
	history, err := reopened.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "new content without proof" {
		t.Fatal("surviving fence blocked content or resurrected old content", history, err)
	}
}

func TestMetadataUpsertDoesNotAttestAReplacementCreatedBeforeAppend(t *testing.T) {
	s, key := pagedStore(t, 1)
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
		if operationErr := os.Remove(path); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	if operationErr := s.UpsertSessionMeta(t.Context(), key, nil, nil); operationErr != nil {
		t.Fatal(operationErr)
	}
	// The source was absent during metadata retirement, but another writer's
	// complete row appeared before append acquired its file. Preserve it without
	// treating it as part of this writer's newly created source.
	replacement := []byte("{\"role\":\"user\",\"content\":\"external replacement\"}\n")
	if operationErr := os.WriteFile(s.jsonlPath(key), replacement, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "new reply"); operationErr != nil {
		t.Fatal("unproven replacement blocked ordinary content", operationErr)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || history[0].Content != "external replacement" ||
		history[1].Content != "new reply" {
		t.Fatal("replacement or later content was lost", history, err)
	}
	if _, operationErr := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr,
		ErrHistoryRecoveryRequired,
	) {
		t.Fatal("metadata retirement attested an unowned replacement", operationErr)
	}
}
