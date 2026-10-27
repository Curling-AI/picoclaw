package memory

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHistoryOriginReadFailureDoesNotLatchRecoveryAndCanRetry(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX file permissions for a non-root user")
	}
	for _, failure := range []string{"permission", "closed reader"} {
		for _, operation := range []string{"legacy content", "legacy receipt", "modern content", "modern read", "modern clear"} {
			t.Run(failure+"/"+operation, func(t *testing.T) {
				store := newTestStore(t)
				key := "sk_v1_transient_origin_reader"
				var expected []CommittedHistoryMessage
				legacy := strings.HasPrefix(operation, "legacy")
				if legacy {
					if writeErr := os.WriteFile(
						store.jsonlPath(key),
						[]byte("{\"role\":\"user\",\"content\":\"legacy\"}\n"),
						0o600,
					); writeErr != nil {
						t.Fatal(writeErr)
					}
					if writeErr := store.writeMeta(key, SessionMeta{Key: key, Count: 1}); writeErr != nil {
						t.Fatal(writeErr)
					}
				} else {
					store, key, expected = witnessedHistory(t)
					meta, readErr := store.readMeta(key)
					if readErr != nil {
						t.Fatal(readErr)
					}
					meta.HistoryOrderWitnessVersion = 0
					if writeErr := store.writeMeta(key, meta); writeErr != nil {
						t.Fatal(writeErr)
					}
					if removeErr := os.Remove(store.historyOrderWitnessPath(key)); removeErr != nil {
						t.Fatal(removeErr)
					}
					var reopenErr error
					store, reopenErr = NewJSONLStore(store.dir)
					if reopenErr != nil {
						t.Fatal(reopenErr)
					}
				}
				before, readErr := os.ReadFile(store.jsonlPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				beforeMeta, readErr := os.ReadFile(store.metaPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				injected := false
				store.openHistoryFile = func(path string) (*os.File, error) {
					if !injected {
						injected = true
						if failure == "permission" {
							if chmodErr := os.Chmod(path, 0); chmodErr != nil {
								return nil, chmodErr
							}
						} else {
							file, openErr := os.Open(path)
							if openErr != nil {
								return nil, openErr
							}
							if closeErr := file.Close(); closeErr != nil {
								return nil, closeErr
							}
							return file, nil // The real scanner/Stat must return the read error.
						}
					}
					return os.Open(path)
				}
				t.Cleanup(func() { _ = os.Chmod(store.jsonlPath(key), 0o600) })
				perform := func() error {
					switch operation {
					case "legacy receipt":
						_, appendErr := store.AppendHistoryReceipt(
							t.Context(),
							key,
							providers.Message{Role: "user", Content: "new"},
						)
						return appendErr
					case "modern read":
						_, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key)
						return receiptErr
					case "modern clear":
						return store.SetHistory(t.Context(), key, nil)
					default:
						return store.AddFullMessage(t.Context(), key, providers.Message{Role: "user", Content: "new"})
					}
				}
				operationErr := perform()
				if chmodErr := os.Chmod(store.jsonlPath(key), 0o600); chmodErr != nil {
					t.Fatal(chmodErr)
				}
				wantErr := os.ErrPermission
				if failure == "closed reader" {
					wantErr = os.ErrClosed
				}
				if !injected || !errors.Is(operationErr, wantErr) ||
					errors.Is(operationErr, ErrHistoryReceiptUnavailable) {
					t.Fatal("transient read error became a proof-loss classification", operationErr)
				}
				for path, want := range map[string][]byte{store.jsonlPath(key): before, store.metaPath(key): beforeMeta} {
					got, fileErr := os.ReadFile(path)
					if fileErr != nil || !bytes.Equal(got, want) {
						t.Fatal("failed read changed content, counter or metadata", path, fileErr)
					}
				}
				for _, path := range []string{store.historyRecoveryRequiredPath(key), store.historyOrderWitnessPath(key), store.historyRecoveryPath(key), store.historyDecisionPath(key)} {
					if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
						t.Fatal("failed read published private recovery or proof state", path, statErr)
					}
				}
				if retryErr := perform(); retryErr != nil {
					t.Fatal("healthy retry remained blocked after the I/O recovered", retryErr)
				}
				read, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key)
				if receiptErr != nil {
					t.Fatal(receiptErr)
				}
				meta, metaErr := store.GetSessionMeta(t.Context(), key)
				if metaErr != nil || meta.HistoryRecoveryRequired ||
					meta.HistoryOrderWitnessVersion != historyOrderWitnessVersion {
					t.Fatal("healthy retry retained a recovery fence or omitted the witness", meta, metaErr)
				}
				if legacy {
					if len(read) != 2 || read[0].Order != (HistoryOrderProof{}) ||
						read[0].LegacyBeforeStreamID != meta.HistoryStreamID ||
						read[1].Order.Sequence != 1 ||
						meta.HistorySequence != 1 {
						t.Fatal("retry invented order for genuine legacy origin", read, meta)
					}
				} else {
					wantCount, wantSequence := 3, uint64(3)
					if operation == "modern content" {
						wantCount, wantSequence = 4, 4
					} else if operation == "modern clear" {
						wantCount = 0
					}
					if len(read) != wantCount || meta.HistoryStreamID != expected[0].Order.StreamID ||
						meta.HistorySequence != wantSequence {
						t.Fatal("retry changed stream or reconstructed its highwater", read, meta)
					}
					if wantCount >= 3 && !reflect.DeepEqual(read[:3], expected) {
						t.Fatal("retry reattested established events", read, expected)
					}
				}
			})
		}
	}
}

func TestContentAppendAfterCheckpointBeyondEOFKeepsTranscript(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_cursor_beyond_eof"
	for _, content := range []string{"one", "two"} {
		if appendErr := store.AddFullMessage(
			t.Context(),
			key,
			providers.Message{Role: "user", Content: content},
		); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	meta, readErr := store.readMeta(key)
	if readErr != nil {
		t.Fatal(readErr)
	}
	beyond := *meta.HistoryBytes + 100
	meta.HistoryBytes = &beyond
	if writeErr := store.writeMeta(key, meta); writeErr != nil {
		t.Fatal(writeErr)
	}
	if appendErr := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{Role: "user", Content: "three"},
	); appendErr != nil {
		t.Fatal(appendErr)
	}
	history, readErr := store.GetHistory(t.Context(), key)
	if readErr != nil || len(history) != 3 || history[0].Content != "one" || history[1].Content != "two" ||
		history[2].Content != "three" {
		t.Fatal("invalid checkpoint lost the existing transcript", history, readErr)
	}
	requireContentRecoveryFence(t, store, key)
}
