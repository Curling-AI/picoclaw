package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/memory/testdata/lineagefixture"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func contentPendingLoop(t *testing.T, expired bool) (*AgentLoop, *memory.JSONLStore, string, string) {
	t.Helper()
	dir := t.TempDir()
	al, store, _, key := newContentCompactionLoopAt(t, dir)
	if err := al.contextManager.Compact(t.Context(), &CompactRequest{
		SessionKey: key, Reason: ContextCompressReasonRetry,
	}); err != nil {
		t.Fatal(err)
	}
	if expired {
		provenance, err := lineagefixture.LegacyArchiveDeadline(dir, key, time.Now().Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		t.Log("pre-typed inline expiration fixture", provenance)
		store, err = memory.NewJSONLStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		al.registry.GetDefaultAgent().Sessions = session.NewJSONLBackend(store)
	}
	return al, store, dir, key
}

func TestContentLineageExpiredArchiveStillReturnsCompletedTurn(t *testing.T) {
	al, store, dir, key := contentPendingLoop(t, true)
	before, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := al.ProcessDirectWithChannel(t.Context(), "next user question", key, "test", "direct")
	history, historyErr := store.GetHistory(t.Context(), key)
	if historyErr != nil || len(history) != 5 || history[3].Content != "next user question" ||
		history[4].Content != "test" {
		t.Fatal("expired archive stopped durable turn appends", history, historyErr)
	}
	if turnErr != nil || response != "test" {
		t.Fatalf("durable turn was hidden by archive backpressure: response=%q error=%v", response, turnErr)
	}
	after, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("expired archive discarded its retained physical source", err)
	}
	if pending, pendingErr := store.ReadPendingHistoryCompaction(
		t.Context(),
		key,
	); !errors.Is(pendingErr, memory.ErrContentLineageExpired) ||
		pending.MutationID == "" {
		t.Fatal("archive backpressure disappeared from its explicit API", pending, pendingErr)
	}
}

type contentSummaryCountingProvider struct {
	calls int
}

func (p *contentSummaryCountingProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	return &providers.LLMResponse{Content: "summary"}, nil
}

func (*contentSummaryCountingProvider) GetDefaultModel() string { return "mock-model" }

func TestContentLineagePendingStopsSummaryWorkBeforeProvider(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, mode := range []string{"summarize", "force"} {
			t.Run(map[bool]string{false: "pending", true: "expired"}[expired]+"/"+mode, func(t *testing.T) {
				al, store, _, key := contentPendingLoop(t, expired)
				for i := range 12 {
					role := "user"
					if i%2 == 1 {
						role = "assistant"
					}
					if err := store.AddMessage(t.Context(), key, role, "more content"); err != nil {
						t.Fatal(err)
					}
				}
				before, err := store.ReadHistoryContentFrame(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				agent := al.registry.GetDefaultAgent()
				provider := &contentSummaryCountingProvider{}
				agent.Provider, agent.KeepLastMessages = provider, 2
				manager := al.contextManager.(*legacyContextManager)
				if mode == "summarize" {
					manager.summarizeSession(agent, key)
				} else {
					_, changed, compactErr := manager.forceCompressionContext(t.Context(), key, 0)
					if changed || (!errors.Is(compactErr, memory.ErrContentLineagePending) &&
						!errors.Is(compactErr, memory.ErrContentLineageExpired)) {
						t.Fatal("pending force compression did not expose its guard", changed, compactErr)
					}
				}
				if provider.calls != 0 {
					t.Fatalf("uncommittable summary called provider %d times", provider.calls)
				}
				after, err := store.ReadHistoryContentFrame(t.Context(), key)
				if err != nil || before.StateToken != after.StateToken || before.Summary != after.Summary ||
					len(before.Entries) != len(after.Entries) {
					t.Fatal("pending compaction partially changed the source", after, err)
				}
			})
		}
	}
}

func TestContentLineageSummaryResumesAfterQualifiedArchive(t *testing.T) {
	al, store, _, key := contentPendingLoop(t, true)
	pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
	if !errors.Is(err, memory.ErrContentLineageExpired) {
		t.Fatal(err)
	}
	if err = store.AcknowledgeHistoryCompaction(t.Context(), key, memory.ContentArchiveAck{
		DomainID: pending.DomainID, MutationID: pending.MutationID,
		BeforeState: pending.BeforeState, AfterState: pending.AfterState,
		Archived: pending.Dropped, ArchiveCommitID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err = store.AddMessage(t.Context(), key, role, "fresh turn"); err != nil {
			t.Fatal(err)
		}
	}
	agent := al.registry.GetDefaultAgent()
	provider := &contentSummaryCountingProvider{}
	agent.Provider, agent.KeepLastMessages = provider, 2
	al.contextManager.(*legacyContextManager).summarizeSession(agent, key)
	if provider.calls == 0 {
		t.Fatal("qualified archive left future summarization permanently disabled")
	}
	if next, nextErr := store.ReadPendingHistoryCompaction(
		t.Context(),
		key,
	); nextErr != nil || next.MutationID == "" ||
		next.MutationID == pending.MutationID {
		t.Fatal("future summary did not produce its own transition", next, nextErr)
	}
}
