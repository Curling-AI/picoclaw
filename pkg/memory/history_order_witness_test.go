package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func witnessedHistory(t *testing.T) (*JSONLStore, string, []CommittedHistoryMessage) {
	t.Helper()
	s := newTestStore(t)
	key := "sk_v1_established_order_witness"
	receipts := make([]CommittedHistoryMessage, 0, 3)
	for _, content := range []string{"first", "second", "third"} {
		receipt, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	return s, key, receipts
}

func TestHistoryOrderWitnessPrecedesFirstProofAndKeepsLegacyBootstrap(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_witnessed_legacy_bootstrap"
	legacy := []byte("{\"role\":\"user\",\"content\":\"genuine legacy\"}\n")
	if err := os.WriteFile(s.jsonlPath(key), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.writeMeta(key, SessionMeta{Key: key, Count: 1}); err != nil {
		t.Fatal(err)
	}
	created := false
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyOrderWitnessPath(key) {
			if _, err := os.Stat(s.historyRecoveryPath(key)); err != nil {
				t.Fatal("witness was written before its journal", err)
			}
			current, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(current, legacy) {
				t.Fatal("new proof bytes preceded the witness", err)
			}
			created = true
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	receipt, err := s.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "new proof"})
	if err != nil || !receipt.Order.Valid() || !created {
		t.Fatal("genuine legacy bootstrap did not establish its witness", receipt, err)
	}
	state, err := s.readHistoryOrderWitness(key, true)
	if err != nil || !state.present || !state.synced || state.witness.StreamID != receipt.Order.StreamID {
		t.Fatal("returned proof has no durable witness", state, err)
	}
	if info, statErr := os.Stat(s.historyOrderWitnessPath(key)); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("witness is not private", info, statErr)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(read) != 2 || read[0].LegacyBeforeStreamID != receipt.Order.StreamID {
		t.Fatal("witness blocked genuine legacy origin", read, err)
	}
}

func TestHistoryOrderWitnessKeepsKnownModernLossUnavailable(t *testing.T) {
	for _, damage := range []string{"missing witness", "corrupt witness", "foreign witness", "missing metadata", "lost counter"} {
		t.Run(damage, func(t *testing.T) {
			s, key, _ := witnessedHistory(t)
			switch damage {
			case "missing witness":
				if err := os.Remove(s.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
			case "corrupt witness":
				if err := os.WriteFile(s.historyOrderWitnessPath(key), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign witness":
				meta, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				witness := witnessForHistory(meta)
				witness.StreamID = newHistoryID()
				data, err := json.Marshal(witness)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.historyOrderWitnessPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing metadata":
				if err := os.Remove(s.metaPath(key)); err != nil {
					t.Fatal(err)
				}
			case "lost counter":
				data, err := os.ReadFile(s.metaPath(key))
				if err != nil {
					t.Fatal(err)
				}
				var meta SessionMeta
				if decodeErr := json.Unmarshal(data, &meta); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				meta.HistorySequence = 0
				data, err = json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, s, key, "first", "second", "third")
			requireContentRecoveryFence(t, s, key)
			if err := s.AddMessage(t.Context(), key, "user", "still available"); err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, s, key, "first", "second", "third", "still available")
			requireContentRecoveryFence(t, s, key)
		})
	}
}

func TestHistoryOrderWitnessMigrationRetainsExactStreamAndMessages(t *testing.T) {
	for _, item := range []struct {
		outcome   string
		truncated bool
	}{{"success", false}, {"OLD", false}, {"NEW", false}, {"OLD", true}, {"NEW", true}} {
		name := item.outcome
		if item.truncated {
			name += " after logical truncate"
		}
		t.Run(name, func(t *testing.T) {
			s, key, expected := witnessedHistory(t)
			if item.truncated {
				if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
					t.Fatal(err)
				}
				expected = expected[1:]
			}
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.HistoryOrderWitnessVersion = 0
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(s.metaPath(key), data, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
				t.Fatal(removeErr)
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("migration metadata failure")
			failed := false
			if item.outcome != "success" {
				s.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == s.metaPath(key) && !failed {
						failed = true
						if item.outcome == "NEW" {
							if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
								return writeErr
							}
						}
						return sentinel
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
			}
			read, readErr := s.ReadHistoryWithReceipts(t.Context(), key)
			if item.outcome == "OLD" {
				if !errors.Is(readErr, sentinel) || len(read) != 0 {
					t.Fatal("uncommitted migration exposed receipts", read, readErr)
				}
				s.writeFile = nil
				read, readErr = s.ReadHistoryWithReceipts(t.Context(), key)
			}
			if readErr != nil || !reflect.DeepEqual(read, expected) {
				t.Fatal("migration changed the stream or reattested an event", read, readErr)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("witness migration rewrote transcript bytes", err)
			}
			current, err := s.GetSessionMeta(t.Context(), key)
			if err != nil || current.HistorySequence != 3 ||
				current.HistoryOrderWitnessVersion != historyOrderWitnessVersion {
				t.Fatal("migration allocated order or omitted its durable marker", current, err)
			}
		})
	}
}

func TestHistoryOrderWitnessWriteAndSyncFailuresNeverPublishFirstProof(t *testing.T) {
	for _, failure := range []string{"before write", "after write", "sync"} {
		t.Run(failure, func(t *testing.T) {
			s := newTestStore(t)
			key := "sk_v1_witness_failure"
			sentinel := errors.New("witness interrupted")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path != s.historyOrderWitnessPath(key) {
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				if failure != "before write" {
					if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
						return err
					}
				}
				if failure == "sync" {
					return os.Chmod(path, 0)
				}
				return sentinel
			}
			receipt, err := s.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "user", Content: "must not publish"},
			)
			if err == nil || receipt.Order.Valid() {
				t.Fatal("witness failure published first order", receipt, err)
			}
			if err := os.Chmod(s.historyOrderWitnessPath(key), 0o600); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			s.writeFile = nil
			requireHistoryContents(t, s, key)
			if err := s.AddMessage(t.Context(), key, "user", "content remains available"); err != nil {
				t.Fatal(err)
			}
			requireHistoryContents(t, s, key, "content remains available")
			requireContentRecoveryFence(t, s, key)
		})
	}
}

