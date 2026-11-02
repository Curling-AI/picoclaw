package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func requireContentRecoveryFence(t *testing.T, store *JSONLStore, key string) {
	t.Helper()
	if _, err := store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
		t.Fatal("content mutation restored receipt eligibility", err)
	}
	if _, err := store.ReadHistoryPage(
		t.Context(),
		HistoryPageRequest{Key: key},
	); !errors.Is(
		err,
		ErrHistoryRecoveryRequired,
	) {
		t.Fatal("content mutation restored page eligibility", err)
	}
	before, err := os.ReadFile(store.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	receipt, appendErr := store.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "unconfirmed"},
	)
	if !errors.Is(appendErr, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
		t.Fatal("content mutation minted a receipt", receipt, appendErr)
	}
	after, readErr := os.ReadFile(store.jsonlPath(key))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("receipt rejection changed content", readErr)
	}
}

func TestContentRecoveryReplacementRetainsOnlyStoredMatchingProof(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_content_replacement_evidence"
	for _, content := range []string{"keep", "change"} {
		if _, err := store.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: content},
		); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if markerErr := store.markHistoryRecoveryRequired(key, &meta); markerErr != nil {
		t.Fatal(markerErr)
	}
	old, err := store.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	// Persist both the existing proof and the recovery flag on the same row.
	data, err := marshalHistoryJSONL(old, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.jsonlPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	keptOrder := messageHistoryOrder(old[0])
	forged := HistoryOrderProof{Version: HistoryOrderVersion, StreamID: newHistoryID(), Sequence: 333}
	prepared := []providers.Message{
		messageWithHistoryOrder(old[0], forged),
		messageWithHistoryOrder(old[1], forged),
		messageWithHistoryOrder(
			providers.Message{Role: "assistant", Content: "new", HistoryID: newHistoryID()},
			forged,
		),
	}
	prepared[1].Content = "edited"
	if err := store.prepareReplacementHistoryOrder(key, prepared, &meta); err != nil {
		t.Fatal(err)
	}
	if messageHistoryOrder(prepared[0]) != keptOrder || !keptOrder.Valid() {
		t.Fatal("unchanged occurrence lost its stored private evidence", prepared[0])
	}
	if messageHistoryOrder(prepared[1]) != (HistoryOrderProof{}) ||
		messageHistoryOrder(prepared[2]) != (HistoryOrderProof{}) {
		t.Fatal("changed or new content borrowed an order proof", prepared)
	}
	if meta.HistorySequence != 2 || !meta.HistoryRecoveryRequired {
		t.Fatal("replacement allocated or recovered an order stream", meta)
	}
}

func TestContentRecoveryTruncateCompactAndReplaceKeepReceiptsUnavailable(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_content_recovery_lifecycle"
	prepareUnprovenRuntimeFixture(t, store, key, "legacy count")
	if err := store.AddMessage(t.Context(), key, "assistant", "new content"); err != nil {
		t.Fatal(err)
	}
	if err := store.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal("content truncation froze a recovery-required session", err)
	}
	if err := store.Compact(t.Context(), key); err != nil {
		t.Fatal("compaction froze a recovery-required session", err)
	}
	history, err := store.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || history[0].Content != "durable fsynced tail" ||
		history[1].Content != "new content" {
		t.Fatal("content lifecycle changed the retained messages", history, err)
	}
	history[1].Content = "edited content"
	if rewriteErr := store.SetHistory(t.Context(), key, history); rewriteErr != nil {
		t.Fatal("content replacement froze a recovery-required session", rewriteErr)
	}
	store, err = NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	history, err = store.GetHistory(t.Context(), key)
	if err != nil || len(history) != 2 || history[1].Content != "edited content" {
		t.Fatal("reopen lost the requested content edit", history, err)
	}
	requireContentRecoveryFence(t, store, key)
}

