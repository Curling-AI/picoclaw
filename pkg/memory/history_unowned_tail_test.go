package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

const durableExternalRow = "{\"role\":\"user\",\"content\":\"durable legacy tail\"}\n"

func historyWithExternalTail(t *testing.T, partial bool) (*JSONLStore, string, []byte, HistoryPage) {
	t.Helper()
	s, key := pagedStore(t, 2)
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	appendRawHistory(t, s, key, []byte(durableExternalRow))
	preserved, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		appendRawHistory(t, s, key, []byte(`{"role":"user","content":"partial`))
	}
	return s, key, preserved, page
}

func requireUnprovenPage(t *testing.T, s *JSONLStore, key, cursor string) {
	t.Helper()
	for _, refresh := range []bool{false, true} {
		page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key, Cursor: cursor, Refresh: refresh})
		if !errors.Is(err, ErrHistoryRecoveryRequired) || page.Complete || len(page.Entries) != 0 {
			t.Fatal("unowned durable history produced a capture proof", page, err)
		}
	}
}

func requireHistoryContents(t *testing.T, s *JSONLStore, key string, expected ...string) {
	t.Helper()
	messages, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != len(expected) {
		t.Fatalf("history has %d messages, want %d: %+v", len(messages), len(expected), messages)
	}
	for i, content := range expected {
		if !strings.HasPrefix(messages[i].Content, content) {
			t.Fatalf("message %d = %q, want prefix %q", i, messages[i].Content, content)
		}
	}
}

func TestHistoryUnownedTailAppendPreservesCompleteRows(t *testing.T) {
	s, key, preserved, page := historyWithExternalTail(t, true)
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1", "durable legacy tail")
	if err := s.AddMessage(t.Context(), key, "assistant", "next append"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(raw, preserved) || bytes.Contains(raw, []byte(`"content":"partial`)) {
		t.Fatal("append replaced a complete external row or adopted the torn suffix", string(raw), err)
	}
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1", "durable legacy tail", "next append")
	requireUnprovenPage(t, s, key, "")
	requireUnprovenPage(t, s, key, page.ResumeCursor)
	var record map[string]any
	last := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if err := json.Unmarshal(last[len(last)-1], &record); err != nil || record["history_recovery_required"] != true {
		t.Fatal("new append falsely reattested the external source", record, err)
	}
}

func TestHistoryUnownedTailFencesBeforeAppendAndAfterRestart(t *testing.T) {
	s, key, preserved, page := historyWithExternalTail(t, false)
	requireUnprovenPage(t, s, key, "")
	requireUnprovenPage(t, s, key, page.ResumeCursor)
	if err := s.SetSummary(t.Context(), key, "display remains available"); err != nil {
		t.Fatal(err)
	}
	s, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	requireUnprovenPage(t, s, key, page.ResumeCursor)
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1", "durable legacy tail")
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(raw, preserved) {
		t.Fatal("a proof attempt modified the unowned durable tail", err)
	}
}

func TestHistoryUnownedTailFailedAppendRollsBackOnlyItsOwnBytes(t *testing.T) {
	s, key, preserved, page := historyWithExternalTail(t, true)
	sentinel := errors.New("injected metadata failure")
	failed := false
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.metaPath(key) && !failed {
			failed = true
			return sentinel
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	if err := s.AddMessage(t.Context(), key, "assistant", "must roll back"); !errors.Is(err, sentinel) {
		t.Fatal("append did not expose its metadata failure", err)
	}
	raw, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(raw, preserved) {
		t.Fatal("failed append rolled back somebody else's durable row", string(raw), err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	requireUnprovenPage(t, s, key, page.ResumeCursor)
	if err := s.AddMessage(t.Context(), key, "assistant", "retry append"); err != nil {
		t.Fatal(err)
	}
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1", "durable legacy tail", "retry append")
}

func TestHistoryUnownedTailMarkerFailureNeverChangesTranscript(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-marker-write", true: "after-marker-write"}[published], func(t *testing.T) {
			s, key, _, _ := historyWithExternalTail(t, true)
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("marker storage failure")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.historyRecoveryRequiredPath(key) {
					if published {
						if operationErr := fileutil.WriteFileAtomic(path, data, mode); operationErr != nil {
							return operationErr
						}
					}
					return sentinel
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			if operationErr := s.AddMessage(
				t.Context(),
				key,
				"assistant",
				"not written",
			); !errors.Is(
				operationErr,
				sentinel,
			) {
				t.Fatal("marker failure was ignored", operationErr)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("marker failure authorized modifying the unowned tail", err)
			}
		})
	}
}

func TestHistoryUnownedTailCompactPreservesSkippedPrefix(t *testing.T) {
	s, key := pagedStore(t, 4)
	if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal(err)
	}
	appendRawHistory(t, s, key, []byte(durableExternalRow))
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.Compact(t.Context(), key); !errors.Is(operationErr, ErrHistoryRecoveryRequired) {
		t.Fatal("compaction adopted unowned rows after a logical truncation", operationErr)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("compaction discarded the retained physical history", err)
	}
	requireHistoryContents(t, s, key, "synthetic-2", "synthetic-3", "durable legacy tail")
	requireUnprovenPage(t, s, key, "")
}

func TestHistoryUnownedTailOwnJournalDecidesBeforeTailDetection(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "OLD", true: "NEW"}[committed], func(t *testing.T) {
			s, key, preserved, _ := historyWithExternalTail(t, false)
			crash := errors.New("simulated interruption")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) {
					if committed {
						if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
							return err
						}
					}
					panic(crash)
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			func() {
				defer func() {
					if recover() != crash {
						t.Fatal("append did not reach the intended interruption")
					}
				}()
				_ = s.AddMessage(t.Context(), key, "assistant", "candidate append")
			}()
			s, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.HasPrefix(raw, preserved) {
				t.Fatal("own journal removed unowned bytes", err)
			}
			expected := []string{"synthetic-0", "synthetic-1", "durable legacy tail"}
			if committed {
				expected = append(expected, "candidate append")
			}
			requireHistoryContents(t, s, key, expected...)
			requireUnprovenPage(t, s, key, "")
			if _, err := os.Stat(s.historyRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("resolved own journal remained pending", err)
			}
		})
	}
}

