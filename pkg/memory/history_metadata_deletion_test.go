package memory

import (
	"bytes"
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
			content := "must not attach"
			if replacement {
				content = "new content without proof"
			}
			appendErr := s.AddMessage(
				t.Context(),
				key,
				"user",
				content,
			)
			if replacement {
				if appendErr != nil {
					t.Fatal("unproven readable replacement blocked ordinary content", appendErr)
				}
				history, historyErr := s.GetHistory(t.Context(), key)
				if historyErr != nil || len(history) != 1 || history[0].Content != content ||
					messageHistoryOrder(history[0]) != (HistoryOrderProof{}) {
					t.Fatal("replacement content was lost or received new order", history, historyErr)
				}
				fenced, metaErr := s.GetSessionMeta(t.Context(), key)
				if metaErr != nil || fenced.HistoryGeneration != before.HistoryGeneration ||
					fenced.HistoryFileGeneration != before.HistoryFileGeneration ||
					fenced.HistoryStreamID != before.HistoryStreamID || fenced.HistorySequence != before.HistorySequence {
					t.Fatal("replacement retired identity or reconstructed stream/sequence", fenced, metaErr)
				}
				if _, markerErr := os.Stat(s.historyRecoveryRequiredPath(key)); markerErr != nil {
					t.Fatal("unproven replacement did not durably retain recovery", markerErr)
				}
				requireContentRecoveryFence(t, s, key)
				reopened, openErr := NewJSONLStore(s.dir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				requireHistoryContents(t, reopened, key, content)
				requireContentRecoveryFence(t, reopened, key)
				persisted, metaErr := reopened.GetSessionMeta(t.Context(), key)
				if metaErr != nil || persisted.HistoryGeneration != before.HistoryGeneration ||
					persisted.HistoryStreamID != before.HistoryStreamID || persisted.HistorySequence != before.HistorySequence {
					t.Fatal("reopen reattested an ambiguous replacement", persisted, metaErr)
				}
				return
			}
			if !errors.Is(appendErr, ErrHistoryCursor) {
				t.Fatal("ambiguous recreation bypassed the existing checkpoint guard", appendErr)
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
