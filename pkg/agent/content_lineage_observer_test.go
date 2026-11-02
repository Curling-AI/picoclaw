package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type interleavedContentBackend struct{ *session.JSONLBackend }

func (backend *interleavedContentBackend) AppendHistoryContent(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.ContentAppendResult, error) {
	result, err := backend.JSONLBackend.AppendHistoryContent(ctx, key, message)
	if err == nil {
		err = backend.JSONLBackend.AppendMessage(
			key,
			providers.Message{Role: "assistant", Content: "B", Delivery: &providers.Delivery{Origin: "cron:job:run-B"}},
		)
	}
	return result, err
}

func TestContentLineageObserverUsesCommittedEntryBeforeIngest(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	setter, ok := any(al).(interface {
		SetSessionHistoryContentObserver(observer func(string, []memory.ContentEntry))
	})
	if !ok {
		t.Fatal("agent has no independent committed-content observer")
	}
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &interleavedContentBackend{JSONLBackend: session.NewJSONLBackend(store)}
	agent.Sessions = backend
	key := "sk_v1_content_observer"
	before, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
		t.Fatal(err)
	}
	observed := false
	al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
	var captured []memory.ContentEntry
	setter.SetSessionHistoryContentObserver(
		func(_ string, entries []memory.ContentEntry) { captured = entries; observed = true },
	)
	if err = al.writeMirrored(
		agent,
		key,
		[]providers.Message{{Role: "assistant", Content: "A", Delivery: &providers.Delivery{Origin: "cron:job:run-A"}}},
	); err != nil {
		t.Fatal(err)
	}
	actual, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(actual.Entries) != 2 || len(captured) != 1 || captured[0].Ref == nil ||
		captured[0].Message.Content != "A" ||
		captured[0].Message.Delivery.Origin != "cron:job:run-A" ||
		*captured[0].Ref != *actual.Entries[0].Ref ||
		*captured[0].Ref == *actual.Entries[1].Ref {
		t.Fatal("observer relabeled a later tail as the committed occurrence", captured, actual, err)
	}
}

func TestContentLineageObserverPreservesCountLossContentWithoutInventingRef(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_observer_count_loss"
	for range 4 {
		if err = store.AddMessage(t.Context(), key, "user", "durable before"); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.TruncateHistory(t.Context(), key, 2); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.Count = 0
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, key+".meta.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	agent.Sessions = session.NewJSONLBackend(store)
	observed := false
	al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
	var captured []memory.ContentEntry
	al.SetSessionHistoryContentObserver(
		func(_ string, entries []memory.ContentEntry) { observed = true; captured = entries },
	)
	if err = al.writeMirrored(
		agent,
		key,
		[]providers.Message{
			{
				Role:     "assistant",
				Content:  "durable after",
				Delivery: &providers.Delivery{Origin: "cron:job:run-after-count-loss"},
			},
		},
	); err != nil {
		t.Fatal("new observer disabled existing content recovery", err)
	}
	if len(captured) != 1 || captured[0].Ref != nil || captured[0].ReceiptBinding != nil ||
		captured[0].Message.Delivery.Origin != "cron:job:run-after-count-loss" {
		t.Fatal("observer lost exact content or invented identity", captured)
	}
	history := agent.Sessions.GetHistory(key)
	if len(history) != 3 || history[2].Content != "durable after" {
		t.Fatal("observer path lost durable messages", history)
	}
}
