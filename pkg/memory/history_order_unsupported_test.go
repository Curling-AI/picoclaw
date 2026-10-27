package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryOrderUnsupportedPreWitnessFormatNeverMigrates(t *testing.T) {
	for _, hasWitness := range []bool{false, true} {
		t.Run(map[bool]string{false: "without witness", true: "with witness"}[hasWitness], func(t *testing.T) {
			store, key, _ := witnessedHistory(t)
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.HistoryOrderWitnessVersion = 0
			metadata, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(store.metaPath(key), metadata, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if !hasWitness {
				if removeErr := os.Remove(store.historyOrderWitnessPath(key)); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			transcript, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			receipts, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key)
			if !errors.Is(receiptErr, ErrHistoryReceiptUnavailable) || len(receipts) != 0 {
				t.Fatal("experimental format was silently activated", receipts, receiptErr)
			}
			for path, expected := range map[string][]byte{store.metaPath(key): metadata, store.jsonlPath(key): transcript} {
				actual, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(actual, expected) {
					t.Fatal("receipt read migrated the experimental source", path, readErr)
				}
			}
			if _, statErr := os.Stat(store.historyRecoveryPath(key)); !os.IsNotExist(statErr) {
				t.Fatal("receipt read created a migration journal", statErr)
			}
			if !hasWitness {
				if _, statErr := os.Stat(store.historyOrderWitnessPath(key)); !os.IsNotExist(statErr) {
					t.Fatal("receipt read created a witness for the experimental source", statErr)
				}
			}
			requireHistoryContents(t, store, key, "first", "second", "third")
			if appendErr := store.AddFullMessage(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "content only"},
			); appendErr != nil {
				t.Fatal("unsupported ordering blocked normal content", appendErr)
			}
			requireHistoryContents(t, store, key, "first", "second", "third", "content only")
			requireContentRecoveryFence(t, store, key)
			stored, err := store.readRecoveryMeta(key)
			if err != nil || stored.HistoryOrderWitnessVersion != 0 || stored.HistoryStreamID != meta.HistoryStreamID ||
				stored.HistorySequence != meta.HistorySequence {
				t.Fatal("content append migrated or renumbered the experimental source", stored, err)
			}
		})
	}
}
