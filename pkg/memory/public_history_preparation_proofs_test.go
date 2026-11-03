package memory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPublicPreparationPreservesDisplayWithoutReattestingIndependentProofs(t *testing.T) {
	for _, proof := range []string{"valid", "order-missing", "ref-unavailable"} {
		t.Run(proof, func(t *testing.T) {
			store, key := newTestStore(t), "sk_v1_prepare_independent_proofs"
			if err := store.AddMessage(t.Context(), key, "assistant", "confirmed legacy display"); err != nil {
				t.Fatal(err)
			}
			initializedContent(t, store, key)
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.PublicHistoryVersion, meta.PublicHistoryRevision, meta.PublicHistoryBytes, meta.PublicHistoryCount = 0, 0, 0, 0
			meta.PublicHistoryGeneration, meta.PublicHistoryDigest, meta.PublicHistoryComplete = "", "", false
			if err = store.writeMeta(key, meta); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(store.publicHistoryPath(key)); err != nil {
				t.Fatal(err)
			}
			if proof == "order-missing" {
				if err = os.Remove(store.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
			}
			if proof == "ref-unavailable" {
				data, readErr := os.ReadFile(store.contentDomainPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var witness contentDomainWitness
				if err = json.Unmarshal(data, &witness); err != nil {
					t.Fatal(err)
				}
				witness.Unavailable = true
				data, err = json.Marshal(witness)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(store.contentDomainPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			preparation, err := store.PreparePublicHistory(t.Context(), key, "")
			if err != nil {
				t.Fatal("independent proof loss suppressed canonical display preparation", err)
			}
			for step := 0; !preparation.Ready && step < 3; step++ {
				preparation, err = store.PreparePublicHistory(t.Context(), key, preparation.Cursor)
				if err != nil {
					t.Fatal(err)
				}
			}
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil || !preparation.Ready || len(history.Entries) != 1 ||
				history.Entries[0].Message.Content != "confirmed legacy display" {
				t.Fatal("preparation lost confirmed display", history, err)
			}
			entry := history.Entries[0]
			if (entry.Ref != nil) != (proof != "ref-unavailable") || entry.Order.Valid() != (proof != "order-missing") {
				t.Fatal("preparation coupled or reattested independent proof", entry)
			}
			preparedMeta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := store.readPublicEntries(key, preparedMeta)
			if err != nil || len(persisted) != 1 || (persisted[0].Ref != nil) != (proof != "ref-unavailable") ||
				persisted[0].Order.Valid() != (proof != "order-missing") {
				t.Fatal("preparation persisted proof without its independent authority", persisted, err)
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || string(before) != string(after) {
				t.Fatal("display preparation rewrote its canonical source", err)
			}
			if proof == "order-missing" {
				if persisted[0].SourceDigest != "" {
					t.Fatal("preparation minted a raw receipt binding without order proof")
				}
				if persisted[0].LegacyBeforeStreamID != "" {
					t.Fatal("preparation minted a legacy order boundary without proof")
				}
				if _, err = os.Stat(store.historyOrderWitnessPath(key)); !os.IsNotExist(err) {
					t.Fatal("display preparation recreated an absent order witness", err)
				}
				if _, err = store.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); err == nil {
					t.Fatal("display preparation reopened order receipts without proof")
				}
			} else if _, err = store.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); err != nil {
				t.Fatal("display preparation blocked independently valid order receipts", err)
			}
		})
	}
}

func TestPublicPreparationIncludesCommittedAppendWithinTheSameBoundedStep(t *testing.T) {
	store, key := legacyPublicFixture(t, 3)
	preparation, err := store.PreparePublicHistory(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"new prompt", "new answer"} {
		if err = store.AddMessage(t.Context(), key, "assistant", text); err != nil {
			t.Fatal(err)
		}
	}
	preparation, err = store.PreparePublicHistory(t.Context(), key, preparation.Cursor)
	if err != nil || !preparation.Ready || preparation.Count != 5 {
		t.Fatal("one bounded step stopped at an obsolete append frontier", preparation, err)
	}
}
