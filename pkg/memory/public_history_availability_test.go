package memory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPublicDisplayKeepsIndependentProofFailuresUnavailable(t *testing.T) {
	for _, failure := range []string{"order-missing", "order-corrupt", "domain-missing", "domain-ref", "domain-order", "marker", "recovery"} {
		t.Run(failure, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_display_availability"
			if err := store.AddMessage(t.Context(), key, "assistant", "confirmed body"); err != nil {
				t.Fatal(err)
			}
			initializedContent(t, store, key)
			before, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil || len(before.Entries) != 1 || before.Entries[0].Ref == nil ||
				!before.Entries[0].Order.Valid() {
				t.Fatal("fixture lacks independent proofs", before, err)
			}
			refValid, orderValid := true, true
			switch failure {
			case "order-missing":
				err = os.Remove(store.historyOrderWitnessPath(key))
				orderValid = false
			case "order-corrupt":
				err = os.WriteFile(store.historyOrderWitnessPath(key), []byte("invalid"), 0o600)
				orderValid = false
			case "domain-missing":
				err = os.Remove(store.contentDomainPath(key))
				refValid, orderValid = false, false
			case "domain-ref", "domain-order":
				var raw []byte
				raw, err = os.ReadFile(store.contentDomainPath(key))
				if err != nil {
					t.Fatal(err)
				}
				var witness contentDomainWitness
				if err = json.Unmarshal(raw, &witness); err != nil {
					t.Fatal(err)
				}
				if failure == "domain-ref" {
					witness.Unavailable = true
					refValid = false
				} else {
					witness.OrderCheckpoint = "invalid"
					orderValid = false
				}
				raw, err = json.Marshal(witness)
				if err == nil {
					err = os.WriteFile(store.contentDomainPath(key), raw, 0o600)
				}
			case "marker":
				err = os.WriteFile(store.jsonlPath(key)+".recovery-required", []byte("required"), 0o600)
				orderValid = false
			case "recovery":
				var meta SessionMeta
				meta, err = store.readMeta(key)
				if err == nil {
					meta.HistoryRecoveryRequired = true
					err = store.writeMeta(key, meta)
				}
				orderValid = false
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, readOnly := range []bool{false, true} {
				var history PublicHistory
				if readOnly {
					history, err = ReadPublicHistoryFS(t.Context(), os.DirFS(store.dir), key)
				} else {
					history, err = store.ReadPublicHistory(t.Context(), key)
				}
				if err != nil || len(history.Entries) != 1 || history.Entries[0].Message.Content != "confirmed body" {
					t.Fatal("independent proof suppressed confirmed display", readOnly, history, err)
				}
				entry := history.Entries[0]
				if (entry.Ref != nil) != refValid || entry.Order.Valid() != orderValid {
					t.Fatal(
						"independent authority was restored or coupled",
						failure,
						readOnly,
						entry,
						refValid,
						orderValid,
					)
				}
			}
		})
	}
}