func TestContentRecoveryReplacementKeepsStoredProofAcrossRewrites(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_content_recovery_rewrite_evidence"
	for _, content := range []string{"keep", "edit"} {
		if _, err := store.AppendHistoryReceipt(
			t.Context(),
			key,
			providers.Message{Role: "assistant", Content: content},
		); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if markerErr := store.markHistoryRecoveryRequired(key, &meta); markerErr != nil {
		t.Fatal(markerErr)
	}
	history, err := store.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	kept := messageHistoryOrder(history[0])
	for _, content := range []string{"edit one", "edit two", "edit three"} {
		history[1].Content = content
		if rewriteErr := store.SetHistory(t.Context(), key, history); rewriteErr != nil {
			t.Fatal(rewriteErr)
		}
		store, err = NewJSONLStore(store.dir)
		if err != nil {
			t.Fatal(err)
		}
		history, err = store.GetHistory(t.Context(), key)
		if err != nil || len(history) != 2 || history[1].Content != content ||
			messageHistoryOrder(history[0]) != kept || messageHistoryOrder(history[1]) != (HistoryOrderProof{}) {
			t.Fatal("content rewrite lost old evidence or assigned new proof", history, err)
		}
		meta, err = store.GetSessionMeta(t.Context(), key)
		if err != nil || meta.HistorySequence != 2 || !meta.HistoryRecoveryRequired {
			t.Fatal("content rewrite changed the order counter or fence", meta, err)
		}
		requireContentRecoveryFence(t, store, key)
	}
}

func TestContentMutationCannotHideLostStreamBeforeLatchingRecovery(t *testing.T) {
	for _, operation := range []string{"clear", "truncate"} {
		t.Run(operation, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_content_recovery_first_mutation"
			prepareUnprovenRuntimeFixture(t, store, key, "lost stream")
			var err error
			if operation == "clear" {
				err = store.SetHistory(t.Context(), key, nil)
			} else {
				err = store.TruncateHistory(t.Context(), key, 0)
			}
			if err != nil {
				t.Fatal("requested content operation failed", err)
			}
			requireContentRecoveryFence(t, store, key)
			if err := store.Compact(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			if err := store.AddMessage(t.Context(), key, "user", "after clear"); err != nil {
				t.Fatal(err)
			}
			history, readErr := store.GetHistory(t.Context(), key)
			if readErr != nil || len(history) != 1 || history[0].Content != "after clear" ||
				messageHistoryOrder(history[0]) != (HistoryOrderProof{}) {
				t.Fatal("clear created new order claims or lost content", history, readErr)
			}
			requireContentRecoveryFence(t, store, key)
		})
	}
}

func TestContentRecoveryAliasTransfersContentWithoutForeignReceipts(t *testing.T) {
	store := newTestStore(t)
	alias, canonical := "agent:main:content-recovery", "sk_v1_content_recovery_alias"
	prepareUnprovenRuntimeFixture(t, store, alias, "lost stream")
	if err := store.AddMessage(t.Context(), alias, "assistant", "unproven delivery"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.jsonlPath(alias))
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.PromoteAliasHistory(t.Context(), canonical, nil, []string{alias})
	if err != nil || !promoted {
		t.Fatal("content promotion froze an unproven session", promoted, err)
	}
	history, err := store.GetHistory(t.Context(), canonical)
	if err != nil || len(history) != 3 || history[2].Content != "unproven delivery" {
		t.Fatal("promotion lost content", history, err)
	}
	for _, message := range history {
		if messageHistoryOrder(message) != (HistoryOrderProof{}) {
			t.Fatal("canonical destination borrowed a foreign proof", message)
		}
	}
	after, err := os.ReadFile(store.jsonlPath(alias))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("promotion changed the original recovery source", err)
	}
	requireContentRecoveryFence(t, store, canonical)
	// A canonical lookup owns the promoted alias; inspect the original source
	// through a fresh reader that still must honor its persistent fence.
	meta, err := store.GetSessionMeta(t.Context(), alias)
	if err != nil || !meta.HistoryRecoveryRequired {
		t.Fatal("promotion cleared the source recovery requirement", meta, err)
	}
}

func TestHistoryReceiptBoundariesObserveMarkerAfterWarmCache(t *testing.T) {
	for _, operation := range []string{"read", "append"} {
		t.Run(operation, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_receipt_warm_cache_marker"
			if _, err := store.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "proven"},
			); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadHistoryWithReceipts(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(store.historyRecoveryRequiredPath(key), nil, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if operation == "read" {
				if _, readErr := store.ReadHistoryWithReceipts(
					t.Context(),
					key,
				); !errors.Is(
					readErr,
					ErrHistoryReceiptUnavailable,
				) {
					t.Fatal("warm cache hid a new marker from receipt reader", readErr)
				}
			} else {
				receipt, appendErr := store.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: "must not append"},
				)
				if !errors.Is(appendErr, ErrHistoryReceiptUnavailable) || receipt.Order.Valid() {
					t.Fatal("warm cache hid a new marker from receipt append", receipt, appendErr)
				}
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("marker observation changed content", err)
			}
		})
	}
}