func TestHistoryOrderWitnessMigrationResyncsObservedWitnessAfterColdOldRecovery(t *testing.T) {
	s, key, expected := witnessedHistory(t)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.HistoryOrderWitnessVersion = 0
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(s.metaPath(key), data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
		t.Fatal(removeErr)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyOrderWitnessPath(key) {
			// Simulate the atomic writer's direct-write fallback without a
			// completed sync. It stays readable across a cold reopen, but
			// activating the witness must still be unable to sync it.
			return os.WriteFile(path, data, 0o400)
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	for attempt := range 2 {
		read, readErr := s.ReadHistoryWithReceipts(t.Context(), key)
		if !errors.Is(readErr, os.ErrPermission) || len(read) != 0 {
			t.Fatal("observed but unsynced witness activated receipts", attempt, read, readErr)
		}
		stored, metaErr := s.readRecoveryMeta(key)
		if metaErr != nil || stored.HistoryOrderWitnessVersion != 0 ||
			stored.HistoryStreamID != meta.HistoryStreamID || stored.HistorySequence != meta.HistorySequence {
			t.Fatal("failed witness sync did not retain exact OLD stream", attempt, stored, metaErr)
		}
		if _, statErr := os.Stat(s.historyRecoveryPath(key)); !os.IsNotExist(statErr) {
			t.Fatal("OLD recovery left its owning journal", attempt, statErr)
		}
		requireHistoryContents(t, s, key, "first", "second", "third")
		s, metaErr = NewJSONLStore(s.dir)
		if metaErr != nil {
			t.Fatal(metaErr)
		}
	}
	if chmodErr := os.Chmod(s.historyOrderWitnessPath(key), 0o600); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	read, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || !reflect.DeepEqual(read, expected) {
		t.Fatal("explicit sync repair changed established receipts", read, err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("witness repair rewrote transcript bytes", err)
	}
}

func TestHistoryOrderWitnessHealthyDeleteAndOldDeleteRecreate(t *testing.T) {
	for _, oldDelete := range []bool{false, true} {
		t.Run(map[bool]string{false: "aware", true: "old"}[oldDelete], func(t *testing.T) {
			s, key, original := witnessedHistory(t)
			if oldDelete {
				for _, path := range []string{s.jsonlPath(key), s.metaPath(key)} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			} else if err := s.removeSessionFiles(sanitizeKey(key)); err != nil {
				t.Fatal(err)
			}
			var err error
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if oldDelete {
				if err := s.AddMessage(t.Context(), key, "user", "new content only"); err != nil {
					t.Fatal(err)
				}
				requireHistoryContents(t, s, key, "new content only")
				requireContentRecoveryFence(t, s, key)
			} else {
				receipt, err := s.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{Role: "user", Content: "new session"},
				)
				if err != nil || !receipt.Order.Valid() || receipt.Order.Sequence != 1 ||
					receipt.Order.StreamID == original[0].Order.StreamID {
					t.Fatal("confirmed healthy deletion did not permit a new unrelated stream", receipt, err)
				}
			}
		})
	}
}

func TestHistoryOrderWitnessUncommittedFailureKeepsContentAppendAvailable(t *testing.T) {
	for _, receiptFirst := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "content append", true: "receipt then fallback"}[receiptFirst],
			func(t *testing.T) {
				s, key, _ := witnessedHistory(t)
				meta, err := s.readMeta(key)
				if err != nil {
					t.Fatal(err)
				}
				meta.HistoryOrderWitnessVersion = 0
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if writeErr := os.WriteFile(s.metaPath(key), data, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
				if chmodErr := os.Chmod(s.historyOrderWitnessPath(key), 0o400); chmodErr != nil {
					t.Fatal(chmodErr)
				}
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
				if receiptFirst {
					receipt, appendErr := s.AppendHistoryReceipt(
						t.Context(),
						key,
						providers.Message{Role: "assistant", Content: "still delivered"},
					)
					if !errors.Is(appendErr, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
						t.Fatal("confirmed OLD did not permit pre-write content fallback", receipt, appendErr)
					}
					requireHistoryContents(t, s, key, "first", "second", "third")
				}
				if appendErr := s.AddMessage(t.Context(), key, "assistant", "still delivered"); appendErr != nil {
					t.Fatal("witness proof failure blocked ordinary content", appendErr)
				}
				requireHistoryContents(t, s, key, "first", "second", "third", "still delivered")
				requireContentRecoveryFence(t, s, key)
				stored, err := s.readRecoveryMeta(key)
				if err != nil || stored.HistoryStreamID != meta.HistoryStreamID ||
					stored.HistorySequence != meta.HistorySequence {
					t.Fatal("content fallback committed an unproven stream or sequence", stored, err)
				}
			},
		)
	}
}

