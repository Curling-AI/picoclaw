package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryPageRejectsUncountedLegacyRowBeforeAppend(t *testing.T) {
	s := newTestStore(t)
	key := "legacy-before-append"
	rows := "{\"role\":\"user\",\"content\":\"first\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"second\"}\n" + durableExternalRow
	if err := os.WriteFile(s.jsonlPath(key), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 2}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if !errors.Is(err, ErrHistoryRecoveryRequired) || page.Complete || len(page.Entries) != 0 {
		t.Fatal("a first page omitted an uncounted durable row", page, err)
	}
	requireHistoryContents(t, s, key, "first", "second", "durable legacy tail")
}

func TestHistoryPageRejectsRowOnlyRecoveryMarker(t *testing.T) {
	s := newTestStore(t)
	key := "row-only-marker"
	rows := "{\"role\":\"user\",\"content\":\"unproven\",\"history_recovery_required\":true}\n"
	if err := os.WriteFile(s.jsonlPath(key), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unaware writer preserved only the row's private marker.
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if !errors.Is(err, ErrHistoryRecoveryRequired) || page.Complete || len(page.Entries) != 0 {
		t.Fatal("row-only private marker produced a page proof", page, err)
	}
	requireHistoryContents(t, s, key, "unproven")
}

func TestHistoryWarmMetadataDoesNotProbeMarkerFilesystem(t *testing.T) {
	s := newTestStore(t)
	for _, key := range []string{"cache-a", "cache-b", "cache-c"} {
		if err := s.writeMeta(key, SessionMeta{Key: key, Summary: "cached"}); err != nil {
			t.Fatal(err)
		}
		// A marker probe is observable as ELOOP, without stubbing cache behavior.
		path := s.historyRecoveryRequiredPath(key)
		if err := os.Symlink(filepath.Base(path), path); err != nil {
			t.Fatal(err)
		}
		if meta, err := s.GetSessionMeta(t.Context(), key); err != nil || meta.Summary != "cached" {
			t.Fatal("hot metadata attempted marker filesystem I/O", meta, err)
		}
		if err := s.SetSummary(t.Context(), key, "updated"); err != nil {
			t.Fatal("write-through metadata attempted marker filesystem I/O", err)
		}
		metaBytes, err := os.ReadFile(s.metaPath(key))
		if err != nil || !json.Valid(metaBytes) {
			t.Fatal("summary update was not persisted", err)
		}
		// Proof boundaries still check the filesystem and fail closed.
		if _, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); err == nil {
			t.Fatal("a proof request ignored its unreadable marker")
		}
	}
	if metas := s.ListSessionMetas(); len(metas) != 3 {
		t.Fatal("a hot listing probed recovery markers", metas)
	}
}

func TestHistoryUnownedTailContentMutationsKeepCaptureFenced(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*JSONLStore, string) error
		want   []string
	}{
		{"set-identical", func(s *JSONLStore, key string) error {
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				return err
			}
			return s.SetHistory(t.Context(), key, history)
		}, []string{"synthetic-0", "synthetic-1", "durable legacy tail"}},
		{"set-suffix", func(s *JSONLStore, key string) error {
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				return err
			}
			return s.SetHistory(t.Context(), key, append(history, providers.Message{Role: "user", Content: "suffix"}))
		}, []string{"synthetic-0", "synthetic-1", "durable legacy tail", "suffix"}},
		{"replace", func(s *JSONLStore, key string) error {
			return s.SetHistory(t.Context(), key, []providers.Message{{Role: "user", Content: "replacement"}})
		}, []string{"replacement"}},
		{
			"truncate",
			func(s *JSONLStore, key string) error { return s.TruncateHistory(t.Context(), key, 1) },
			[]string{"durable legacy tail"},
		},
		{
			"compact-noop",
			func(s *JSONLStore, key string) error { return s.Compact(t.Context(), key) },
			[]string{"synthetic-0", "synthetic-1", "durable legacy tail"},
		},
		{"clear", func(s *JSONLStore, key string) error { return s.SetHistory(t.Context(), key, nil) }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, key, _, page := historyWithExternalTail(t, false)
			if err := tt.mutate(s, key); err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, s, key, tt.want...)
			requireUnprovenPage(t, s, key, page.ResumeCursor)
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, reopened, key, tt.want...)
			requireUnprovenPage(t, reopened, key, "")
			if err := reopened.AddMessage(t.Context(), key, "assistant", "after mutation"); err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, reopened, key, append(tt.want, "after mutation")...)
			requireUnprovenPage(t, reopened, key, "")
		})
	}
}

