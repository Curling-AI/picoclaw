package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestReceiptRejectionLatchesRecoveryBeforeRuntimeFallback(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_receipt_rejection_latch"
	before := prepareUnprovenRuntimeFixture(t, store, key, "legacy count")
	receipt, err := store.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "must not commit"},
	)
	if !errors.Is(err, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
		t.Fatal("unproven append did not reject its receipt before writing", receipt, err)
	}
	after, err := os.ReadFile(store.jsonlPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected receipt changed durable content", err)
	}
	store, err = NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, readErr := store.ReadHistoryWithReceipts(
		t.Context(),
		key,
	); !errors.Is(
		readErr,
		ErrHistoryReceiptUnavailable,
	) {
		t.Fatal("rejected proof was forgotten before runtime fallback", readErr)
	}
}

func TestHistoryReceiptsStayUnavailableAfterLegacyReplacementOrRecreation(t *testing.T) {
	for _, operation := range []string{"replace", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_persistent_order_recovery"
			prepareUnprovenRuntimeFixture(t, store, key, "legacy count")
			if err := store.AddMessage(t.Context(), key, "assistant", "content without proof"); err != nil {
				t.Fatal(err)
			}
			priorCount := 0
			if operation == "replace" {
				// An older SetHistory erases unknown private metadata and row fields.
				if err := os.WriteFile(
					store.jsonlPath(key),
					[]byte("{\"role\":\"user\",\"content\":\"old rewrite\"}\n"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					store.metaPath(key),
					[]byte("{\"key\":\""+key+"\",\"count\":1}"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
				priorCount = 1
			} else {
				for _, path := range []string{store.jsonlPath(key), store.metaPath(key)} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			var err error
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, readErr := store.ReadHistoryWithReceipts(
				t.Context(),
				key,
			); !errors.Is(
				readErr,
				ErrHistoryReceiptUnavailable,
			) {
				t.Fatal("old writer removal of JSON fields re-enabled receipts", readErr)
			}
			if appendErr := store.AddFullMessage(t.Context(), key, providers.Message{
				Role:                "assistant",
				Content:             "aware runtime",
				HistoryOrderVersion: 1,
				HistoryStreamID:     newHistoryID(),
				HistorySequence:     777,
			}); appendErr != nil {
				t.Fatal(appendErr)
			}
			history, readErr := store.GetHistory(t.Context(), key)
			if readErr != nil || len(history) != priorCount+1 || history[len(history)-1].Content != "aware runtime" {
				t.Fatal("recovery fence froze runtime content", history, readErr)
			}
			if messageHistoryOrder(history[len(history)-1]) != (HistoryOrderProof{}) {
				t.Fatal("recovery-required append retained an input proof", history)
			}
			if _, pageErr := store.ReadHistoryPage(
				t.Context(),
				HistoryPageRequest{Key: key},
			); !errors.Is(
				pageErr,
				ErrHistoryRecoveryRequired,
			) {
				t.Fatal("old writer removal of JSON fields re-enabled cursor pages", pageErr)
			}
			before, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			receipt, appendErr := store.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "must not commit"},
			)
			if !errors.Is(appendErr, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
				t.Fatal("persistent fence minted a new stream", receipt, appendErr)
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected receipt created a duplicate occurrence", err)
			}
		})
	}
}

