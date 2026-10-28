package agent

import (
	"bytes"
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

func TestSessionHistoryUnsupportedOrderFormatPreservesMirroredDelivery(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	dir := t.TempDir()
	key := "sk_v1_witness_sync_delivery"
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first", "second", "third"} {
		if _, appendErr := store.AppendHistoryReceipt(
			t.Context(), key, providers.Message{Role: "assistant", Content: content},
		); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	meta, err := store.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.HistoryOrderWitnessVersion = 0
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, key+".meta.json"), data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	store, err = memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	agent.Sessions = session.NewJSONLBackend(store)
	callbacks, legacyCallbacks := 0, 0
	al.SetSessionHistoryReceiptObserver(func(_ string, committed []memory.CommittedHistoryMessage) {
		callbacks++
		if committed != nil {
			t.Error("unsupported order format produced an order notification", committed)
		}
	})
	al.SetSessionHistoryObserver(func(string) { legacyCallbacks++ })
	if mirrorErr := al.writeMirrored(agent, key, []providers.Message{
		{Role: "assistant", Content: "delivered once"},
		{Role: "assistant", Content: "later delivery"},
	}); mirrorErr != nil {
		t.Fatal("proof-only failure lost mirrored content", mirrorErr)
	}
	history := agent.Sessions.GetHistory(key)
	if len(history) != 5 || history[3].Content != "delivered once" || history[4].Content != "later delivery" ||
		callbacks != 1 || legacyCallbacks != 1 {
		t.Fatal("content-only delivery lost data or duplicated input", history, callbacks, legacyCallbacks)
	}
	if _, receiptErr := store.ReadHistoryWithReceipts(
		t.Context(),
		key,
	); !errors.Is(
		receiptErr,
		memory.ErrHistoryReceiptUnavailable,
	) {
		t.Fatal("content fallback restored order eligibility", receiptErr)
	}
	stored, err := store.GetSessionMeta(t.Context(), key)
	if err != nil || stored.HistoryStreamID != meta.HistoryStreamID || stored.HistorySequence != meta.HistorySequence {
		t.Fatal("content fallback changed committed stream/counter", stored, err)
	}
}

