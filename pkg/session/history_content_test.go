package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type checkedContentBackend interface {
	ReadHistoryContent(context.Context, string) ([]providers.Message, error)
}

func checkedBackend(t *testing.T, backend *session.JSONLBackend) checkedContentBackend {
	t.Helper()
	reader, ok := any(backend).(checkedContentBackend)
	if !ok {
		t.Fatal("the backend has no checked content-only reader")
	}
	return reader
}

func TestJSONLBackendCheckedContentPreservesOrdinaryMetadataAndErrors(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := session.NewJSONLBackend(store)
	key, alias := "sk_v1_checked_backend", "agent:main:checked-backend"
	at := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	message := providers.Message{
		Role: "assistant", Content: "durable", CreatedAt: &at,
		Delivery: &providers.Delivery{Origin: "cron:source", At: at.Add(-time.Second)},
	}
	if _, err := backend.AppendHistoryReceipt(t.Context(), key, message); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(t.Context(), key, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, key+".jsonl.order-witness")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, memory.ErrHistoryOrderUnavailable) {
		t.Fatal("missing proof still produced receipts", err)
	}
	content, err := checkedBackend(t, backend).ReadHistoryContent(t.Context(), alias)
	if err != nil || len(content) != 1 || content[0].CreatedAt == nil || !content[0].CreatedAt.Equal(at) ||
		content[0].Delivery == nil || content[0].Delivery.Origin != message.Delivery.Origin || !content[0].Delivery.At.Equal(message.Delivery.At) ||
		content[0].HistoryID != "" || content[0].HistoryOrderVersion != 0 {
		t.Fatal("checked backend lost ordinary metadata or exposed proof", content, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if content, err := checkedBackend(
		t,
		backend,
	).ReadHistoryContent(canceled, alias); !errors.Is(err, context.Canceled) ||
		content != nil {
		t.Fatal("backend suppressed canceled content reading", content, err)
	}
	if err := os.Remove(filepath.Join(dir, key+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if content, err := checkedBackend(
		t,
		backend,
	).ReadHistoryContent(t.Context(), alias); !errors.Is(err, os.ErrNotExist) ||
		content != nil {
		t.Fatal("backend suppressed a missing durable file", content, err)
	}
}

func TestJSONLBackendCheckedContentDoesNotUseUncheckedUnsupportedStore(t *testing.T) {
	backend := session.NewJSONLBackend(fullDiskStore{})
	content, err := checkedBackend(t, backend).ReadHistoryContent(t.Context(), "unsupported")
	if !errors.Is(err, memory.ErrHistoryUnsupported) || content != nil {
		t.Fatal("unsupported store silently fell back to unchecked history", content, err)
	}
}
