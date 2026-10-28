package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryOwnershipIgnoresUnrelatedWitnessWithoutChangingItsState(t *testing.T) {
	for _, cache := range []string{"cold", "warm"} {
		t.Run(cache, func(t *testing.T) {
			store, other, expected := witnessedHistory(t)
			if err := store.TruncateHistory(t.Context(), other, 2); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSummary(t.Context(), other, "unrelated summary"); err != nil {
				t.Fatal(err)
			}
			before, err := store.GetSessionMeta(t.Context(), other)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			files := contentSourceFiles(t, store, other)
			_, restore := unreadableHistoryWitness(t, store.historyOrderWitnessPath(other), "directory read")
			if cache == "cold" {
				store, err = NewJSONLStore(store.dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			key := "agent:main:healthy-owner"
			first, appendErr := store.AppendHistoryReceipt(
				t.Context(),
				key,
				providers.Message{Role: "user", Content: "healthy"},
			)
			if appendErr != nil || !first.Order.Valid() || first.Order.Sequence != 1 {
				t.Fatal("a non-owner witness prevented healthy proof", first, appendErr)
			}
			if cache == "cold" {
				if _, cached := store.cachedMeta(sanitizeKey(other)); cached {
					t.Fatal("ownership projection populated the full metadata cache")
				}
			}
			restore()
			if got := contentSourceFiles(t, store, other); !reflect.DeepEqual(got, files) {
				t.Fatal("identity selection changed non-owner content, proof or recovery fence")
			}
			after, metaErr := store.GetSessionMeta(t.Context(), other)
			afterJSON, encodeErr := json.Marshal(after)
			if metaErr != nil || encodeErr != nil || string(afterJSON) != string(beforeJSON) || after.Count != 3 ||
				after.Skip != 1 ||
				after.Summary != "unrelated summary" {
				t.Fatal("partial identity replaced non-owner metadata/checkpoint", before, after, metaErr)
			}
			next, nextErr := store.AppendHistoryReceipt(
				t.Context(),
				other,
				providers.Message{Role: "assistant", Content: "same stream"},
			)
			if nextErr != nil || next.Order.StreamID != expected[0].Order.StreamID || next.Order.Sequence != 4 {
				t.Fatal("identity scan damaged the non-owner stream/highwater", next, nextErr)
			}
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			receipts, readErr := store.ReadHistoryWithReceipts(t.Context(), other)
			if readErr != nil || len(receipts) != 3 || !reflect.DeepEqual(receipts[:2], expected[1:]) ||
				receipts[2].Order != next.Order {
				t.Fatal("non-owner proofs/checkpoint changed after reopening", receipts, readErr)
			}
		})
	}
}

func TestHistoryOwnershipUnknownIdentityNeverGrantsProof(t *testing.T) {
	for _, invalid := range []string{"unreadable", "invalid JSON", "invalid key", "invalid aliases", "pending journal"} {
		t.Run(invalid, func(t *testing.T) {
			if invalid == "unreadable" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("requires POSIX non-root permissions")
			}
			store, other, _ := witnessedHistory(t)
			requested := "agent:main:hidden-alias"
			if err := store.UpsertSessionMeta(t.Context(), other, nil, []string{requested}); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(store.metaPath(other))
			if err != nil {
				t.Fatal(err)
			}
			files := contentSourceFiles(t, store, other)
			switch invalid {
			case "unreadable":
				if err = os.Chmod(store.metaPath(other), 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(store.metaPath(other), 0o600) })
			case "invalid JSON", "invalid key", "invalid aliases":
				damaged := []byte("{")
				if invalid == "invalid key" {
					damaged = []byte(`{"key":42,"aliases":["` + requested + `"]}`)
				} else if invalid == "invalid aliases" {
					damaged = []byte(`{"key":"` + other + `","aliases":[42]}`)
				}
				if err = os.WriteFile(store.metaPath(other), damaged, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if invalid == "pending journal" {
				// A NEW/OLD decision may restore aliases not in today's header.
				// This non-owner identity alone cannot settle that decision.
				if err = os.WriteFile(store.metaPath(other), []byte(`{"key":"`+other+`"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				store.markHistoryPending(other)
			}
			for _, operation := range []string{"append", "read", "content"} {
				var operationErr error
				if operation == "append" {
					receipt, appendErr := store.AppendHistoryReceipt(
						t.Context(),
						requested,
						providers.Message{Role: "user", Content: "not proven"},
					)
					operationErr = appendErr
					if receipt.Order.Valid() || receipt.Message.Role != "" {
						t.Fatal("uncertain identity returned a receipt", receipt)
					}
				} else if operation == "read" {
					_, operationErr = store.ReadHistoryWithReceipts(t.Context(), requested)
				} else {
					_, operationErr = store.ReadHistoryContent(t.Context(), requested)
				}
				if !errors.Is(operationErr, ErrHistoryOwnershipUnavailable) ||
					(invalid == "unreadable" && !errors.Is(operationErr, os.ErrPermission)) {
					t.Fatal("unknown ownership granted proof or hid its cause", operation, operationErr)
				}
			}
			if _, statErr := os.Stat(store.jsonlPath(requested)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("proof selection wrote the alias as a different source", statErr)
			}
			if invalid != "unreadable" {
				if err = os.WriteFile(store.metaPath(other), original, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if invalid == "unreadable" {
				if err = os.Chmod(store.metaPath(other), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := contentSourceFiles(t, store, other); !reflect.DeepEqual(got, files) {
				t.Fatal("identity scan mutated the unknown source")
			}
		})
	}
}

func TestHistoryOwnershipSelectedMetadataReadErrorStaysExplicit(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX non-root permissions")
	}
	for _, requested := range []string{"sk_v1_owner_io", "agent:main:owner-io", "agent:main:owner-alias"} {
		t.Run(requested, func(t *testing.T) {
			store := newTestStore(t)
			canonical := requested
			if requested == "agent:main:owner-alias" {
				canonical = "sk_v1_alias_owner_io"
			}
			first, err := store.AppendHistoryReceipt(
				t.Context(),
				canonical,
				providers.Message{Role: "user", Content: "retained"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.UpsertSessionMeta(t.Context(), canonical, nil, []string{requested}); err != nil {
				t.Fatal(err)
			}
			before := contentSourceFiles(t, store, canonical)
			path := store.metaPath(canonical)
			if err = os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			_, operationErr := store.AppendHistoryReceipt(
				t.Context(),
				requested,
				providers.Message{Role: "assistant", Content: "retry once"},
			)
			// A truly unreadable alias header cannot identify its owner. Direct
			// source metadata I/O must remain retryable without a content retry.
			if requested == canonical {
				if !errors.Is(operationErr, os.ErrPermission) || errors.Is(operationErr, ErrHistoryOrderUnavailable) {
					t.Fatal("selected metadata I/O became a legacy fallback", operationErr)
				}
			} else if !errors.Is(operationErr, ErrHistoryOwnershipUnavailable) || !errors.Is(operationErr, os.ErrPermission) {
				t.Fatal("unreadable alias identity did not fail proof selection", operationErr)
			}
			if err = os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(contentSourceFiles(t, store, canonical), before) {
				t.Fatal("failed metadata read mutated proof/content state")
			}
			next, retryErr := store.AppendHistoryReceipt(
				t.Context(),
				requested,
				providers.Message{Role: "assistant", Content: "retry once"},
			)
			if retryErr != nil || next.Order.StreamID != first.Order.StreamID || next.Order.Sequence != 2 {
				t.Fatal("healthy retry reconstructed or rotated proof", next, retryErr)
			}
		})
	}
}

func TestHistoryOwnershipCancellationIsNotAFallback(t *testing.T) {
	store := newTestStore(t)
	if err := os.WriteFile(store.metaPath("000_unreadable_identity"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, appendErr := store.AppendHistoryReceipt(
		ctx,
		"agent:main:cancel",
		providers.Message{Role: "user", Content: "canceled"},
	)
	_, readErr := store.ReadHistoryWithReceipts(ctx, "agent:main:cancel")
	_, contentErr := store.ReadHistoryContent(ctx, "agent:main:cancel")
	for _, err := range []error{appendErr, readErr, contentErr} {
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrHistoryOwnershipUnavailable) {
			t.Fatal("cancellation became fallback-eligible ownership failure", err)
		}
	}
}
