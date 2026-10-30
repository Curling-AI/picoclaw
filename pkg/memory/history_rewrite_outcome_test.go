package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryRewriteReportsAppliedNEWWithoutIssuingOrderProof(t *testing.T) {
	for _, stage := range []string{"before write", "decision write", "decision read", "witness probe", "unknown checkpoint", "OLD bytes with NEW metadata", "changed bytes with NEW metadata"} {
		t.Run(stage, func(t *testing.T) {
			s := newTestStore(t)
			key := "sk_v1_rewrite_new_outcome"
			for _, msg := range []providers.Message{{Role: "user", Content: "retained input"}, {Role: "assistant", Content: "delivery removed by rollback", Delivery: &providers.Delivery{Origin: "cron:rewrite"}}} {
				if _, err := s.AppendHistoryReceipt(t.Context(), key, msg); err != nil {
					t.Fatal(err)
				}
			}
			const summary = "canonical summary"
			if err := s.SetSummary(t.Context(), key, summary); err != nil {
				t.Fatal(err)
			}
			before, err := s.ReadHistoryWithReceipts(t.Context(), key)
			if err != nil || len(before) != 2 {
				t.Fatal(before, err)
			}
			oldData, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			witness, err := os.ReadFile(s.historyOrderWitnessPath(key))
			if err != nil {
				t.Fatal(err)
			}
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if stage == "before write" && path == s.jsonlPath(key) {
					return syscall.EIO
				}
				if (stage == "decision write" || stage == "OLD bytes with NEW metadata" || stage == "changed bytes with NEW metadata") &&
					path == s.historyDecisionPath(key) {
					return syscall.EIO
				}
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == s.metaPath(key) {
					switch stage {
					case "witness probe":
						if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
							return removeErr
						}
						if mkdirErr := os.Mkdir(s.historyOrderWitnessPath(key), 0o700); mkdirErr != nil {
							return mkdirErr
						}
					case "decision read":
						if writeErr := os.WriteFile(s.historyDecisionPath(key), []byte("{"), 0o600); writeErr != nil {
							return writeErr
						}
					case "OLD bytes with NEW metadata":
						if writeErr := os.WriteFile(s.jsonlPath(key), oldData, 0o600); writeErr != nil {
							return writeErr
						}
					case "changed bytes with NEW metadata":
						currentData, sourceErr := os.ReadFile(s.jsonlPath(key))
						if sourceErr != nil {
							return sourceErr
						}
						changedData := bytes.ReplaceAll(currentData, []byte("retained input"), []byte("retained bogus"))
						if writeErr := os.WriteFile(s.jsonlPath(key), changedData, 0o600); writeErr != nil {
							return writeErr
						}
					case "unknown checkpoint":
						var meta SessionMeta
						if decodeErr := json.Unmarshal(data, &meta); decodeErr != nil {
							return decodeErr
						}
						meta.Count++
						changed, encodeErr := json.Marshal(meta)
						if encodeErr != nil {
							return encodeErr
						}
						if writeErr := fileutil.WriteFileAtomic(path, changed, mode); writeErr != nil {
							return writeErr
						}
					}
					if stage != "before write" {
						return syscall.EIO
					}
				}
				return nil
			}
			err = s.SetHistory(t.Context(), key, []providers.Message{before[0].Message})
			applied := stage == "decision write" || stage == "decision read" || stage == "witness probe"
			if !errors.Is(err, syscall.EIO) || errors.Is(err, ErrHistoryInputCommitted) != applied {
				t.Errorf("rewrite outcome lost source NEW or original I/O cause: stage=%s error=%v", stage, err)
			}
			data, readErr := os.ReadFile(s.jsonlPath(key))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if stage == "before write" && !bytes.Equal(data, oldData) {
				t.Fatal("failed pre-write replaced OLD history")
			}
			if applied && bytes.Contains(data, []byte("delivery removed by rollback")) {
				t.Fatal("fixture did not apply the rewrite before its operational failure")
			}
			if stage == "unknown checkpoint" || stage == "OLD bytes with NEW metadata" ||
				stage == "changed bytes with NEW metadata" {
				if _, statErr := os.Stat(s.historyRecoveryPath(key)); statErr != nil {
					t.Fatal("unproved outcome discarded authoritative journal", statErr)
				}
				if _, receiptErr := s.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
					t.Fatal("unknown checkpoint emitted order proof")
				}
				return
			}
			if stage == "decision write" {
				decision, decisionErr := s.recoverHistoryWriteDecision(key)
				if decision != historyWriteUnknown || !errors.Is(decisionErr, syscall.EIO) {
					t.Fatal("content NEW was mislabeled as a durable recovery decision", decision, decisionErr)
				}
				if _, receiptErr := s.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
					t.Fatal("failed decision publication emitted order proof")
				}
			}
			s.writeFile = nil
			if stage == "decision read" {
				if _, receiptErr := s.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
					t.Fatal("unreadable decision emitted order proof")
				}
				if removeErr := os.Remove(s.historyDecisionPath(key)); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			if stage == "witness probe" {
				var witnessErr *os.PathError
				if !errors.As(err, &witnessErr) || witnessErr.Path != s.historyOrderWitnessPath(key) ||
					witnessErr.Op != "read" {
					t.Error("witness failure lost its actual I/O cause", err)
				}
				if _, receiptErr := s.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
					t.Fatal("missing witness emitted order proof")
				}
				if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
					t.Fatal(removeErr)
				}
				if writeErr := fileutil.WriteFileAtomic(
					s.historyOrderWitnessPath(key),
					witness,
					0o600,
				); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			fresh, restartErr := NewJSONLStore(s.dir)
			if restartErr != nil {
				t.Fatal(restartErr)
			}
			actual, readErr := fresh.ReadHistoryWithReceipts(t.Context(), key)
			expected := before[:1]
			if stage == "before write" {
				expected = before
			}
			if readErr != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatal(
					"recovery changed retained content, occurrence IDs or existing order",
					actual,
					expected,
					readErr,
				)
			}
			meta, metaErr := fresh.readMeta(key)
			if metaErr != nil || meta.Summary != summary || meta.Count != len(expected) {
				t.Fatal("rewrite/recovery changed canonical metadata", meta, metaErr)
			}
		})
	}
}

