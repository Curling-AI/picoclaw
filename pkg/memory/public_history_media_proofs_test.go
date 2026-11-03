package memory

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicMediaPreparationKeepsIndependentProofLossUnavailable(t *testing.T) {
	for _, proof := range []string{"order", "ref"} {
		t.Run(proof, func(t *testing.T) {
			store, key, run := newTestStore(t), "agent:web:deferred-proof", uuid.NewString()
			ctx := providers.WithHistoryRun(t.Context(), run)
			if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
				t.Fatal(err)
			}
			initializedContent(t, store, key)
			projector := &transientPublicProjector{failing: true}
			store.SetPublicHistoryProjector(projector)
			if err := store.AddMessage(ctx, key, "assistant", "projection gap"); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordPublicMedia(ctx, key, run, "[one](artifact:one.pdf)"); err != nil {
				t.Fatal(err)
			}
			if proof == "order" {
				if err := os.Remove(store.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := os.ReadFile(store.contentDomainPath(key))
				if err != nil {
					t.Fatal(err)
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
			if err := store.RecordPublicMedia(ctx, key, run, "[two](artifact:two.pdf)"); err != nil {
				t.Fatal("proof loss blocked completed display", err)
			}
			projector.failing = false
			result := finishDeferredMedia(t, store, key)
			if len(result.Entries) != 4 {
				t.Fatal("proof loss lost media", result)
			}
			for _, entry := range result.Entries {
				if proof == "order" && (entry.Order.Valid() || entry.AnchorOrder.Valid() || entry.SourceDigest != "") {
					t.Fatal("media preparation minted an unavailable order receipt", entry)
				}
				if proof == "ref" && (entry.Ref != nil || entry.AnchorRef != nil) {
					t.Fatal("media preparation reactivated content identity", entry)
				}
			}
			if proof == "order" {
				if _, err := os.Stat(store.historyOrderWitnessPath(key)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("media recreated the missing order witness", err)
				}
			}
		})
	}
}

func TestPublicMediaPreparingClearAndDeleteCannotRestoreOldDelivery(t *testing.T) {
	for _, operation := range []string{"clear", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store, key, run, ctx := deferredMediaFixture(t)
			var err error
			if operation == "clear" {
				err = store.ClearHistory(ctx, key)
			} else {
				err = store.DeleteSession(ctx, key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = store.RecordPublicMedia(ctx, key, run, "[report](artifact:immutable.pdf)"); err == nil {
				t.Fatal("retired source accepted a late delivery")
			}
			if err = store.AddMessage(ctx, key, "user", "new conversation"); err != nil {
				t.Fatal(err)
			}
			history, err := store.ReadPublicHistory(ctx, key)
			if err != nil || len(history.Entries) != 1 || history.Entries[0].Message.Content != "new conversation" {
				t.Fatal("old deferred media was restored after retiring the source", history, err)
			}
		})
	}
}

func TestPublicMediaPreparingCannotBorrowALocatorFromSameSizeRawTampering(t *testing.T) {
	store, key, run := newTestStore(t), "agent:web:tampered-media", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if err := store.AddMessage(ctx, key, "user", "request"); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, store, key)
	store.SetPublicHistoryProjector(&transientPublicProjector{failing: true})
	if err := store.AddMessage(ctx, key, "assistant", "unprojected row"); err != nil {
		t.Fatal(err)
	}
	model, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	replacement := strings.Replace(string(raw), model[len(model)-1].HistoryID, uuid.NewString(), 1)
	if replacement == string(raw) || len(replacement) != len(raw) {
		t.Fatal("fixture did not replace exact native ID at the same size")
	}
	if err = os.WriteFile(store.jsonlPath(key), []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordPublicMedia(
		ctx,
		key,
		run,
		"[new](artifact:new.pdf)",
	); !errors.Is(
		err,
		ErrPublicHistoryCoverage,
	) {
		t.Fatal("same-size RAW edit borrowed old locator authority", err)
	}
}
