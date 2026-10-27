package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type checkedHistoryContentReader interface {
	ReadHistoryContent(context.Context, string) ([]providers.Message, error)
}

func checkedContentReader(t *testing.T, store *JSONLStore) checkedHistoryContentReader {
	t.Helper()
	reader, ok := any(store).(checkedHistoryContentReader)
	if !ok {
		t.Fatal("the store has no checked content-only reader")
	}
	return reader
}

func contentSourceFiles(t *testing.T, store *JSONLStore, key string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, path := range []string{store.jsonlPath(key), store.metaPath(key), store.historyOrderWitnessPath(key), store.historyRecoveryRequiredPath(key)} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(path)] = data
	}
	return files
}

func TestCheckedContentSurvivesProofLossWithoutRestoringProof(t *testing.T) {
	for _, loss := range []string{"witness", "metadata", "recovery marker"} {
		t.Run(loss, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_checked_content"
			at := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
			input := providers.Message{
				Role:         "assistant",
				Content:      "durable delivery",
				CreatedAt:    &at,
				HistoryRunID: "11111111-1111-4111-8111-111111111111",
				Delivery: &providers.Delivery{
					Origin: "cron:source",
					At:     at.Add(-time.Second),
				},
				Media: []string{"artifact://fixture"},
			}
			if _, err := store.AppendHistoryReceipt(t.Context(), key, input); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			switch loss {
			case "witness":
				if err := os.Remove(store.historyOrderWitnessPath(key)); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				if err := os.Remove(store.metaPath(key)); err != nil {
					t.Fatal(err)
				}
			case "recovery marker":
				if err := os.WriteFile(
					store.historyRecoveryRequiredPath(key),
					[]byte("required\n"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			}
			store, err := NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
				t.Fatal("proof loss did not fence receipts", err)
			}
			before := contentSourceFiles(t, store, key)
			history, err := checkedContentReader(t, store).ReadHistoryContent(t.Context(), key)
			if err != nil || len(history) != 1 || history[0].Content != input.Content ||
				history[0].CreatedAt == nil || !history[0].CreatedAt.Equal(at) ||
				!reflect.DeepEqual(
					history[0].Delivery,
					input.Delivery,
				) || !reflect.DeepEqual(history[0].Media, input.Media) {
				t.Fatal("proof loss hid ordinary content or delivery metadata", history, err)
			}
			if !reflect.DeepEqual(contentSourceFiles(t, store, key), before) {
				t.Fatal("reading content changed durable proof state or transcript bytes")
			}
			if err := store.AddMessage(t.Context(), key, "assistant", "next durable answer"); err != nil {
				t.Fatal(err)
			}
			history, err = checkedContentReader(t, store).ReadHistoryContent(t.Context(), key)
			if err != nil || len(history) != 2 || history[1].Content != "next durable answer" {
				t.Fatal("content-only append or subsequent read was lost", history, err)
			}
			for _, message := range history {
				if message.HistoryID != "" || message.HistoryRunID != "" || message.HistoryOrderVersion != 0 ||
					message.HistoryStreamID != "" ||
					message.HistorySequence != 0 {
					t.Fatal("checked content exposed private receipt provenance", message)
				}
			}
			if _, err := store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryOrderUnavailable) {
				t.Fatal("content-only read or append restored authority", err)
			}
		})
	}
}

