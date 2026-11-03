package memory

import (
	"bytes"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestNativeContentInitializationRequiresAnUnwrittenSource(t *testing.T) {
	for _, suffix := range []string{"", ".jsonl", ".meta.json", ".jsonl.content-domain", ".jsonl.order-witness", ".jsonl.recovery-required"} {
		t.Run(suffix, func(t *testing.T) {
			store, createErr := NewJSONLStore(t.TempDir())
			if createErr != nil {
				t.Fatal(createErr)
			}
			key := "sk_v1_new_native_source"
			path := store.dir + "/" + key + suffix
			before := []byte("retained source evidence")
			if suffix == ".meta.json" {
				before = []byte(
					`{"key":"` + key + `","count":0,"content_identity":null,"history_recovery_required":true}`,
				)
			}
			if suffix != "" {
				if err := os.WriteFile(path, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.InitializeNewHistoryContentIdentity(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			if suffix != "" {
				after, err := os.ReadFile(path)
				files, listErr := os.ReadDir(store.dir)
				if err != nil || listErr != nil || !bytes.Equal(before, after) || len(files) != 1 {
					t.Fatal("native initialization repaired or adopted an existing source", err, listErr)
				}
				return
			}
			result, err := store.AppendHistoryContent(
				t.Context(),
				key,
				providers.Message{Role: "user", Content: "first prompt"},
			)
			if err != nil || result.Entry.Ref == nil || !result.Entry.Ref.Valid() {
				t.Fatal("new native source did not bind its first append", result, err)
			}
		})
	}
}
