package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryPageRejectsExpandedFirstEntry(t *testing.T) {
	s, key := pagedStore(t, 0)
	raw := []byte(`{"role":"user","content":"` + strings.Repeat("<", 200<<10) + "\"}\n")
	if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); !errors.Is(err, ErrHistoryCapacity) {
		t.Fatal("expanded entry returned a non-progressing page instead of a capacity error", err)
	}
}

func TestHistoryPageLeavesUnterminatedTailUncommitted(t *testing.T) {
	s, key := pagedStore(t, 2)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	appendRawHistory(t, s, key, []byte(`{"role":"user","content":"torn`))
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !page.Complete || page.EndPosition != 2 || page.TotalPosition != 2 ||
		!reflect.DeepEqual(page.Entries, before.Entries) || page.ResumeCursor != before.ResumeCursor {
		t.Fatal("uncommitted tail changed the committed transcript or its checkpoint", page, err)
	}
}

func TestHistoryPagePreservesContextErrors(t *testing.T) {
	s, key := pagedStore(t, 1)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer expire()
	for _, ctx := range []context.Context{canceled, expired} {
		if _, err := s.ReadHistoryPage(ctx, HistoryPageRequest{Key: key}); !errors.Is(err, ctx.Err()) {
			t.Fatal("context failure retired a valid source cursor", err)
		}
	}
}

func TestHistoryPageIgnoresAppendBeforeMetaCommit(t *testing.T) {
	s, key := pagedStore(t, 1)
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := marshalHistoryMessage(providers.Message{Role: "assistant", Content: "uncommitted"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	appendRawHistory(t, s, key, append(orphan, '\n'))
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !page.Complete || !reflect.DeepEqual(page.Entries, before.Entries) {
		t.Fatal("an append without metadata commit broke the source", page, err)
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "committed next"); operationErr != nil {
		t.Fatal(operationErr)
	}
	page, err = s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key, Cursor: before.ResumeCursor, Refresh: true})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message.Content != "committed next" {
		t.Fatal("the next append committed an abandoned record", page, err)
	}
}

func TestHistoryWriteFailurePreservesCommittedState(t *testing.T) {
	for _, operation := range []string{"append", "suffix", "edit", "compact"} {
		for _, failure := range []string{"meta", "meta-partial", "jsonl"} {
			if failure == "jsonl" && (operation == "append" || operation == "suffix") {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				s, key := pagedStore(t, 4)
				if operation == "compact" {
					if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
						t.Fatal(err)
					}
				}
				before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
				if err != nil {
					t.Fatal(err)
				}
				beforeBytes, err := os.ReadFile(s.jsonlPath(key))
				if err != nil {
					t.Fatal(err)
				}
				history, err := s.GetHistory(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				sentinel := errors.New("injected storage write failure")
				s.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if failure == "meta-partial" && path == s.metaPath(key) {
						if writeErr := os.WriteFile(path, []byte(`{"count":`), mode); writeErr != nil {
							return writeErr
						}
						return sentinel
					}
					if (failure == "meta" && path == s.metaPath(key)) ||
						(failure == "jsonl" && path == s.jsonlPath(key)) {
						return sentinel
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				switch operation {
				case "append":
					err = s.AddMessage(t.Context(), key, "assistant", "rejected append")
				case "suffix":
					err = s.SetHistory(
						t.Context(),
						key,
						append(history, providers.Message{Role: "assistant", Content: "rejected suffix"}),
					)
				case "edit":
					history[1].Content = "rejected edit"
					err = s.SetHistory(t.Context(), key, history)
				case "compact":
					err = s.Compact(t.Context(), key)
				}
				if !errors.Is(err, sentinel) {
					t.Fatal("storage failure was hidden", err)
				}
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
				_ = s.ListSessionMetas()
				after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
				if err != nil || after.Generation != before.Generation ||
					after.FileGeneration != before.FileGeneration ||
					!reflect.DeepEqual(after.Entries, before.Entries) ||
					after.TotalPosition != before.TotalPosition {
					t.Fatal("failed write changed committed history after restart", after, err)
				}
				afterBytes, err := os.ReadFile(s.jsonlPath(key))
				if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
					t.Fatal("failed write changed source bytes", err)
				}
				if err := s.AddMessage(t.Context(), key, "assistant", "successful next write"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); err != nil {
					t.Fatal("failed write poisoned later appends", err)
				}
			})
		}
	}
}