func TestHistoryUnownedAliasPromotionPreservesContentAndFence(t *testing.T) {
	for _, sourceMarked := range []bool{true, false} {
		t.Run(map[bool]string{true: "source-marked", false: "destination-marked"}[sourceMarked], func(t *testing.T) {
			s, markedKey, _, _ := historyWithExternalTail(t, false)
			alias, canonical := markedKey, "canonical-content-only"
			want := []string{"synthetic-0", "synthetic-1", "durable legacy tail"}
			if !sourceMarked {
				canonical, alias = markedKey, "healthy-alias"
				if err := s.SetHistory(t.Context(), canonical, nil); err != nil {
					t.Fatal(err)
				}
				if err := s.AddMessage(t.Context(), alias, "user", "healthy alias content"); err != nil {
					t.Fatal(err)
				}
				want = []string{"healthy alias content"}
			}
			preserved, err := os.ReadFile(s.jsonlPath(alias))
			if err != nil {
				t.Fatal(err)
			}
			promoted, err := s.PromoteAliasHistory(t.Context(), canonical, nil, []string{alias})
			if err != nil || !promoted {
				t.Fatal("alias content unavailable", promoted, err)
			}
			requireHistoryContents(t, s, canonical, want...)
			requireUnprovenPage(t, s, canonical, "")
			requireUnprovenPage(t, s, alias, "")
			if raw, err := os.ReadFile(s.jsonlPath(alias)); err != nil || !bytes.Equal(raw, preserved) {
				t.Fatal("alias promotion erased its source", err)
			}
			if err := s.SetHistory(t.Context(), canonical, nil); err != nil {
				t.Fatal(err)
			}
			requireUnprovenPage(t, s, canonical, "")
		})
	}
}

func TestHistoryUnownedRewriteRecoversActualOldMetadata(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "OLD", true: "NEW"}[committed], func(t *testing.T) {
			s, key, preserved, _ := historyWithExternalTail(t, false)
			sentinel := errors.New("metadata rewrite interrupted")
			failed := false
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) && !failed {
					failed = true
					if committed {
						if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
							return err
						}
					}
					return sentinel
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			if err := s.SetHistory(
				t.Context(),
				key,
				[]providers.Message{{Role: "user", Content: "replacement"}},
			); !errors.Is(
				err,
				sentinel,
			) {
				t.Fatal("metadata failure disappeared", err)
			}
			raw, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				requireHistoryContents(t, s, key, "replacement")
			} else if !bytes.Equal(raw, preserved) {
				t.Fatal("OLD recovery lost external complete rows", string(raw))
			}
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			requireUnprovenPage(t, reopened, key, "")
			if err := reopened.SetHistory(
				t.Context(),
				key,
				[]providers.Message{{Role: "user", Content: "retry"}},
			); err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, reopened, key, "retry")
			requireUnprovenPage(t, reopened, key, "")
		})
	}
}

func TestHistoryObservedMarkerMustBeDurableBeforeMutation(t *testing.T) {
	s, key, preserved, _ := historyWithExternalTail(t, false)
	sentinel := errors.New("failed after creating marker")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		err := fileutil.WriteFileAtomic(path, data, mode)
		if err == nil && path == s.historyRecoveryRequiredPath(key) {
			return sentinel
		}
		return err
	}
	if err := s.AddMessage(t.Context(), key, "assistant", "not committed"); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	requireUnprovenPage(t, s, key, "")
	marker := s.historyRecoveryRequiredPath(key)
	if err := os.Chmod(marker, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o600) })
	s.writeFile = nil
	if err := s.SetHistory(t.Context(), key, nil); err == nil {
		t.Fatal("observing a failed marker write falsely proved it durable")
	}
	if raw, err := os.ReadFile(s.jsonlPath(key)); err != nil || !bytes.Equal(raw, preserved) {
		t.Fatal("failed marker sync authorized content mutation", err)
	}
	if err := os.Chmod(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHistory(t.Context(), key, nil); err != nil {
		t.Fatal(err)
	}
	requireHistoryContents(t, s, key)
	requireUnprovenPage(t, s, key, "")
}