func (backend *uncertainReceiptBackend) AppendHistoryReceipt(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.CommittedHistoryMessage, error) {
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
				if operationErr := store.UpsertSessionMeta(
					t.Context(),
					item.canonical,
					nil,
					[]string{item.requested},
				); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			if writeErr := os.WriteFile(
				filepath.Join(dir, "000broken.meta.json"),
				[]byte(`{"key":"sk_v1_rev`),
				0o600,
			); writeErr != nil {
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
			writeErr := al.writeMirrored(
				agent,
				item.requested,
				[]providers.Message{{Role: "assistant", Content: "delivered"}},
			)
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

func TestSessionHistoryWitnessReadFailureDoesNotFallbackMirroredDelivery(t *testing.T) {
	for _, cache := range []string{"warm", "cold"} {
		t.Run(cache, func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			dir, key := t.TempDir(), "sk_v1_transient_witness_delivery"
			store, openErr := memory.NewJSONLStore(dir)
			if openErr != nil {
				t.Fatal(openErr)
			}
			first, appendErr := store.AppendHistoryReceipt(
				t.Context(), key, providers.Message{Role: "user", Content: "retained"},
			)
			if appendErr != nil {
				t.Fatal(appendErr)
			}
			path := filepath.Join(dir, key+".jsonl")
			before, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			witness := path + ".order-witness"
			if err := os.Rename(witness, witness+".io-fixture"); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				if restored {
					return
				}
				if err := os.Remove(witness); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.Rename(witness+".io-fixture", witness); err != nil {
					t.Fatal(err)
				}
				restored = true
			}
			t.Cleanup(restore)
			if err := os.Mkdir(witness, 0o700); err != nil {
				t.Fatal(err)
			}
			_, witnessErr := os.ReadFile(witness)
			var ioErr *os.PathError
			if !errors.As(witnessErr, &ioErr) {
				t.Fatal("fixture did not produce a real witness read error", witnessErr)
			}
			if cache == "cold" {
				reopened, reopenErr := memory.NewJSONLStore(dir)
				if reopenErr != nil {
					t.Fatal(reopenErr)
				}
				store = reopened
			}
			agent.Sessions = session.NewJSONLBackend(store)
			callbacks := 0
			al.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) { callbacks++ })
			input := []providers.Message{{Role: "assistant", Content: "delivered once"}}
			writeErr := al.writeMirrored(agent, key, input)
			restore()
			if !errors.Is(writeErr, ioErr.Err) || errors.Is(writeErr, memory.ErrHistoryOrderUnavailable) ||
				callbacks != 0 {
				t.Fatal("witness I/O authorized an unproven delivery fallback", writeErr, callbacks)
			}
			after, afterErr := os.ReadFile(path)
			if afterErr != nil || !bytes.Equal(after, before) {
				t.Fatal("failed witness read still committed mirrored content", afterErr)
			}
			if _, markerErr := os.Stat(path + ".recovery-required"); !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatal("runtime fallback created a durable proof-loss marker", markerErr)
			}
			if retryErr := al.writeMirrored(agent, key, input); retryErr != nil {
				t.Fatal("runtime delivery stayed fenced after witness access recovered", retryErr)
			}
			receipts, receiptErr := store.ReadHistoryWithReceipts(t.Context(), key)
			if receiptErr != nil || len(receipts) != 2 || callbacks != 1 || receipts[0].Order != first.Order ||
				receipts[0].Message.HistoryID != first.Message.HistoryID || receipts[1].Order.StreamID != first.Order.StreamID ||
				receipts[1].Order.Sequence != 2 {
				t.Fatal("runtime retry lost or reattested an occurrence", receipts, receiptErr, callbacks)
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
	if operationErr := agent.Sessions.(session.CheckedAppender).AppendMessage(
		key,
		providers.Message{Role: "user", Content: "conversation"},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	al.mirror.parked = map[string][]bus.InboundMessage{
		key: {
			{
				Channel:    "system",
				ChatID:     "telegram:chat",
				SessionKey: key,
				SenderID:   "background",
				Content:    "finished result",
			},
		},
	}
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
	deferred, operationErr := al.deliverToConversation(
		agent,
		key,
		providers.Message{Role: "assistant", Content: "later delivery"},
	)
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
		if writeErr := al.writeMirrored(
			agent,
			key,
			[]providers.Message{{Role: "assistant", Content: content}},
		); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	history := agent.Sessions.GetHistory(key)
	if len(history) != 4 || history[1].Content != "durable tail" || history[3].Content != "second delivery" ||
		callbacks != 2 ||
		legacyCallbacks != 2 {
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
	if !errors.Is(writeErr, memory.ErrHistoryOrderUnavailable) || readErr != nil || len(history) != 1 ||
		backend.fallbackCalls != 0 ||
		callbacks != 0 {
		t.Fatal(
			"uncertain commit was retried or announced",
			writeErr,
			history,
			readErr,
			backend.fallbackCalls,
			callbacks,
		)
	}
	reconciled, readErr := store.ReadHistoryWithReceipts(t.Context(), key)
	if readErr != nil || len(reconciled) != 1 || !reconciled[0].Order.Valid() {
		t.Fatal("canonical reconciliation lost the durable event", reconciled, readErr)
	}
}

func TestSessionHistoryRecoveryKeepsRealTurnsAndRollbackAvailable(t *testing.T) {
	for _, fixture := range []string{"legacy count", "stale checkpoint", "count above rows"} {
		t.Run(fixture, func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			dir := t.TempDir()
			key := "sk_v1_real_turn_content_recovery"
			prefix := []byte(
				"{\"role\":\"user\",\"content\":\"original request\"}\n{\"role\":\"assistant\",\"content\":\"original answer\"}\n",
			)
			raw := append(
				append([]byte(nil), prefix...),
				[]byte("{\"role\":\"assistant\",\"content\":\"durable external answer\"}\n")...)
			meta := memory.SessionMeta{Key: key, Count: 2}
			if fixture == "count above rows" {
				meta.Count = 4
			}
			if fixture == "stale checkpoint" {
				size := int64(len(prefix))
				meta.HistoryBytes = &size
			}
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string][]byte{key + ".jsonl": raw, key + ".meta.json": data} {
				if writeErr := os.WriteFile(filepath.Join(dir, path), contents, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			store, err := memory.NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			backend := session.NewJSONLBackend(store)
			agent.Sessions = backend
			for _, input := range []string{"first live request", "second live request"} {
				if response, err := al.ProcessDirect(t.Context(), input, key); err != nil || response == "" {
					t.Fatal("real turn could not finalize its preserved content", response, err)
				}
			}
			before := backend.GetHistory(key)
			if len(before) != 7 || before[2].Content != "durable external answer" {
				t.Fatal("real turns lost durable history", before)
			}
			if err := backend.AppendMessage(
				key,
				providers.Message{Role: "user", Content: "aborted input"},
			); err != nil {
				t.Fatal(err)
			}
			ts := &turnState{sessionKey: key, restorePointHistory: before}
			if err := ts.restoreSession(agent); err != nil {
				t.Fatal("turn rollback could not save content", err)
			}
			after := backend.GetHistory(key)
			if len(after) != len(before) {
				t.Fatal("rollback retained the aborted input", after)
			}
			for index := range before {
				if before[index].Role != after[index].Role || before[index].Content != after[index].Content {
					t.Fatal("rollback changed retained content", index, before, after)
				}
			}
			if response, err := al.ProcessDirect(t.Context(), "after rollback", key); err != nil || response == "" {
				t.Fatal("turn after rollback could not complete", response, err)
			}
			if clearErr := al.contextManager.Clear(t.Context(), key); clearErr != nil {
				t.Fatal("clear could not persist the requested content removal", clearErr)
			}
			if history := backend.GetHistory(key); len(history) != 0 {
				t.Fatal("clear reported success while keeping the conversation", history)
			}
			if response, turnErr := al.ProcessDirect(
				t.Context(),
				"after clear",
				key,
			); turnErr != nil ||
				response == "" {
				t.Fatal("turn after clear could not complete", response, turnErr)
			}
			if history := backend.GetHistory(key); len(history) != 2 {
				t.Fatal("turn after clear did not persist its request and answer", history)
			}
			if _, err := store.ReadHistoryWithReceipts(
				t.Context(),
				key,
			); !errors.Is(
				err,
				memory.ErrHistoryOrderUnavailable,
			) {
				t.Fatal("successful turns restored order receipts", err)
			}
		})
	}
}
