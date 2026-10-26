package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestTruncateHistoryExcludesAnUnterminatedLegacyRow(t *testing.T) {
	s, key := pagedStore(t, 0)
	raw := []byte(
		"{\"role\":\"user\",\"content\":\"u0\"}\n{\"role\":\"assistant\",\"content\":\"a1\"}\n{\"role\":\"user\",\"content\":\"u2\"}\n{\"role\":\"assistant\",\"content\":\"uncommitted\"}",
	)
	if operationErr := os.WriteFile(s.jsonlPath(key), raw, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.writeMeta(key, SessionMeta{Count: 4}); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.TruncateHistory(t.Context(), key, 10); operationErr != nil {
		t.Fatal(operationErr)
	}
	page, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || !page.Complete || len(page.Entries) != 3 || page.TotalPosition != 3 {
		t.Fatal("truncate adopted an unterminated legacy row", page, err)
	}
	for _, refresh := range []bool{false, true} {
		resumed, resumeErr := s.ReadHistoryPage(
			t.Context(),
			HistoryPageRequest{Key: key, Cursor: page.ResumeCursor, Refresh: refresh},
		)
		if resumeErr != nil || !resumed.Complete || len(resumed.Entries) != 0 {
			t.Fatal("legacy tail retired a committed cursor", resumed, resumeErr)
		}
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "next committed"); operationErr != nil {
		t.Fatal(operationErr)
	}
	resumed, err := s.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key, Cursor: page.ResumeCursor, Refresh: true},
	)
	if err != nil || !resumed.Complete || len(resumed.Entries) != 1 || resumed.Entries[0].Position != 3 {
		t.Fatal("next append lost the committed prefix", resumed, err)
	}
}

func TestSetHistorySuffixRetiresLegacyPhysicalGaps(t *testing.T) {
	s, key := pagedStore(t, 0)
	raw := []byte("{\"role\":\"user\",\"content\":\"u0\"}\n{broken\n{\"role\":\"assistant\",\"content\":\"a1\"}\n")
	if operationErr := os.WriteFile(s.jsonlPath(key), raw, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.writeMeta(key, SessionMeta{Count: 3}); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(before.Entries) != 2 || before.Entries[1].Position != 2 {
		t.Fatal("invalid legacy fixture", before, err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, providers.Message{Role: "user", Content: "u2"})
	if operationErr := s.SetHistory(t.Context(), key, history); operationErr != nil {
		t.Fatal(operationErr)
	}
	after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(after.Entries) != 3 || after.Generation == before.Generation ||
		after.ParentGeneration != before.Generation || after.RetainedUntil != 1 ||
		after.Entries[1].Position != 1 || after.Entries[1].ID != before.Entries[1].ID {
		t.Fatal("suffix silently reused moved physical positions", after, err)
	}
}

func TestHistoryCleanupFailureFencesLaterMetadataChanges(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX directory permissions for a non-root user")
	}
	s, key := pagedStore(t, 4)
	t.Cleanup(func() { _ = os.Chmod(s.dir, 0o755) })
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if operationErr := fileutil.WriteFileAtomic(path, data, mode); operationErr != nil {
			return operationErr
		}
		if path == s.metaPath(key) {
			return os.Chmod(s.dir, 0o555)
		}
		return nil
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "confirmed append"); operationErr == nil {
		t.Fatal("unlink failure was hidden")
	}
	if decision, err := s.recoverHistoryWriteDecision(key); decision != historyWriteUnknown || err == nil {
		t.Fatal("metadata NEW was confused with a durable recovery decision", decision, err)
	}
	if operationErr := s.TruncateHistory(t.Context(), key, 2); operationErr == nil {
		t.Fatal("metadata changed while cleanup was still pending")
	}
	if operationErr := os.Chmod(s.dir, 0o755); operationErr != nil {
		t.Fatal(operationErr)
	}
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
			return writeErr
		}
		if path == s.historyDecisionPath(key) {
			return os.Chmod(s.dir, 0o555)
		}
		return nil
	}
	if decision, err := s.recoverHistoryWriteDecision(key); decision != historyWriteNew || err == nil {
		t.Fatal("unlink failure lost the durable NEW decision", decision, err)
	}
	if operationErr := os.Chmod(s.dir, 0o755); operationErr != nil {
		t.Fatal(operationErr)
	}
	s.writeFile = nil
	if operationErr := s.TruncateHistory(t.Context(), key, 2); operationErr != nil {
		t.Fatal(operationErr)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	history, err := restarted.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || history[1].Content != "confirmed append" {
		t.Fatal("restart undid a committed append and truncation", history, err)
	}
}

