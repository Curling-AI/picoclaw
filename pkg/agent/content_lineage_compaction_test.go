package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func newContentCompactionLoop(t *testing.T) (*AgentLoop, *memory.JSONLStore, memory.ContentFrame, string) {
	t.Helper()
	return newContentCompactionLoopAt(t, t.TempDir())
}

func newContentCompactionLoopAt(
	t *testing.T,
	dir string,
) (*AgentLoop, *memory.JSONLStore, memory.ContentFrame, string) {
	t.Helper()
	al := newCMTestAgentLoop(testConfig(t))
	t.Cleanup(al.Close)
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_context_compaction"
	backend := session.NewJSONLBackend(store)
	al.registry.GetDefaultAgent().Sessions = backend
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, role := range []string{"user", "assistant", "user", "assistant", "user"} {
		if err = backend.AppendMessage(
			key,
			providers.Message{Role: role, Content: "same", CreatedAt: &at},
		); err != nil {
			t.Fatal(err)
		}
	}
	before, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation)
	if err != nil {
		t.Fatal(err)
	}
	return al, store, frame, key
}

func TestContentLineageContextCompactionKeepsExactTransitionAfterLateCron(t *testing.T) {
	al, store, before, key := newContentCompactionLoop(t)
	events, closeEvents := subscribeRuntimeEventsForTest(t, al, 8, runtimeevents.KindAgentContextCompress)
	defer closeEvents()
	if err := al.contextManager.Compact(
		t.Context(),
		&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
	); err != nil {
		t.Fatal(err)
	}
	event := waitForRuntimeEvent(
		t,
		events,
		time.Second,
		func(event runtimeevents.Event) bool { return event.Kind == runtimeevents.KindAgentContextCompress },
	)
	payload, ok := event.Payload.(ContextCompressPayload)
	if !ok || payload.ContentTransition == nil {
		t.Fatal("ContextManager committed no immutable content transition")
	}
	transition := *payload.ContentTransition
	if len(transition.Dropped) != 2 || len(transition.Retained) != 3 ||
		transition.Dropped[1] != *before.Entries[1].Ref ||
		transition.Retained[1] != *before.Entries[3].Ref {
		t.Fatal("duplicate answers changed occurrences", transition)
	}
	if err := store.AddFullMessage(
		t.Context(),
		key,
		providers.Message{
			Role:     "assistant",
			Content:  "late cron",
			Delivery: &providers.Delivery{Origin: "cron:job:run-b", At: time.Now()},
		},
	); err != nil {
		t.Fatal(err)
	}
	committed, err := store.ReadHistoryCompactionFrame(t.Context(), key, transition.MutationID, memory.ContentAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Entries) != 3 || committed.StateToken != transition.AfterState ||
		committed.Entries[2].Message.Content != "same" {
		t.Fatal("late cron substituted the committed postimage", committed)
	}
	current, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(current.Entries) != 4 || current.Entries[3].Message.Delivery.Origin != "cron:job:run-b" ||
		current.StateToken == transition.AfterState {
		t.Fatal("late cron was lost or mistaken for committed postimage", current, err)
	}
	if !reflect.DeepEqual(transition, *payload.ContentTransition) {
		t.Fatal("event changed after a later append")
	}
}

func TestContentLineageContextCompactionPropagatesCancellation(t *testing.T) {
	al, store, before, key := newContentCompactionLoop(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := al.contextManager.Compact(ctx, &CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled compaction did not preserve cancellation", err)
	}
	after, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || after.StateToken != before.StateToken || len(after.Entries) != len(before.Entries) {
		t.Fatal("canceled compaction changed durable content", after, err)
	}
}

func TestContentLineageSummaryRejectsConcurrentAppendWithoutPartialSummary(t *testing.T) {
	al, store, before, key := newContentCompactionLoop(t)
	agent := al.registry.GetDefaultAgent()
	agent.KeepLastMessages = 2
	agent.Provider = &appendingProvider{sessions: agent.Sessions, key: key}
	events, closeEvents := subscribeRuntimeEventsForTest(t, al, 8, runtimeevents.KindAgentSessionSummarize)
	defer closeEvents()
	manager := al.contextManager.(*legacyContextManager)
	manager.summarizeSession(agent, key)
	after, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(after.Entries) != len(before.Entries)+1 || after.Summary != before.Summary ||
		after.Entries[len(after.Entries)-1].Message.Content != "entrega durante o resumo" {
		t.Fatal("summary applied a stale cut or lost a concurrent append", after, err)
	}
	if pending, pendingErr := store.ReadPendingHistoryCompaction(
		t.Context(),
		key,
	); pendingErr != nil ||
		pending.MutationID != "" {
		t.Fatal("rejected CAS created a committed transition", pending, pendingErr)
	}
	if len(collectRuntimeEventStream(events)) != 0 {
		t.Fatal("rejected CAS emitted a successful summary event")
	}
}

func TestContentLineageContextCompactionWorksWithoutOrderWitness(t *testing.T) {
	dir := t.TempDir()
	al, store, before, key := newContentCompactionLoopAt(t, dir)
	if err := os.Remove(filepath.Join(dir, key+".jsonl.order-witness")); err != nil {
		t.Fatal(err)
	}
	if err := al.contextManager.Compact(
		t.Context(),
		&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
	); err != nil {
		t.Fatal("independent content compaction required an order receipt", err)
	}
	after, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || after.IdentityStatus != memory.ContentIdentityKnown || len(after.Entries) != 3 ||
		*after.Entries[0].Ref != *before.Entries[2].Ref {
		t.Fatal("order loss changed independent retained occurrences", after, err)
	}
	for _, entry := range after.Entries {
		if entry.ReceiptBinding != nil {
			t.Fatal("compaction reattested an unavailable order receipt")
		}
	}
	if _, err = store.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, memory.ErrHistoryReceiptUnavailable) {
		t.Fatal("content compaction reopened order receipts", err)
	}
}