func TestHistoryRewritePendingNEWCannotConfirmAnotherInput(t *testing.T) {
	s := newTestStore(t)
	key := "sk_v1_pending_new_other_input"
	for _, text := range []string{"keep", "remove"} {
		if _, err := s.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "user", Content: text},
		); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyDecisionPath(key) {
			return syscall.EIO
		}
		if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
			return writeErr
		}
		if path == s.metaPath(key) {
			return syscall.EIO
		}
		return nil
	}
	if rewriteErr := s.SetHistory(
		t.Context(),
		key,
		[]providers.Message{before[0].Message},
	); !errors.Is(
		rewriteErr,
		ErrHistoryInputCommitted,
	) {
		t.Fatal("fixture did not leave prior NEW pending", rewriteErr)
	}
	retained, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	input := providers.Message{Role: "assistant", Content: "different input that must not be claimed"}
	for _, operation := range []string{"plain append", "receipt append", "typed append", "input append"} {
		t.Run(operation, func(t *testing.T) {
			var appendErr error
			switch operation {
			case "plain append":
				appendErr = s.AddFullMessage(t.Context(), key, input)
			case "receipt append":
				receipt, receiptErr := s.AppendHistoryReceipt(t.Context(), key, input)
				appendErr = receiptErr
				if receipt.Message.Role != "" || receipt.Order.Valid() {
					t.Error("pending prior write attested a different input", receipt)
				}
			case "typed append":
				result, resultErr := s.AppendHistoryContent(t.Context(), key, input)
				appendErr = resultErr
				if result.Entry.Message.Role != "" || result.Receipt != nil {
					t.Error("pending prior write confirmed another typed input", result)
				}
			case "input append":
				result, resultErr := s.AppendHistoryContentInput(t.Context(), key, input)
				appendErr = resultErr
				if result.Entry.Message.Role != "" || result.Receipt != nil {
					t.Error("pending prior write confirmed another input", result)
				}
			}
			if !errors.Is(appendErr, syscall.EIO) || errors.Is(appendErr, ErrHistoryInputCommitted) {
				t.Error("cold recovery of prior NEW misreported the current unexecuted input", appendErr)
			}
			actual, readErr := os.ReadFile(s.jsonlPath(key))
			if readErr != nil || !bytes.Equal(actual, retained) {
				t.Fatal("failed append preflight changed source", readErr)
			}
		})
	}
}