func TestPruneHistoryWaitsForWriterAndRemovesItsRecovery(t *testing.T) {
	for _, refreshed := range []bool{false, true} {
		name := "expired"
		if refreshed {
			name = "refreshed"
		}
		t.Run(name, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:cron-round18"
			if operationErr := s.AddMessage(t.Context(), key, "user", "cron history"); operationErr != nil {
				t.Fatal(operationErr)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := json.Marshal(historyWriteRecovery{Meta: meta, Next: meta})
			if err != nil {
				t.Fatal(err)
			}
			if operationErr := os.WriteFile(s.historyRecoveryPath(key), journal, 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			decision, err := json.Marshal(
				historyDecisionRecord{Journal: journalDigest(journal), Decision: historyWriteNew},
			)
			if err != nil {
				t.Fatal(err)
			}
			if operationErr := os.WriteFile(s.historyDecisionPath(key), decision, 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			s.markHistoryPending(key)
			old := time.Now().Add(-30 * 24 * time.Hour)
			paths := []string{s.metaPath(key), s.jsonlPath(key), s.historyRecoveryPath(key), s.historyDecisionPath(key)}
			for _, path := range paths {
				if operationErr := os.Chtimes(path, old, old); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			lock := s.sessionLock(key)
			lock.Lock()
			type result struct {
				count int
				err   error
			}
			done, started := make(chan result, 1), make(chan struct{})
			go func() {
				close(started)
				count, err := s.PruneCronRuns(7*24*time.Hour, time.Now())
				done <- result{count, err}
			}()
			<-started
			select {
			case got := <-done:
				lock.Unlock()
				t.Fatal("prune bypassed an active session writer", got)
			case <-time.After(100 * time.Millisecond):
			}
			if refreshed {
				now := time.Now()
				if operationErr := os.Chtimes(s.metaPath(key), now, now); operationErr != nil {
					lock.Unlock()
					t.Fatal(operationErr)
				}
			}
			lock.Unlock()
			got := <-done
			want := 1
			if refreshed {
				want = 0
			}
			if got.err != nil || got.count != want {
				t.Fatal("prune ignored the locked retention state", got)
			}
			for _, path := range paths {
				_, err := os.Stat(path)
				if (!refreshed && !errors.Is(err, os.ErrNotExist)) || (refreshed && err != nil) {
					t.Fatal("prune left an unexpected recovery artifact", path, err)
				}
			}
		})
	}
}

func TestHistoryUnknownCommitNeverRollsBackUnreadableMetadata(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX file permissions for a non-root user")
	}
	s, key := pagedStore(t, 2)
	t.Cleanup(func() { _ = os.Chmod(s.metaPath(key), 0o644) })
	sentinel := errors.New("metadata committed but acknowledgement lost")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if operationErr := fileutil.WriteFileAtomic(path, data, mode); operationErr != nil {
			return operationErr
		}
		if path == s.metaPath(key) {
			if operationErr := os.Chmod(path, 0); operationErr != nil {
				return operationErr
			}
			return sentinel
		}
		return nil
	}
	if operationErr := s.AddMessage(
		t.Context(),
		key,
		"assistant",
		"confirmed unreadable commit",
	); !errors.Is(
		operationErr,
		sentinel,
	) {
		t.Fatal("metadata acknowledgement failure was hidden", operationErr)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, pageErr := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); pageErr == nil {
		t.Fatal("unknown commit was exposed before metadata became readable")
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("recovery destroyed Data while commit was unknown", err)
	}
	if operationErr := os.Chmod(s.metaPath(key), 0o644); operationErr != nil {
		t.Fatal(operationErr)
	}
	page, err := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 3 || page.Entries[2].Message.Content != "confirmed unreadable commit" {
		t.Fatal("confirmed commit did not survive unknown recovery and restart", page, err)
	}
}

func TestHistoryOldDecisionSurvivesRollbackMetadataFailure(t *testing.T) {
	for _, operation := range []string{"append", "rewrite"} {
		t.Run(operation, func(t *testing.T) {
			s, key := pagedStore(t, 3)
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			failed := errors.New("metadata write failed")
			writes := 0
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.metaPath(key) {
					writes++
					if writes == 2 {
						// A late/retried meta publication must not change an OLD
						// decision once recovery has already restored old Data.
						journal, readErr := os.ReadFile(s.historyRecoveryPath(key))
						if readErr != nil {
							return readErr
						}
						var recovery historyWriteRecovery
						if operationErr := json.Unmarshal(journal, &recovery); operationErr != nil {
							return operationErr
						}
						encoded, encodeErr := json.Marshal(recovery.Next)
						if encodeErr != nil {
							return encodeErr
						}
						if operationErr := os.WriteFile(path, encoded, mode); operationErr != nil {
							return operationErr
						}
					}
					return failed
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			if operation == "append" {
				err = s.AddMessage(t.Context(), key, "assistant", "must roll back")
			} else {
				err = s.SetHistory(t.Context(), key, []providers.Message{{Role: "user", Content: "must roll back"}})
			}
			if !errors.Is(err, failed) || writes != 2 {
				t.Fatal("fixture did not interrupt OLD publication after rollback", writes, err)
			}
			restarted, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("restart changed the decided OLD transcript", err)
			}
			page, err := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil || len(page.Entries) != 3 || page.TotalPosition != 3 {
				t.Fatal("late NEW metadata overrode a persisted OLD decision", page, err)
			}
		})
	}
}

func TestHistoryConfirmedDeleteRetiresCachedGeneration(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "owned"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			s, key := pagedStore(t, 3)
			before, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			if external {
				for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
					if operationErr := os.Remove(path); operationErr != nil {
						t.Fatal(operationErr)
					}
				}
			} else if operationErr := s.removeSessionFiles(sanitizeKey(key)); operationErr != nil {
				t.Fatal(operationErr)
			}
			if operationErr := s.AddMessage(t.Context(), key, "user", "new conversation"); operationErr != nil {
				t.Fatal(operationErr)
			}
			after, err := s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
			if err != nil || len(after.Entries) != 1 || after.TotalPosition != 1 ||
				after.Generation == before.Generation || after.ParentGeneration != "" ||
				after.Entries[0].Message.Content != "new conversation" {
				t.Fatal("recreated history reused deleted state", after, err)
			}
			if _, err := s.ReadHistoryPage(
				t.Context(),
				HistoryPageRequest{Key: key, Cursor: before.ResumeCursor},
			); !errors.Is(
				err,
				ErrHistoryCursor,
			) {
				t.Fatal("deleted cursor remained valid", err)
			}
		})
	}
}

