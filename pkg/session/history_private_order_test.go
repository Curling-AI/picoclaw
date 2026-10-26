package session_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestProviderHistoryIgnoresInvalidPrivateOrderWhileReceiptsAndRewritesRejectIt(t *testing.T) {
	for name, order := range map[string]json.RawMessage{
		"null":        json.RawMessage(`null`),
		"future":      json.RawMessage(`{"version":2,"streamId":"11111111-1111-4111-8111-111111111111","sequence":2}`),
		"wrong shape": json.RawMessage(`"corrupt private metadata"`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_private_order_provider"
			for _, content := range []string{"earlier content", "visible content"} {
				if _, appendErr := store.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: content},
				); appendErr != nil {
					t.Fatal(appendErr)
				}
			}
			dataPath := filepath.Join(dir, key+".jsonl")
			raw, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			rows := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
			var record map[string]json.RawMessage
			if decodeErr := json.Unmarshal(rows[1], &record); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			record["history_order"] = order
			rows[1], err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			changed := append(bytes.Join(rows, []byte("\n")), '\n')
			if writeErr := os.WriteFile(dataPath, changed, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			metaPath := filepath.Join(dir, key+".meta.json")
			rawMeta, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]json.RawMessage
			if decodeErr := json.Unmarshal(rawMeta, &meta); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			meta["history_bytes"], err = json.Marshal(len(changed))
			if err != nil {
				t.Fatal(err)
			}
			rawMeta, err = json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(metaPath, rawMeta, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			store, err = memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := session.NewJSONLBackend(store)
			history := backend.GetHistory(key)
			if len(history) != 2 || history[0].Content != "earlier content" || history[1].Content != "visible content" {
				t.Fatal("private proof corruption hid readable provider content", history)
			}
			for _, message := range history {
				if message.HistoryOrderVersion != 0 || message.HistoryStreamID != "" || message.HistorySequence != 0 {
					t.Fatal("provider retained private ordering metadata", message)
				}
			}
			if _, readErr := backend.ReadHistoryWithReceipts(
				t.Context(),
				key,
			); !errors.Is(
				readErr,
				memory.ErrHistoryOrderUnavailable,
			) {
				t.Fatal("receipt reader accepted invalid private proof", readErr)
			}
			history[1].Content = "attempted rewrite"
			if rewriteErr := store.SetHistory(
				t.Context(),
				key,
				history,
			); !errors.Is(
				rewriteErr,
				memory.ErrHistoryOrderUnavailable,
			) {
				t.Fatal("rewrite washed invalid private proof", rewriteErr)
			}
			after, err := os.ReadFile(dataPath)
			if err != nil || !bytes.Equal(after, changed) {
				t.Fatal("strict rewrite changed the durable transcript", err)
			}
		})
	}
}
