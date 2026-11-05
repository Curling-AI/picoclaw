package memory

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentLineageTypedAppendPreservesContentAfterCountLoss(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_typed_count_loss"
	for range 4 {
		if err = s.AddMessage(t.Context(), key, "user", "durable before"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, s, key)
	meta, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.Count = 0
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "durable after"},
	)
	if err != nil {
		t.Fatal("exact appender rejected normal content recovery", err)
	}
	if result.Receipt != nil || result.Entry.ReceiptBinding != nil || result.Entry.Ref != nil {
		t.Fatal("lost independent proof was reattested", result)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 3 || history[2].Content != "durable after" {
		t.Fatal("exact append lost content", history, err)
	}
}