func TestHistoryOrderWitnessMigrationFailureKeepsContentLifecycleAvailable(t *testing.T) {
	for _, operation := range []string{"replace", "compact"} {
		t.Run(operation, func(t *testing.T) {
			s, key, _ := witnessedHistory(t)
			if operation == "compact" {
				if err := s.TruncateHistory(t.Context(), key, 2); err != nil {
					t.Fatal(err)
				}
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.HistoryOrderWitnessVersion = 0
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(s.metaPath(key), data, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if chmodErr := os.Chmod(s.historyOrderWitnessPath(key), 0o400); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "replace" {
				if rewriteErr := s.SetHistory(
					t.Context(),
					key,
					[]providers.Message{{Role: "user", Content: "edited content"}},
				); rewriteErr != nil {
					t.Fatal("witness migration failure blocked requested content edit", rewriteErr)
				}
				requireHistoryContents(t, s, key, "edited content")
			} else {
				if compactErr := s.Compact(t.Context(), key); compactErr != nil {
					t.Fatal("witness migration failure blocked requested compaction", compactErr)
				}
				requireHistoryContents(t, s, key, "second", "third")
			}
			requireContentRecoveryFence(t, s, key)
			stored, err := s.readRecoveryMeta(key)
			if err != nil || stored.HistoryStreamID != meta.HistoryStreamID ||
				stored.HistorySequence != meta.HistorySequence {
				t.Fatal("content lifecycle invented a replacement stream or counter", stored, err)
			}
		})
	}
}

func TestHistoryOrderWitnessLossAfterCommitCannotRequestPrewriteFallback(t *testing.T) {
	s, key, _ := witnessedHistory(t)
	sentinel := errors.New("metadata reported failure after commit")
	failed := false
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
			return err
		}
		if path == s.metaPath(key) && !failed {
			failed = true
			if err := os.Remove(s.historyOrderWitnessPath(key)); err != nil {
				return err
			}
			return sentinel
		}
		return nil
	}
	receipt, err := s.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "already durable"},
	)
	if err == nil || receipt.Order.Valid() || !errors.Is(err, ErrHistoryOrderUnavailable) ||
		errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("committed write was acknowledged or allowed a pre-write retry", receipt, err)
	}
	requireHistoryContents(t, s, key, "first", "second", "third", "already durable")
	requireContentRecoveryFence(t, s, key)
}