func TestHistoryUnownedTailGuardsMutators(t *testing.T) {
	operations := map[string]func(*JSONLStore, string) error{
		"set-identical-fast-path": func(s *JSONLStore, key string) error {
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				return err
			}
			return s.SetHistory(t.Context(), key, history[:2])
		},
		"set-suffix-fast-path": func(s *JSONLStore, key string) error {
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				return err
			}
			return s.SetHistory(
				t.Context(),
				key,
				append(history[:2], providers.Message{Role: "user", Content: "replacement"}),
			)
		},
		"replace": func(s *JSONLStore, key string) error {
			return s.SetHistory(t.Context(), key, []providers.Message{{Role: "user", Content: "replacement"}})
		},
		"truncate": func(s *JSONLStore, key string) error { return s.TruncateHistory(t.Context(), key, 1) },
		"compact":  func(s *JSONLStore, key string) error { return s.Compact(t.Context(), key) },
		"delete":   func(s *JSONLStore, key string) error { return s.removeSessionFiles(sanitizeKey(key)) },
		"alias-source": func(s *JSONLStore, key string) error {
			_, err := s.PromoteAliasHistory(t.Context(), "empty-target", nil, []string{key})
			return err
		},
		"alias-target": func(s *JSONLStore, key string) error {
			_, err := s.PromoteAliasHistory(t.Context(), key, nil, []string{"empty-alias"})
			return err
		},
	}
	for name, mutate := range operations {
		t.Run(name, func(t *testing.T) {
			s, key, preserved, _ := historyWithExternalTail(t, false)
			if err := mutate(s, key); !errors.Is(err, ErrHistoryRecoveryRequired) {
				t.Fatal("mutator accepted unowned durable history")
			}
			raw, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(raw, preserved) {
				t.Fatal("mutator discarded durable history", err)
			}
			requireUnprovenPage(t, s, key, "")
		})
	}
}