func TestCheckedContentDistinguishesReadFailuresFromEmptyHistory(t *testing.T) {
	for _, failure := range []string{"missing content", "lost metadata and content", "recovery without content", "unknown byte checkpoint", "unreadable content", "invalid metadata", "wrong owner", "ambiguous alias", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			store := newTestStore(t)
			key := "sk_v1_checked_errors"
			if err := store.AddMessage(t.Context(), key, "user", "retained"); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch failure {
			case "missing content":
				if err := os.Remove(store.jsonlPath(key)); err != nil {
					t.Fatal(err)
				}
			case "unknown byte checkpoint":
				meta, err := store.GetSessionMeta(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				meta.Count, meta.HistoryBytes = 0, nil
				if err := store.writeMeta(key, meta); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(store.jsonlPath(key)); err != nil {
					t.Fatal(err)
				}
			case "lost metadata and content", "recovery without content":
				for _, path := range []string{store.jsonlPath(key), store.metaPath(key)} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "recovery without content" {
					if err := os.Remove(store.historyOrderWitnessPath(key)); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(
						store.historyRecoveryRequiredPath(key),
						[]byte("required\n"),
						0o600,
					); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				store, err = NewJSONLStore(store.dir)
				if err != nil {
					t.Fatal(err)
				}
			case "unreadable content":
				if err := os.Remove(store.jsonlPath(key)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(store.jsonlPath(key), 0o700); err != nil {
					t.Fatal(err)
				}
			case "invalid metadata":
				if err := os.WriteFile(store.metaPath(key), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				store, err = NewJSONLStore(store.dir)
				if err != nil {
					t.Fatal(err)
				}
			case "wrong owner":
				if err := store.writeMeta(key, SessionMeta{Key: "different_owner", Count: 1}); err != nil {
					t.Fatal(err)
				}
			case "ambiguous alias":
				for _, owner := range []string{key, "sk_v1_second_owner"} {
					if err := store.UpsertSessionMeta(
						t.Context(),
						owner,
						nil,
						[]string{"agent:main:ambiguous"},
					); err != nil {
						t.Fatal(err)
					}
				}
				key = "agent:main:ambiguous"
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			messages, err := checkedContentReader(t, store).ReadHistoryContent(ctx, key)
			if err == nil || messages != nil {
				t.Fatal("content failure became a successful empty history", messages, err)
			}
			if failure == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation was hidden", err)
			}
			if failure == "missing content" && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing durable transcript lost its I/O error", err)
			}
		})
	}
	store := newTestStore(t)
	read, err := checkedContentReader(t, store).ReadHistoryContent(t.Context(), "sk_v1_legitimately_empty")
	if err != nil || read == nil || len(read) != 0 {
		t.Fatal("a genuinely absent history is not an explicit empty result", read, err)
	}
	if raw, err := os.ReadFile(
		store.jsonlPath("sk_v1_legitimately_empty"),
	); !errors.Is(err, os.ErrNotExist) ||
		len(bytes.TrimSpace(raw)) != 0 {
		t.Fatal("empty content read created a transcript", err)
	}
	if err := store.UpsertSessionMeta(t.Context(), "sk_v1_owned_empty", nil, []string{"agent:main:empty"}); err != nil {
		t.Fatal(err)
	}
	if read, err := checkedContentReader(
		t,
		store,
	).ReadHistoryContent(t.Context(), "agent:main:empty"); err != nil || read == nil ||
		len(read) != 0 {
		t.Fatal("an owned new alias cannot return empty history", read, err)
	}
	if _, err := store.AppendHistoryReceipt(
		t.Context(),
		"sk_v1_explicitly_empty",
		providers.Message{Role: "user", Content: "to clear"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.SetHistory(t.Context(), "sk_v1_explicitly_empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.jsonlPath("sk_v1_explicitly_empty")); err != nil {
		t.Fatal(err)
	}
	if read, err := checkedContentReader(
		t,
		store,
	).ReadHistoryContent(t.Context(), "sk_v1_explicitly_empty"); err != nil || read == nil ||
		len(read) != 0 {
		t.Fatal("a committed empty checkpoint is not an empty history", read, err)
	}
}

func TestCheckedContentDecoderPreservesCancellationInsteadOfPartialSuccess(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		messages []providers.Message
		err      error
	}
	done := make(chan result, 1)
	go func() {
		messages, err := decodeHistoryMessages(ctx, reader, 0, true)
		done <- result{messages, err}
	}()
	if _, err := writer.Write([]byte("{\"role\":\"user\",\"content\":\"readable prefix\"}\n")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.messages != nil {
			t.Fatal("cancellation became a successful partial read", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("content decoder did not observe cancellation")
	}
}
