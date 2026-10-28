package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type intervalInterleavedContentBackend struct {
	*session.JSONLBackend
	before []memory.ContentFrame
	after  []memory.ContentFrame
}

func (backend *intervalInterleavedContentBackend) AppendHistoryContent(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.ContentAppendResult, error) {
	before, err := backend.ReadHistoryContentFrame(ctx, key)
	if err != nil {
		return memory.ContentAppendResult{}, err
	}
	result, err := backend.JSONLBackend.AppendHistoryContent(ctx, key, message)
	if err != nil {
		return memory.ContentAppendResult{}, err
	}
	after, err := backend.ReadHistoryContentFrame(ctx, key)
	if err != nil {
		return memory.ContentAppendResult{}, err
	}
	backend.before = append(backend.before, before)
	backend.after = append(backend.after, after)
	_, err = backend.JSONLBackend.AppendHistoryContent(ctx, key, providers.Message{
		Role: "assistant", Content: "B", Delivery: &providers.Delivery{Origin: "cron:job:run-B"},
	})
	return result, err
}

func TestContentLineageCommitObserverPreservesInterleavedIntervalsBeforeIngest(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &intervalInterleavedContentBackend{JSONLBackend: session.NewJSONLBackend(store)}
	agent.Sessions = backend
	key := "sk_v1_commit_observer_interleave"
	before, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
		t.Fatal(err)
	}
	observed := false
	al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
	var captured []memory.ContentAppendResult
	al.SetSessionHistoryContentCommitObserver(func(_ string, results []memory.ContentAppendResult) {
		captured = results
		observed = true
	})
	if err = al.writeMirrored(agent, key, []providers.Message{
		{Role: "assistant", Content: "A1", Delivery: &providers.Delivery{Origin: "cron:job:run-A1"}},
		{Role: "assistant", Content: "A2", Delivery: &providers.Delivery{Origin: "cron:job:run-A2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 2 || len(backend.before) != 2 {
		t.Fatal("observer did not retain each committed append", captured)
	}
	for i, result := range captured {
		if result.DomainID != backend.before[i].DomainID || result.BeforeState != backend.before[i].StateToken ||
			result.AfterState != backend.after[i].StateToken || result.Entry.Ref == nil ||
			result.Entry.Message.Content != []string{"A1", "A2"}[i] || result.Receipt == nil ||
			result.Receipt.Message.HistoryID == "" || !result.Receipt.Order.Valid() ||
			result.Receipt.Message.HistoryOrderVersion != result.Receipt.Order.Version ||
			result.Receipt.Message.HistoryStreamID != result.Receipt.Order.StreamID ||
			result.Receipt.Message.HistorySequence != result.Receipt.Order.Sequence {
			t.Fatal("observer substituted a later tail or lost the exact receipt", i, result)
		}
	}
	if captured[0].AfterState == captured[1].BeforeState {
		t.Fatal("observer joined state intervals across an intervening B append")
	}
	actual, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(actual.Entries) != 4 || captured[1].AfterState == actual.StateToken {
		t.Fatal("observer invented one aggregate interval for interleaved commits", actual, err)
	}
}

func TestContentLineageCommitObserverPayloadIsDetachedFromOtherObservers(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := session.NewJSONLBackend(store)
	agent.Sessions = backend
	key := "sk_v1_commit_observer_detached"
	before, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); err != nil {
		t.Fatal(err)
	}
	var captured []memory.ContentAppendResult
	al.SetSessionHistoryContentCommitObserver(
		func(_ string, results []memory.ContentAppendResult) { captured = results },
	)
	al.SetSessionHistoryContentObserver(func(_ string, entries []memory.ContentEntry) {
		if len(captured) != 1 {
			t.Fatal("entry-only observer ran before the commit snapshot was retained")
		}
		entries[0].Message.Delivery.Origin = "mutated-by-v5-observer"
		entries[0].Ref.OccurrenceID = "mutated-by-v5-observer"
	})
	al.SetSessionHistoryReceiptObserver(func(_ string, receipts []memory.CommittedHistoryMessage) {
		if len(receipts) == 1 {
			receipts[0].Message.Delivery.Origin = "mutated-by-receipt-observer"
		}
	})
	if err = al.writeMirrored(agent, key, []providers.Message{{
		Role: "assistant", Content: "A", Delivery: &providers.Delivery{Origin: "cron:job:run-A"},
	}}); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].Entry.Ref == nil || !captured[0].Entry.Ref.Valid() ||
		captured[0].Entry.Message.Delivery.Origin != "cron:job:run-A" || captured[0].Receipt == nil ||
		captured[0].Receipt.Message.Delivery.Origin != "cron:job:run-A" ||
		captured[0].Receipt.Message.HistoryID == "" {
		t.Fatal("another observer mutated the retained exact commit", captured)
	}
	actual, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(actual.Entries) != 1 || *actual.Entries[0].Ref != *captured[0].Entry.Ref ||
		actual.Entries[0].Message.Delivery.Origin != "cron:job:run-A" {
		t.Fatal("observer mutation reached durable source", actual, err)
	}
}