func TestHistoryPartialTailAppendKeepsProvenPrefix(t *testing.T) {
	s, key := pagedStore(t, 2)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	appendRawHistory(t, s, key, []byte(`{"role":"user","content":"partial`))
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "complete next"); operationErr != nil {
		t.Fatal(operationErr)
	}
	page, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: before.ResumeCursor, Refresh: true},
	)
	if err != nil || !page.Complete || len(page.Entries) != 1 || page.Entries[0].Message.Content != "complete next" {
		t.Fatal("a partial suffix was promoted or retired the proven source", page, err)
	}
	if _, err := os.Stat(s.historyRecoveryRequiredPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partial suffix latched recovery without a complete external row", err)
	}
}

func TestHistoryRecoveryMarkerCannotBeClearedByMetadataOrCache(t *testing.T) {
	s, key := pagedStore(t, 2)
	// Even a torn marker is a durable fence, including when metadata is warm.
	if err := os.WriteFile(s.historyRecoveryRequiredPath(key), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	requireUnprovenPage(t, s, key, "")
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 2}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil || !meta.HistoryRecoveryRequired {
		t.Fatal("metadata rewrite cleared the persistent recovery fence", meta, err)
	}
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1")
}

func TestHistoryRecoveryMarkerParsingFailsClosed(t *testing.T) {
	for _, value := range []string{"true", "null", `"false"`, "{}"} {
		if err := historyRecordRecoveryRequired(
			[]byte(`{"history_recovery_required":` + value + `}`),
		); !errors.Is(
			err,
			ErrHistoryRecoveryRequired,
		) {
			t.Fatal("malformed or positive marker was accepted", value, err)
		}
	}
	for _, value := range []string{`{}`, `{"history_recovery_required":false}`} {
		if err := historyRecordRecoveryRequired([]byte(value)); err != nil {
			t.Fatal("unmarked legacy row was rejected", value, err)
		}
	}
}

func TestHistoryUnownedTailPrunePreservesSession(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-owned:unproven"
	if operationErr := s.AddMessage(context.Background(), key, "user", "owned row"); operationErr != nil {
		t.Fatal(operationErr)
	}
	appendRawHistory(t, s, key, []byte(durableExternalRow))
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
		if operationErr := os.Chtimes(path, old, old); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	removed, err := s.PruneCronRuns(time.Hour, time.Now())
	if err != nil || removed != 0 {
		t.Fatal("prune removed unowned durable history", removed, err)
	}
	requireHistoryContents(t, s, key, "owned row", "durable legacy tail")
	requireUnprovenPage(t, s, key, "")
}

func TestHistoryUnownedTailFenceSurvivesLegacyRewriteAndMissingFiles(t *testing.T) {
	s, key, _, page := historyWithExternalTail(t, false)
	if err := s.AddMessage(t.Context(), key, "assistant", "unproven append"); err != nil {
		t.Fatal(err)
	}
	// Legacy code reserializes provider messages and drops unknown metadata.
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	for _, message := range history {
		if operationErr := json.NewEncoder(&raw).Encode(message); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	metaBytes, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if operationErr := json.Unmarshal(metaBytes, &meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	delete(meta, "history_recovery_required")
	delete(meta, "history_bytes")
	metaBytes, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := os.WriteFile(s.jsonlPath(key), raw.Bytes(), 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := os.WriteFile(s.metaPath(key), metaBytes, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	requireUnprovenPage(t, s, key, page.ResumeCursor)
	requireHistoryContents(t, s, key, "synthetic-0", "synthetic-1", "durable legacy tail", "unproven append")
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
		if operationErr := os.Remove(path); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(t.Context(), key, "user", "still requires recovery"); err != nil {
		t.Fatal(err)
	}
	requireHistoryContents(t, s, key, "still requires recovery")
	requireUnprovenPage(t, s, key, "")
}