func TestUnprovenRuntimeAppendPreservesContentUntilExplicitRecovery(t *testing.T) {
	for _, state := range []string{"legacy count", "stale checkpoint", "lost stream", "retired lineage"} {
		t.Run(state, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_unproven_runtime"
			original := prepareUnprovenRuntimeFixture(t, store, key, state)
			before, err := store.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			extra := 0
			for attempt := 0; attempt < 4; attempt++ {
				if attempt == 1 && state == "legacy count" {
					file, openErr := os.OpenFile(store.jsonlPath(key), os.O_APPEND|os.O_WRONLY, 0o600)
					if openErr != nil {
						t.Fatal(openErr)
					}
					if _, writeErr := file.WriteString(
						"{\"role\":\"assistant\",\"content\":\"durable while unavailable\"}\n",
					); writeErr != nil {
						t.Fatal(writeErr)
					}
					if syncErr := file.Sync(); syncErr != nil {
						t.Fatal(syncErr)
					}
					if closeErr := file.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					original, err = os.ReadFile(store.jsonlPath(key))
					if err != nil {
						t.Fatal(err)
					}
					extra++
				}
				if attempt == 3 {
					store, err = NewJSONLStore(store.dir)
					if err != nil {
						t.Fatal(err)
					}
				}
				if appendErr := store.AddFullMessage(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: fmt.Sprintf("runtime_%d", attempt)},
				); appendErr != nil {
					t.Fatal("unprovable prefix froze runtime persistence", attempt, appendErr)
				}
				visible, readErr := store.GetHistory(t.Context(), key)
				if readErr != nil || len(visible) != len(before)+attempt+1+extra ||
					visible[len(visible)-1].Content != fmt.Sprintf("runtime_%d", attempt) {
					t.Fatal("runtime lost durable content", visible, readErr)
				}
				raw, rawErr := os.ReadFile(store.jsonlPath(key))
				if rawErr != nil || !bytes.HasPrefix(raw, original) {
					t.Fatal("append truncated or rewrote the durable prefix", rawErr)
				}
				if _, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
					t.Fatal("unproven runtime data was exposed as committed order")
				}
			}
			// An older metadata writer can discard the unavailable field. The
			// record marker must still prevent implicit re-enabling of proofs.
			stateMeta, stateErr := store.readMeta(key)
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			stateMeta.HistoryRecoveryRequired = false
			oldMeta, encodeErr := json.Marshal(stateMeta)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if writeErr := fileutil.WriteFileAtomic(store.metaPath(key), oldMeta, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
				t.Fatal("lost metadata turned marked records into genuine legacy")
			}
			if appendErr := store.AddFullMessage(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "after older metadata"},
			); appendErr != nil {
				t.Fatal(appendErr)
			}
			raw, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			receipt, receiptErr := store.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "cannot claim proof"},
			)
			if receiptErr == nil || receipt.Order.Valid() {
				t.Fatal("unavailable receipt path claimed order", receipt, receiptErr)
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("failed receipt append wrote a second occurrence", err)
			}
			var meta map[string]json.RawMessage
			data, err := os.ReadFile(store.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if decodeErr := json.Unmarshal(data, &meta); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if string(meta["history_recovery_required"]) != "true" {
				t.Fatal("unavailable state was not latched across content commits", string(data))
			}
			// Recovery is an explicit caller action in a new canonical session, not
			// automatic renumbering of the old stream or inference from retained max.
			visible, err := store.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			recoveredKey := "recovered_" + key
			if recoveryErr := store.SetHistory(t.Context(), recoveredKey, visible); recoveryErr != nil {
				t.Fatal(recoveryErr)
			}
			recovered, err := store.ReadHistoryWithReceipts(t.Context(), recoveredKey)
			if err != nil || len(recovered) != len(visible) {
				t.Fatal("explicit recovery could not import content", recovered, err)
			}
			for index, item := range recovered {
				if !item.Order.Valid() || item.Order.Sequence != uint64(index+1) ||
					item.Message.Content != visible[index].Content {
					t.Fatal("explicit recovery did not create its own exact proofs", item)
				}
				if old := messageHistoryOrder(visible[index]); old.Valid() && old.StreamID == item.Order.StreamID {
					t.Fatal("recovery guessed continuity of a lost stream", item)
				}
			}
			oldRaw, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || !bytes.Equal(oldRaw, raw) {
				t.Fatal("explicit new-session recovery changed quarantined source", err)
			}
			if _, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key); receiptErr == nil {
				t.Fatal("recovery silently cleared the original unavailable state")
			}
		})
	}
}

func prepareUnprovenRuntimeFixture(t *testing.T, store *JSONLStore, key, state string) []byte {
	t.Helper()
	var raw []byte
	meta := SessionMeta{Key: key, Count: 2}
	switch state {
	case "legacy count", "stale checkpoint":
		prefix := []byte("{\"role\":\"user\",\"content\":\"old1\"}\n{\"role\":\"assistant\",\"content\":\"old2\"}\n")
		raw = append(
			append([]byte(nil), prefix...),
			[]byte("{\"role\":\"assistant\",\"content\":\"durable fsynced tail\"}\n")...)
		if state == "stale checkpoint" {
			size := int64(len(prefix))
			meta.HistoryBytes = &size
		}
	case "lost stream":
		for index := 0; index < 5; index++ {
			if _, appendErr := store.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: fmt.Sprint(index)},
			); appendErr != nil {
				t.Fatal(appendErr)
			}
		}
		history, readErr := store.GetHistory(t.Context(), key)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if rewriteErr := store.SetHistory(t.Context(), key, history[:2]); rewriteErr != nil {
			t.Fatal(rewriteErr)
		}
		var metaErr error
		meta, metaErr = store.readMeta(key)
		if metaErr != nil {
			t.Fatal(metaErr)
		}
		if meta.HistorySequence != 5 {
			t.Fatal("fixture lost its discarded highwater", meta)
		}
		meta.HistoryOrderVersion, meta.HistoryStreamID, meta.HistorySequence = 0, "", 0
		meta.HistoryLegacyFileGeneration, meta.HistoryLegacyBytes = "", 0
		var readErr2 error
		raw, readErr2 = os.ReadFile(store.jsonlPath(key))
		if readErr2 != nil {
			t.Fatal(readErr2)
		}
	case "retired lineage":
		prior := SessionMeta{Key: key, HistoryGeneration: newHistoryID(), HistoryFileGeneration: newHistoryID()}
		row, encodeErr := marshalHistoryMessage(
			prepareHistoryMessage(
				providers.Message{Role: "assistant", Content: "bounded old record"},
				time.Unix(1, 0),
				"",
			),
			prior,
		)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		raw = append(append(row, '\n'), []byte("{\"role\":\"user\",\"content\":\"old pin record\"}\n")...)
	default:
		t.Fatal(state)
	}
	if writeErr := os.WriteFile(store.jsonlPath(key), raw, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := store.writeMeta(key, meta); writeErr != nil {
		t.Fatal(writeErr)
	}
	return append([]byte(nil), raw...)
}
