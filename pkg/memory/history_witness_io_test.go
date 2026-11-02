package memory

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func unreadableHistoryWitness(t *testing.T, path, failure string) (error, func()) {
	t.Helper()
	var restored bool
	restore := func() {
		if restored {
			return
		}
		if failure == "permission" {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := os.Rename(path+".io-fixture", path); err != nil {
				t.Fatal(err)
			}
		}
		restored = true
	}
	if failure == "permission" {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires POSIX file permissions for a non-root user")
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restore)
	} else {
		if err := os.Rename(path, path+".io-fixture"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restore)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := os.ReadFile(path)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture did not cause a real, non-absence witness read failure", err)
	}
	return pathErr.Err, restore
}

func historyProofOperation(t *testing.T, store *JSONLStore, key, operation string) error {
	t.Helper()
	switch operation {
	case "append":
		_, err := store.AppendHistoryReceipt(
			t.Context(), key, providers.Message{Role: "assistant", Content: "retry appends once"},
		)
		return err
	case "page":
		_, err := store.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
		return err
	default:
		_, err := store.ReadHistoryWithReceipts(t.Context(), key)
		return err
	}
}

func TestHistoryWitnessReadErrorsStayRetryable(t *testing.T) {
	for _, failure := range []string{"permission", "directory read"} {
		for _, cache := range []string{"warm", "cold", "cold alias"} {
			for _, operation := range []string{"read", "append", "page"} {
				if cache == "cold alias" && operation == "page" {
					continue // Bounded pages take the physical canonical key directly.
				}
				t.Run(failure+"/"+cache+"/"+operation, func(t *testing.T) {
					store, key, expected := witnessedHistory(t)
					alias := "agent:main:transient-witness"
					if aliasErr := store.UpsertSessionMeta(t.Context(), key, nil, []string{alias}); aliasErr != nil {
						t.Fatal(aliasErr)
					}
					before := contentSourceFiles(t, store, key)
					wantErr, restore := unreadableHistoryWitness(t, store.historyOrderWitnessPath(key), failure)
					if cache != "warm" {
						reopened, openErr := NewJSONLStore(store.dir)
						if openErr != nil {
							t.Fatal(openErr)
						}
						store = reopened
					}
					requested := key
					if cache == "cold alias" {
						requested = alias
					}
					operationErr := historyProofOperation(t, store, requested, operation)
					restore()
					if !errors.Is(operationErr, wantErr) || errors.Is(operationErr, ErrHistoryOrderUnavailable) ||
						errors.Is(operationErr, ErrHistoryOwnershipUnavailable) ||
						errors.Is(operationErr, ErrHistoryRecoveryRequired) {
						t.Error("actual witness I/O became a proof-loss or fallback classification", operationErr)
					}
					if got := contentSourceFiles(t, store, key); !reflect.DeepEqual(got, before) {
						t.Error("failed witness read changed durable content/proof state or created a fence")
					}
					if retryErr := historyProofOperation(t, store, requested, operation); retryErr != nil {
						t.Fatal("healthy retry remained fenced after witness access recovered", retryErr)
					}
					wantCount := len(expected)
					if operation == "append" {
						wantCount++
					}
					for _, state := range []string{"retry", "reopen"} {
						if state == "reopen" {
							reopened, openErr := NewJSONLStore(store.dir)
							if openErr != nil {
								t.Fatal(openErr)
							}
							store = reopened
						}
						receipts, readErr := store.ReadHistoryWithReceipts(t.Context(), key)
						if readErr != nil || len(receipts) != wantCount ||
							!reflect.DeepEqual(receipts[:len(expected)], expected) {
							t.Fatal(state+" changed established stream/proofs", receipts, readErr)
						}
						if operation == "append" && (receipts[3].Order.StreamID != expected[0].Order.StreamID ||
							receipts[3].Order.Sequence != 4 || receipts[3].Message.Content != "retry appends once") {
							t.Fatal(state+" duplicated the failed append or rotated/reconstructed the stream", receipts)
						}
						if _, markerErr := os.Stat(
							store.historyRecoveryRequiredPath(key),
						); !errors.Is(
							markerErr,
							os.ErrNotExist,
						) {
							t.Fatal(state+" retained a durable fence after a transient read error", markerErr)
						}
					}
				})
			}
		}
	}
}

func TestConfirmedWitnessLossKeepsItsFenceAfterReadableRetry(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "mismatched", "existing fence"} {
		t.Run(damage, func(t *testing.T) {
			store, key, expected := witnessedHistory(t)
			witnessPath := store.historyOrderWitnessPath(key)
			original, readErr := os.ReadFile(witnessPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch damage {
			case "missing":
				if err := os.Remove(witnessPath); err != nil {
					t.Fatal(err)
				}
			case "corrupt", "mismatched":
				data := []byte("{")
				if damage == "mismatched" {
					var witness historyOrderWitness
					if err := json.Unmarshal(original, &witness); err != nil {
						t.Fatal(err)
					}
					witness.StreamID = newHistoryID()
					encoded, encodeErr := json.Marshal(witness)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					data = encoded
				}
				if err := os.WriteFile(witnessPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "existing fence":
				meta, metaErr := store.readMeta(key)
				if metaErr != nil {
					t.Fatal(metaErr)
				}
				if err := store.markHistoryRecoveryRequired(key, &meta); err != nil {
					t.Fatal(err)
				}
			}
			for _, operation := range []string{"read", "append", "page"} {
				if err := historyProofOperation(t, store, key, operation); !errors.Is(err, ErrHistoryRecoveryRequired) {
					t.Fatal("confirmed proof loss remained receipt-eligible", damage, operation, err)
				}
			}
			marker, markerErr := os.ReadFile(store.historyRecoveryRequiredPath(key))
			if markerErr != nil {
				t.Fatal("confirmed loss did not retain its durable fence", markerErr)
			}
			if err := os.WriteFile(witnessPath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			requireContentRecoveryFence(t, store, key)
			reopened, openErr := NewJSONLStore(store.dir)
			if openErr != nil {
				t.Fatal(openErr)
			}
			requireContentRecoveryFence(t, reopened, key)
			if err := reopened.AddMessage(t.Context(), key, "assistant", "content remains writable"); err != nil {
				t.Fatal(err)
			}
			content, contentErr := checkedContentReader(t, reopened).ReadHistoryContent(t.Context(), key)
			meta, metaErr := reopened.readMeta(key)
			if contentErr != nil || len(content) != 4 || content[3].Content != "content remains writable" ||
				metaErr != nil || meta.HistoryStreamID != expected[0].Order.StreamID || meta.HistorySequence != 3 {
				t.Fatal(
					"fenced content retry reconstructed order or lost durable content",
					content,
					contentErr,
					meta,
					metaErr,
				)
			}
			after, afterErr := os.ReadFile(reopened.historyRecoveryRequiredPath(key))
			if afterErr != nil || !reflect.DeepEqual(after, marker) {
				t.Fatal("readable retry changed a legitimate recovery marker", afterErr)
			}
			requireContentRecoveryFence(t, reopened, key)
		})
	}
}
