package memory

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryPendingRollbackSurvivesListingAndAppend(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX write permissions enforced for a non-root user")
	}
	for _, committed := range []bool{false, true} {
		name := "old"
		if committed {
			name = "new"
		}
		t.Run(name, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:main:pending-rollback"
			if operationErr := s.SetHistory(
				t.Context(),
				key,
				[]providers.Message{{Role: "user", Content: "committed"}},
			); operationErr != nil {
				t.Fatal(operationErr)
			}
			before, err := s.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			history, err := s.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			restore := func() {
				_ = os.Chmod(s.dir, 0o755)
				_ = os.Chmod(s.jsonlPath(key), 0o644)
			}
			t.Cleanup(restore)
			sentinel := errors.New("injected metadata failure with rollback storage unavailable")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) {
					if chmodErr := os.Chmod(s.jsonlPath(key), 0o444); chmodErr != nil {
						return chmodErr
					}
					if chmodErr := os.Chmod(s.dir, 0o555); chmodErr != nil {
						return chmodErr
					}
					return sentinel
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			history[0].Content = "discarded"
			if operationErr := s.SetHistory(t.Context(), key, history); !errors.Is(operationErr, sentinel) {
				t.Fatal("expected the metadata write and rollback to fail", operationErr)
			}
			journal, err := os.ReadFile(s.historyRecoveryPath(key))
			if err != nil {
				t.Fatal("failed recovery lost its backup", err)
			}
			var recovery historyWriteRecovery
			if decodeErr := json.Unmarshal(journal, &recovery); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			restore()
			s.writeFile = nil
			wantContent, wantGeneration := "committed", before.HistoryGeneration
			if committed {
				// The metadata commit can have landed even when the writer reported
				// an error. Recovery must then publish NEW, replacing any stale cache.
				if writeErr := s.writeMeta(key, recovery.Next); writeErr != nil {
					t.Fatal(writeErr)
				}
				s.storeCachedMeta(sanitizeKey(key), before)
				wantContent, wantGeneration = "discarded", recovery.Next.HistoryGeneration
			}
			metas := s.ListSessionMetas()
			if len(metas) != 1 || metas[0].HistoryGeneration != wantGeneration {
				t.Errorf("listing exposed metadata before recovery chose its state: %+v", metas)
			}
			if operationErr := s.AddMessage(t.Context(), key, "assistant", "next append"); operationErr != nil {
				t.Fatal("next append failed", operationErr)
			}
			actual, err := s.GetHistory(t.Context(), key)
			if err != nil || len(actual) != 2 || actual[0].Content != wantContent ||
				actual[1].Content != "next append" {
				t.Errorf("pending recovery was overwritten by a successful append: %+v %v", actual, err)
			}
			page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil || page.Generation != wantGeneration || !page.Complete || len(page.Entries) != 2 {
				t.Fatal("successful append left the recovered source unreadable", page, err)
			}
			if _, statErr := os.Stat(s.historyRecoveryPath(key)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("completed recovery retained its journal", statErr)
			}
		})
	}
}

func TestHistoryListingWaitsForActiveWriter(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:main:active-history-writer"
	if operationErr := s.SetHistory(
		t.Context(),
		key,
		[]providers.Message{{Role: "user", Content: "original"}},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	blocked, proceed := make(chan struct{}), make(chan struct{})
	var firstWrite atomic.Bool
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.metaPath(key) && firstWrite.CompareAndSwap(false, true) {
			close(blocked)
			<-proceed
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	writer := make(chan error, 1)
	history[0].Content = "confirmed edit"
	go func() { writer <- s.SetHistory(t.Context(), key, history) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach its metadata boundary")
	}
	listed := make(chan []SessionMeta, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		listed <- s.ListSessionMetas()
	}()
	<-started
	select {
	case metas := <-listed:
		close(proceed)
		<-writer
		t.Fatal("listing bypassed the active writer's session lock", metas)
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	if operationErr := <-writer; operationErr != nil {
		t.Fatal(operationErr)
	}
	select {
	case metas := <-listed:
		if len(metas) != 1 || metas[0].HistoryGeneration == before.HistoryGeneration {
			t.Fatal("listing retained stale pre-commit metadata", metas)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listing did not resume after the writer completed")
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message.Content != "confirmed edit" {
		t.Fatal("listing recovered a journal belonging to an active writer", page, err)
	}
}