func TestHistoryRecoveryAfterInterruptedCommit(t *testing.T) {
	for _, operation := range []string{"append", "edit", "compact"} {
		for _, committed := range []bool{false, true} {
			name := operation + "/before-meta"
			if committed {
				name = operation + "/after-meta"
			}
			t.Run(name, func(t *testing.T) {
				s, key := pagedStore(t, 4)
				if operation == "compact" {
					if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
						t.Fatal(err)
					}
				}
				before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
				if err != nil {
					t.Fatal(err)
				}
				history, err := s.GetHistory(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				interrupted := errors.New("simulated process interruption")
				s.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == s.metaPath(key) {
						if committed {
							if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
								t.Fatal(writeErr)
							}
						}
						panic(interrupted)
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				func() {
					defer func() {
						if recovered := recover(); recovered != interrupted {
							t.Fatal("write did not reach the expected interruption boundary", recovered)
						}
					}()
					switch operation {
					case "append":
						_ = s.AddMessage(t.Context(), key, "assistant", "new committed response")
					case "edit":
						history[1].Content = "edited response"
						_ = s.SetHistory(t.Context(), key, history)
					case "compact":
						_ = s.Compact(t.Context(), key)
					}
				}()
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
				_ = s.ListSessionMetas()
				after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
				if err != nil || !after.Complete {
					t.Fatal("restart did not recover a consistent committed source", after, err)
				}
				if !committed {
					if after.Generation != before.Generation || after.FileGeneration != before.FileGeneration ||
						!reflect.DeepEqual(
							after.Entries,
							before.Entries,
						) || after.TotalPosition != before.TotalPosition {
						t.Fatal("restart exposed an uncommitted replacement", after)
					}
				} else {
					switch operation {
					case "append":
						if len(after.Entries) != 5 || after.Entries[4].Message.Content != "new committed response" ||
							after.Generation != before.Generation {
							t.Fatal("restart rolled back a committed append", after)
						}
					case "edit":
						if after.Generation == before.Generation ||
							after.Entries[1].Message.Content != "edited response" {
							t.Fatal("restart rolled back a committed edit", after)
						}
					case "compact":
						if after.SourcePrefix != 2 || after.Generation != before.Generation ||
							after.Entries[0].ID != before.Entries[2].ID || after.Entries[0].Position != 2 {
							t.Fatal("restart rolled back committed compaction or lost its identities", after)
						}
					}
				}
				if _, err := os.Stat(s.historyRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed recovery retained a stale journal", err)
				}
			})
		}
	}
}

func TestHistoryInvalidRecoveryPreservesTranscript(t *testing.T) {
	s, key := pagedStore(t, 2)
	for range 5 {
		if err := s.AddMessage(t.Context(), "healthy", "user", "healthy history"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(
		s.historyRecoveryPath(key),
		[]byte(`{"meta":{"key":"synthetic-paged-history"},"next":{}}`),
		0o600,
	); writeErr != nil {
		t.Fatal(writeErr)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal("one invalid checkpoint disabled the whole store", err)
	}
	healthy, err := restarted.GetHistory(t.Context(), "healthy")
	if err != nil || len(healthy) != 5 {
		t.Fatal("invalid checkpoint hid another session", healthy, err)
	}
	if _, pageErr := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); pageErr == nil {
		t.Fatal("broken session was not fenced")
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid recovery descriptor damaged the transcript", err)
	}
}

func TestHistoryRewritesWithFilteredRowsRetireMovedPositions(t *testing.T) {
	for _, operation := range []string{"edit", "compact"} {
		t.Run(operation, func(t *testing.T) {
			s, key := pagedStore(t, 0)
			raw := []byte(
				"{\"role\":\"user\",\"content\":\"u0\"}\n{broken\n{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"private thought\"}\n{\"role\":\"assistant\",\"content\":\"a1\"}\n{\"role\":\"user\",\"content\":\"u2\"}\n{\"role\":\"assistant\",\"content\":\"a3\"}\n",
			)
			if err := os.WriteFile(s.jsonlPath(key), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.writeMeta(key, SessionMeta{Count: 6}); err != nil {
				t.Fatal(err)
			}
			before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			if operation == "compact" {
				if operationErr := s.TruncateHistory(t.Context(), key, 3); operationErr != nil {
					t.Fatal(operationErr)
				}
				err = s.Compact(t.Context(), key)
			} else {
				history, readErr := s.GetHistory(t.Context(), key)
				if readErr != nil {
					t.Fatal(readErr)
				}
				err = s.SetHistory(t.Context(), key, history[:3])
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil || after.Generation == before.Generation || after.ParentGeneration != before.Generation ||
				after.RetainedUntil != 1 {
				t.Fatal("filtered physical positions were silently reused", after, err)
			}
			for _, entry := range after.Entries {
				for _, old := range before.Entries {
					if entry.Message.Content == old.Message.Content && entry.ID != old.ID {
						t.Fatal("retained message lost its identity", entry, old)
					}
				}
			}
		})
	}
}

func appendRawHistory(t *testing.T, s *JSONLStore, key string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(s.jsonlPath(key), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
