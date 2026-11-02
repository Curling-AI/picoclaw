package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestContentLineageAppendAfterIncompleteTailPreservesLegacyRecovery(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "warm", true: "cold"}[reopen], func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_content_partial_append"
			if err = s.AddMessage(t.Context(), key, "user", "durable before"); err != nil {
				t.Fatal(err)
			}
			before := initializedContent(t, s, key)
			file, err := os.OpenFile(s.jsonlPath(key), os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = file.WriteString(`{"role":"assistant","content":"interrupted append`); err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			physical, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReadHistoryContentFrame(t.Context(), key); !errors.Is(err, ErrContentStateChanged) {
				t.Fatal("checked getter silently accepted an incomplete tail", err)
			}
			unchanged, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(physical, unchanged) {
				t.Fatal("checked getter repaired the incomplete tail", err)
			}
			if reopen {
				s, err = NewJSONLStore(s.dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = s.AddMessage(t.Context(), key, "assistant", "durable after"); err != nil {
				t.Fatal("independent identity blocked legacy append recovery", err)
			}
			after, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || after.IdentityStatus != ContentIdentityKnown || len(after.Entries) != 2 ||
				after.Entries[0].Message.Content != "durable before" ||
				after.Entries[1].Message.Content != "durable after" ||
				*after.Entries[0].Ref != *before.Entries[0].Ref ||
				*after.Entries[1].Ref == *before.Entries[0].Ref {
				t.Fatal("append lost committed content or misbound an occurrence", after, err)
			}
		})
	}
}

func TestContentLineageAppendAfterMetadataCountLossPreservesContent(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_count_loss"
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
	if err = s.AddMessage(t.Context(), key, "assistant", "durable after"); err != nil {
		t.Fatal("lost identity bookkeeping blocked a normal content append", err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 3 || history[2].Content != "durable after" {
		t.Fatal("bookkeeping loss discarded content", history, err)
	}
	if _, err = s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("bookkeeping fallback restored order receipts", err)
	}
}
