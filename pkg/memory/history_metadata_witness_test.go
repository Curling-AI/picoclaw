package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func TestMetadataUpsertRetainsSurvivingOrderEvidence(t *testing.T) {
	for _, evidence := range []string{"witness", "recovery marker"} {
		for _, cache := range []string{"warm", "cold"} {
			t.Run(evidence+"/"+cache, func(t *testing.T) {
				store, key, _ := witnessedHistory(t)
				witness, readErr := os.ReadFile(store.historyOrderWitnessPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, path := range []string{store.jsonlPath(key), store.metaPath(key)} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if evidence == "recovery marker" {
					if err := os.Remove(store.historyOrderWitnessPath(key)); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(store.historyRecoveryRequiredPath(key), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if cache == "cold" {
					reopened, openErr := NewJSONLStore(store.dir)
					if openErr != nil {
						t.Fatal(openErr)
					}
					store = reopened
				}
				if err := store.UpsertSessionMeta(t.Context(), key, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := store.AddMessage(t.Context(), key, "user", "content stays available"); err != nil {
					t.Fatal(err)
				}
				requireHistoryContents(t, store, key, "content stays available")
				requireContentRecoveryFence(t, store, key)
				if evidence == "witness" {
					after, witnessErr := os.ReadFile(store.historyOrderWitnessPath(key))
					if witnessErr != nil || !bytes.Equal(after, witness) {
						t.Fatal("metadata retirement erased or replaced a surviving witness", witnessErr)
					}
				} else if _, witnessErr := os.Stat(store.historyOrderWitnessPath(key)); !errors.Is(witnessErr, os.ErrNotExist) {
					t.Fatal("metadata retirement created a new stream behind a recovery marker", witnessErr)
				}
				reopened, openErr := NewJSONLStore(store.dir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				requireHistoryContents(t, reopened, key, "content stays available")
				requireContentRecoveryFence(t, reopened, key)
			})
		}
	}
}