func TestHistoryDeletionDoesNotRestoreAnOrphanedRewrite(t *testing.T) {
	s, key := pagedStore(t, 2)
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: meta, Next: meta, Data: data, Existed: true},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key)} {
		if operationErr := os.Remove(path); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.metaPath(key), s.jsonlPath(key), s.historyRecoveryPath(key), s.historyDecisionPath(key)} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("recovery resurrected an externally deleted session", path, statErr)
		}
	}
	if operationErr := restarted.AddMessage(t.Context(), key, "user", "fresh after deletion"); operationErr != nil {
		t.Fatal(operationErr)
	}
	page, err := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 1 || page.Generation == meta.HistoryGeneration {
		t.Fatal("orphan cleanup retained the deleted generation", page, err)
	}
}

func TestHistoryStaleJournalPreservesConfirmedMetadataTruncation(t *testing.T) {
	s, key := pagedStore(t, 4)
	var journal []byte
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyRecoveryPath(key) {
			journal = bytes.Clone(data)
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	if operationErr := s.AddMessage(t.Context(), key, "assistant", "confirmed append"); operationErr != nil {
		t.Fatal(operationErr)
	}
	s.writeFile = nil
	// Simulate a leftover journal from a version that ignored unlink errors.
	if operationErr := os.WriteFile(s.historyRecoveryPath(key), journal, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.TruncateHistory(t.Context(), key, 2); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.SetSummary(t.Context(), key, "confirmed summary"); operationErr != nil {
		t.Fatal(operationErr)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	history, err := restarted.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || history[1].Content != "confirmed append" {
		t.Fatal("stale journal undid confirmed metadata-only changes", history, err)
	}
	if summary, err := restarted.GetSummary(t.Context(), key); err != nil || summary != "confirmed summary" {
		t.Fatal("stale journal overwrote a newer summary", summary, err)
	}
}

func TestHistoryUnrecognizedCheckpointFencesInsteadOfRollingBack(t *testing.T) {
	s, key := pagedStore(t, 2)
	old, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	offset := *old.HistoryBytes
	next := old
	next.Count++
	if operationErr := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: old, Next: next, AppendOffset: &offset},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	newer := old
	newer.Count += 2
	if operationErr := s.writeMeta(key, newer); operationErr != nil {
		t.Fatal(operationErr)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, pageErr := restarted.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); pageErr == nil {
		t.Fatal("unrecognized metadata was guessed to be OLD or NEW")
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unknown checkpoint changed transcript bytes", err)
	}
}

func TestHistoryJournalWriteFailureLeavesCommittedTranscriptUntouched(t *testing.T) {
	s, key := pagedStore(t, 2)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("journal write failed after partial direct write")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyRecoveryPath(key) {
			if writeErr := os.WriteFile(path, []byte(`{"meta":`), mode); writeErr != nil {
				return writeErr
			}
			return sentinel
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	if appendErr := s.AddMessage(t.Context(), key, "assistant", "must never start"); !errors.Is(appendErr, sentinel) {
		t.Fatal("journal failure was hidden", appendErr)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("transcript changed before its journal was saved", err)
	}
	s.writeFile = nil
	if appendErr := s.AddMessage(t.Context(), key, "assistant", "next committed"); appendErr != nil {
		t.Fatal("cleaned partial journal poisoned the next append", appendErr)
	}
}
