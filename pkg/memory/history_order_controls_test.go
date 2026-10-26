package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryReceiptRechecksOwnerAfterResolution(t *testing.T) {
	for _, operation := range []string{"append", "read"} {
		for _, change := range []string{"revoke alias", "replace owner"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				store := newTestStore(t)
				canonical, alias := "aaa_receipt_owner", "agent:main:receipt-owner"
				if _, appendErr := store.AppendHistoryReceipt(t.Context(), canonical, providers.Message{Role: "user", Content: "owned history"}); appendErr != nil {
					t.Fatal(appendErr)
				}
				if metaErr := store.UpsertSessionMeta(t.Context(), canonical, nil, []string{alias}); metaErr != nil {
					t.Fatal(metaErr)
				}
				before, err := os.ReadFile(store.jsonlPath(canonical))
				if err != nil {
					t.Fatal(err)
				}
				original, err := store.GetSessionMeta(t.Context(), canonical)
				if err != nil {
					t.Fatal(err)
				}
				barrier := ""
				for index := 0; ; index++ {
					barrier = fmt.Sprintf("zzz_receipt_barrier_%d", index)
					if store.sessionLock(barrier) != store.sessionLock(canonical) {
						break
					}
				}
				if metaErr := store.UpsertSessionMeta(t.Context(), barrier, nil, nil); metaErr != nil {
					t.Fatal(metaErr)
				}
				barrierLock := store.sessionLock(barrier)
				barrierLock.Lock()
				var release sync.Once
				unlock := func() { release.Do(barrierLock.Unlock) }
				t.Cleanup(unlock)
				store.metaMu.Lock()
				delete(store.metaCache, sanitizeKey(canonical))
				store.metaMu.Unlock()
				type result struct {
					receipt CommittedHistoryMessage
					history []CommittedHistoryMessage
					err     error
				}
				done := make(chan result, 1)
				go func() {
					if operation == "append" {
						receipt, appendErr := store.AppendHistoryReceipt(t.Context(), alias, providers.Message{Role: "assistant", Content: "must not persist"})
						done <- result{receipt: receipt, err: appendErr}
					} else {
						history, readErr := store.ReadHistoryWithReceipts(t.Context(), alias)
						done <- result{history: history, err: readErr}
					}
				}()
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, loaded := store.cachedMeta(sanitizeKey(canonical)); loaded {
						break
					}
					if time.Now().After(deadline) {
						unlock()
						t.Fatal("resolver did not read the original owner")
					}
					time.Sleep(time.Millisecond)
				}
				// The resolver has read its owner, but cannot finish its directory scan
				// while the unrelated last key is locked. Revoke under the real key lock.
				ownerLock := store.sessionLock(canonical)
				ownerLock.Lock()
				changed := original
				if change == "revoke alias" {
					changed.Aliases = nil
				} else {
					changed.Key = "different_owner"
				}
				changeErr := store.writeMeta(canonical, changed)
				ownerLock.Unlock()
				if changeErr != nil {
					unlock()
					t.Fatal(changeErr)
				}
				unlock()
				var actual result
				select {
				case actual = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("owner check did not finish")
				}
				if !errors.Is(actual.err, ErrHistoryOrderUnavailable) || errors.Is(actual.err, ErrHistoryOwnershipUnavailable) || actual.receipt.Order.Valid() || len(actual.history) != 0 {
					t.Fatal("stale owner resolution authorized receipt or fallback", actual)
				}
				after, readErr := os.ReadFile(store.jsonlPath(canonical))
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("revoked ownership changed transcript bytes", readErr)
				}
				finalMeta, metaErr := store.GetSessionMeta(t.Context(), canonical)
				if metaErr != nil || finalMeta.Count != original.Count || finalMeta.HistorySequence != original.HistorySequence {
					t.Fatal("revoked ownership advanced the producer checkpoint", finalMeta, metaErr)
				}
			})
		}
	}
}

func TestHistoryReceiptRejectsDuplicateSequenceOnDistinctOccurrences(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_duplicate_order"
	first, err := store.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "second"})
	if err != nil || first.Message.HistoryID == second.Message.HistoryID {
		t.Fatal("fixture needs distinct real occurrences", first, second, err)
	}
	raw, err := os.ReadFile(store.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	rows := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	var record map[string]json.RawMessage
	if decodeErr := json.Unmarshal(rows[1], &record); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	record["history_order"], err = json.Marshal(first.Order)
	if err != nil {
		t.Fatal(err)
	}
	rows[1], err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(bytes.Join(rows, []byte("\n")), '\n')
	if writeErr := os.WriteFile(store.jsonlPath(key), changed, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(changed))
	meta.HistoryBytes = &size
	if metaErr := store.writeMeta(key, meta); metaErr != nil {
		t.Fatal(metaErr)
	}
	if _, readErr := store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(readErr, ErrHistoryOrderUnavailable) {
		t.Fatal("distinct occurrence IDs hid duplicated sequence", readErr)
	}
	after, err := os.ReadFile(store.jsonlPath(key))
	if err != nil || !bytes.Equal(after, changed) {
		t.Fatal("failed proof read mutated transcript", err)
	}
}

func TestHistoryReceiptRejectsDurableNewDecisionWithChangedCheckpoint(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_new_mismatch"
	if _, appendErr := store.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "first"}); appendErr != nil {
		t.Fatal(appendErr)
	}
	var expected []byte
	fault := errors.New("metadata changed after durable NEW decision")
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
			return writeErr
		}
		if path != store.metaPath(key) {
			return nil
		}
		expected = append([]byte(nil), data...)
		journal, readErr := os.ReadFile(store.historyRecoveryPath(key))
		if readErr != nil {
			return readErr
		}
		if decisionErr := store.recordHistoryDecision(key, journal, historyWriteNew); decisionErr != nil {
			return decisionErr
		}
		var changed SessionMeta
		if decodeErr := json.Unmarshal(data, &changed); decodeErr != nil {
			return decodeErr
		}
		changed.HistorySequence--
		broken, encodeErr := json.Marshal(changed)
		if encodeErr != nil {
			return encodeErr
		}
		return errors.Join(fault, fileutil.WriteFileAtomic(path, broken, mode))
	}
	receipt, err := store.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "assistant", Content: "uncertain"})
	if err == nil || receipt.Order.Valid() {
		t.Fatal("NEW enum hid a mismatched checkpoint", receipt, err)
	}
	if _, statErr := os.Stat(store.historyRecoveryPath(key)); statErr != nil {
		t.Fatal("mismatched NEW lost its recovery fence", statErr)
	}
	store.writeFile = nil
	if writeErr := fileutil.WriteFileAtomic(store.metaPath(key), expected, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	history, err := reopened.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(history) != 2 || history[1].Order.Sequence != 2 || history[1].Message.Content != "uncertain" {
		t.Fatal("repair lost an actually stored occurrence", history, err)
	}
}