func TestHistoryWriteObservationFailureRetainsOnlyDecidedNEW(t *testing.T) {
	cases := []struct {
		name        string
		decided     bool
		appendInput bool
		observation string
	}{
		{"unknown metadata rewrite", false, false, "metadata"},
		{"unknown JSONL rewrite", false, false, "JSONL"},
		{"NEW metadata rewrite", true, false, "metadata"},
		{"NEW JSONL rewrite", true, false, "JSONL"},
		{"unknown metadata append", false, true, "metadata"},
		{"NEW metadata append", true, true, "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			key := "sk_v1_decided_new_observation"
			for _, text := range []string{"retained", "removed by rollback"} {
				if _, err := s.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{Role: "user", Content: text},
				); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ReadHistoryWithReceipts(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			target := s.jsonlPath(key)
			if tc.observation == "metadata" {
				target = s.metaPath(key)
			}
			backup := target + ".read-failure-backup"
			poisoned := false
			var written []CommittedHistoryMessage
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.historyDecisionPath(key) {
					if !poisoned {
						raw, sourceErr := os.ReadFile(s.jsonlPath(key))
						if sourceErr != nil {
							return sourceErr
						}
						var decodeErr error
						written, decodeErr = committedHistoryMessages(raw)
						if decodeErr != nil {
							return decodeErr
						}
						if moveErr := os.Rename(target, backup); moveErr != nil {
							return moveErr
						}
						if mkdirErr := os.Mkdir(target, 0o700); mkdirErr != nil {
							return mkdirErr
						}
						poisoned = true
					}
					if !tc.decided {
						return syscall.EIO
					}
				}
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == s.metaPath(key) {
					return syscall.EIO
				}
				return nil
			}
			if tc.appendInput {
				err = s.AddFullMessage(
					t.Context(),
					key,
					providers.Message{
						Role:     "assistant",
						Content:  "one new durable delivery",
						Delivery: &providers.Delivery{Origin: "cron:observation"},
					},
				)
			} else {
				err = s.SetHistory(t.Context(), key, []providers.Message{before[0].Message})
			}
			if !poisoned {
				t.Fatal("fixture did not reach post-NEW observation")
			}
			if !errors.Is(err, syscall.EIO) || errors.Is(err, ErrHistoryInputCommitted) != tc.decided {
				t.Errorf("observation failure forgot known NEW or invented unknown NEW: %v", err)
			}
			var pathErr *os.PathError
			if (!tc.appendInput || tc.decided) &&
				(!errors.As(err, &pathErr) || pathErr.Path != target || pathErr.Op != "read") {
				t.Error("lost later observation cause", err)
			}
			_, journalErr := os.Stat(s.historyRecoveryPath(key))
			if tc.decided && !errors.Is(journalErr, os.ErrNotExist) {
				t.Error("fixture did not finish the decided NEW journal", journalErr)
			}
			if !tc.decided && journalErr != nil {
				t.Error("unknown outcome discarded its authoritative journal", journalErr)
			}
			visible, receiptErr := s.ReadHistoryWithReceipts(t.Context(), key)
			if tc.decided && tc.observation == "metadata" {
				if receiptErr == nil && !reflect.DeepEqual(visible, written) {
					t.Error("warm known checkpoint invented receipt identity", visible, written)
				}
				cold, coldErr := NewJSONLStore(s.dir)
				if coldErr != nil {
					t.Fatal(coldErr)
				}
				if _, coldErr = cold.ReadHistoryWithReceipts(t.Context(), key); coldErr == nil {
					t.Error("cold source without metadata emitted order proof")
				}
			} else if receiptErr == nil {
				t.Error("unavailable source emitted order proof")
			}
			s.writeFile = nil
			if removeErr := os.Remove(target); removeErr != nil {
				t.Fatal(removeErr)
			}
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			fresh, restartErr := NewJSONLStore(s.dir)
			if restartErr != nil {
				t.Fatal(restartErr)
			}
			actual, readErr := fresh.ReadHistoryWithReceipts(t.Context(), key)
			if readErr != nil || !reflect.DeepEqual(actual, written) {
				t.Fatal("observation recovery changed durable IDs, body or order proof", actual, written, readErr)
			}
			retained := before[:1]
			if tc.appendInput {
				retained = before
			}
			if !reflect.DeepEqual(actual[:len(retained)], retained) {
				t.Fatal("observation failure reattested retained messages", actual, retained)
			}
		})
	}
}