func TestHistoryOrderWitnessUnconfirmedRecoveryCannotRequestContentFallback(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_witness_uncertain_rollback"
	sentinel := errors.New("OLD metadata restoration failed")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyOrderWitnessPath(key) {
			return os.WriteFile(path, data, 0o400)
		}
		if path == s.metaPath(key) {
			return sentinel
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	receipt, err := s.AppendHistoryReceipt(
		t.Context(), key, providers.Message{Role: "assistant", Content: "unconfirmed"},
	)
	if err == nil || receipt.Order.Valid() || !errors.Is(err, ErrHistoryOrderUnavailable) ||
		errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("incomplete OLD recovery permitted a content retry", receipt, err)
	}
	if _, statErr := os.Stat(s.historyRecoveryPath(key)); statErr != nil {
		t.Fatal("unconfirmed recovery lost its owning journal", statErr)
	}
	data, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || len(data) != 0 {
		t.Fatal("witness failure reached the input bytes", string(data), err)
	}
}

func TestHistoryOrderWitnessProofBoundaryRechecksWarmWitness(t *testing.T) {
	for _, operation := range []string{"read", "append", "page"} {
		t.Run(operation, func(t *testing.T) {
			s, key, _ := witnessedHistory(t)
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
				t.Fatal(removeErr)
			}
			if operation == "page" {
				if _, pageErr := s.ReadHistoryPage(
					t.Context(),
					HistoryPageRequest{Key: key},
				); !errors.Is(
					pageErr,
					ErrHistoryRecoveryRequired,
				) {
					t.Fatal("warm page ignored a missing required witness", pageErr)
				}
			} else if operation == "read" {
				if _, receiptErr := s.ReadHistoryWithReceipts(
					t.Context(),
					key,
				); !errors.Is(
					receiptErr,
					ErrHistoryReceiptUnavailable,
				) {
					t.Fatal("warm proof reader ignored a missing required witness", receiptErr)
				}
			} else {
				receipt, appendErr := s.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: "must not commit"},
				)
				if !errors.Is(appendErr, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
					t.Fatal("warm receipt append ignored a missing required witness", receipt, appendErr)
				}
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("proof boundary altered data after losing its witness", err)
			}
		})
	}
}
