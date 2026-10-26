package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type uncertainReceiptBackend struct {
	*session.JSONLBackend
	fallbackCalls int
}

func (backend *uncertainReceiptBackend) AppendHistoryReceipt(ctx context.Context, key string, message providers.Message) (memory.CommittedHistoryMessage, error) {
	if _, err := backend.JSONLBackend.AppendHistoryReceipt(ctx, key, message); err != nil {
		return memory.CommittedHistoryMessage{}, err
	}
	return memory.CommittedHistoryMessage{}, memory.ErrHistoryOrderUnavailable
}

func (backend *uncertainReceiptBackend) AppendMessage(key string, message providers.Message) error {
	backend.fallbackCalls++
	return backend.JSONLBackend.AppendMessage(key, message)
}

func TestSessionHistoryReceiptOwnershipFailurePreservesMirroredDelivery(t *testing.T) {
	for _, item := range []struct{ name, requested, canonical string }{
		{"legacy key", "agent:main:review-alias-key", "agent:main:review-alias-key"},
		{"new kept session", "sk_v1_new_kept_session", "sk_v1_new_kept_session"},
		{"known alias", "agent:main:alias-delivery", "sk_v1_canonical_owner"},
	} {
		t.Run(item.name, func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			dir := t.TempDir()
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if item.requested != item.canonical {
				if operationErr := store.UpsertSessionMeta(t.Context(), item.canonical, nil, []string{item.requested}); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			if writeErr := os.WriteFile(filepath.Join(dir, "000broken.meta.json"), []byte(`{"key":"sk_v1_rev`), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			agent.Sessions = session.NewJSONLBackend(store)
			callbacks, legacyCallbacks := 0, 0
			al.SetSessionHistoryReceiptObserver(func(_ string, committed []memory.CommittedHistoryMessage) {
				callbacks++
				if committed != nil {
					t.Error("unproven ownership emitted live receipts", committed)
				}
			})
			al.SetSessionHistoryObserver(func(string) { legacyCallbacks++ })
			writeErr := al.writeMirrored(agent, item.requested, []providers.Message{{Role: "assistant", Content: "delivered"}})
			history, readErr := store.GetHistory(t.Context(), item.canonical)
			if writeErr != nil || readErr != nil || len(history) != 1 || history[0].Content != "delivered" {
				t.Fatal("unrelated broken metadata lost the mirrored delivery", writeErr, readErr, history)
			}
			if callbacks != 1 || legacyCallbacks != 1 {
				t.Fatal("confirmed fallback lost its unproven notification", callbacks, legacyCallbacks)
			}
			if item.requested != item.canonical {
				wrong, wrongErr := store.GetHistory(t.Context(), item.requested)
				if wrongErr != nil || len(wrong) != 0 {
					t.Fatal("fallback wrote the alias as a separate session", wrong, wrongErr)
				}
			}
		})
	}
}

func TestSessionHistoryReceiptPanicReleasesParkedResultStripe(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent.Sessions = session.NewJSONLBackend(store)
	key := "agent:main:receipt-panic"
	if operationErr := agent.Sessions.(session.CheckedAppender).AppendMessage(key, providers.Message{Role: "user", Content: "conversation"}); operationErr != nil {
		t.Fatal(operationErr)
	}
	al.mirror.parked = map[string][]bus.InboundMessage{key: {{Channel: "system", ChatID: "telegram:chat", SessionKey: key, SenderID: "background", Content: "finished result"}}}
	al.mirror.quiet = map[string]bool{key: true}
	const sentinel = "receipt observer panic"
	al.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) { panic(sentinel) })
	var observed any
	func() {
		defer func() { observed = recover() }()
		al.releaseParkedResults(key)
	}()
	if observed != sentinel {
		t.Fatal("receipt observer panic propagation changed", observed)
	}
	stripe := al.mirror.stripe(key)
	if !stripe.TryLock() {
		stripe.Unlock() // Only this fixture's panicking release path acquired it.
		t.Fatal("a committed observer panic left the session stripe locked")
	}
	stripe.Unlock()
	al.SetSessionHistoryReceiptObserver(nil)
	deferred, operationErr := al.deliverToConversation(agent, key, providers.Message{Role: "assistant", Content: "later delivery"})
	if operationErr != nil || deferred {
		t.Fatal("later delivery could not enter the conversation", deferred, operationErr)
	}
	history, readErr := store.GetHistory(t.Context(), key)
	if readErr != nil || len(history) != 3 || history[2].Content != "later delivery" {
		t.Fatal("panic cleanup lost durable history", history, readErr)
	}
}

func TestSessionHistoryUnprovenPrefixPersistsWithOnlyLegacyNotifications(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	dir := t.TempDir()
	key := "sk_v1_runtime_without_proof"
	raw := []byte("{\"role\":\"user\",\"content\":\"legacy\"}\n{\"role\":\"assistant\",\"content\":\"durable tail\"}\n")
	if writeErr := os.WriteFile(filepath.Join(dir, key+".jsonl"), raw, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	meta, err := json.Marshal(memory.SessionMeta{Key: key, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, key+".meta.json"), meta, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	agent.Sessions = session.NewJSONLBackend(store)
	callbacks, legacyCallbacks := 0, 0
	al.SetSessionHistoryReceiptObserver(func(_ string, committed []memory.CommittedHistoryMessage) {
		callbacks++
		if committed != nil {
			t.Error("unproven content was called an exact order receipt", committed)
		}
	})
	al.SetSessionHistoryObserver(func(string) { legacyCallbacks++ })
	for _, content := range []string{"first delivery", "second delivery"} {
		if writeErr := al.writeMirrored(agent, key, []providers.Message{{Role: "assistant", Content: content}}); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	history := agent.Sessions.GetHistory(key)
	if len(history) != 4 || history[1].Content != "durable tail" || history[3].Content != "second delivery" || callbacks != 2 || legacyCallbacks != 2 {
		t.Fatal("content-only fallback lost data or notification", history, callbacks, legacyCallbacks)
	}
	if _, readErr := store.ReadHistoryWithReceipts(t.Context(), key); readErr == nil {
		t.Fatal("content-only session exposed trusted receipts")
	}
}

func TestSessionHistoryUncertainCommitDoesNotRetryThroughLegacyFallback(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &uncertainReceiptBackend{JSONLBackend: session.NewJSONLBackend(store)}
	agent.Sessions = backend
	callbacks := 0
	al.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) { callbacks++ })
	al.SetSessionHistoryObserver(func(string) { callbacks++ })
	key := "sk_v1_uncertain_mirror"
	writeErr := al.writeMirrored(agent, key, []providers.Message{{Role: "assistant", Content: "already durable"}})
	history, readErr := store.GetHistory(t.Context(), key)
	if !errors.Is(writeErr, memory.ErrHistoryOrderUnavailable) || readErr != nil || len(history) != 1 || backend.fallbackCalls != 0 || callbacks != 0 {
		t.Fatal("uncertain commit was retried or announced", writeErr, history, readErr, backend.fallbackCalls, callbacks)
	}
	reconciled, readErr := store.ReadHistoryWithReceipts(t.Context(), key)
	if readErr != nil || len(reconciled) != 1 || !reconciled[0].Order.Valid() {
		t.Fatal("canonical reconciliation lost the durable event", reconciled, readErr)
	}
}
