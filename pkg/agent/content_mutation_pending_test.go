package agent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func typedPendingLoop(t *testing.T) (*AgentLoop, *memory.JSONLStore, string, string, memory.ContentMutationResult) {
	t.Helper()
	dir := t.TempDir()
	al, store, frame, key := newContentCompactionLoopAt(t, dir)
	refs := make([]memory.ContentRef, 2)
	for i := range refs {
		refs[i] = *frame.Entries[i].Ref
	}
	result, err := store.CommitHistoryContentMutation(t.Context(), key, memory.ContentMutationPlan{
		MutationID: uuid.NewString(), IntentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("owned resend request"))),
		DomainID: frame.DomainID, BeforeState: frame.StateToken, Kind: memory.ContentMutationRetainPrefix,
		RetainPrefix: &memory.ContentMutationRetainPrefixIntent{Retained: refs},
	})
	if err != nil {
		t.Fatal(err)
	}
	return al, store, dir, key, result
}

func TestContentMutationPendingSavePreservesCompletedTurnWithoutNewOrderProof(t *testing.T) {
	for _, lostOrder := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy-order", true: "lost-order"}[lostOrder], func(t *testing.T) {
			al, store, dir, key, pending := typedPendingLoop(t)
			meta, err := store.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			physicalBefore, err := os.ReadFile(filepath.Join(dir, key+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if lostOrder {
				if err = os.Remove(filepath.Join(dir, key+".jsonl.order-witness")); err != nil {
					t.Fatal(err)
				}
				store, err = memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				al.registry.GetDefaultAgent().Sessions = session.NewJSONLBackend(store)
			}

			response, turnErr := al.ProcessDirectWithChannel(t.Context(), "new prompt", key, "test", "direct")
			history, historyErr := store.GetHistory(t.Context(), key)
			if turnErr != nil || response != "test" || historyErr != nil || len(history) != 4 ||
				history[2].Content != "new prompt" || history[3].Content != "test" {
				t.Fatal("pending application hid a completed durable response", response, turnErr, history, historyErr)
			}
			physicalAfter, physicalErr := os.ReadFile(filepath.Join(dir, key+".jsonl"))
			if physicalErr != nil || !bytes.HasPrefix(physicalAfter, physicalBefore) {
				t.Fatal("pending Save rewrote retained source", physicalErr)
			}
			after, err := store.ReadPendingHistoryContentMutation(t.Context(), key)
			if err != nil || after.Transition.MutationID != pending.Transition.MutationID ||
				after.Status != memory.ContentMutationPending {
				t.Fatal("ordinary Save discarded the retained typed operation", after, err)
			}
			if lostOrder {
				current, readErr := store.GetSessionMeta(t.Context(), key)
				if readErr != nil || current.HistorySequence != meta.HistorySequence ||
					current.HistoryStreamID != meta.HistoryStreamID {
					t.Fatal("ordinary turn rebuilt a lost counter/stream", readErr)
				}
				if _, readErr = store.ReadHistoryWithReceipts(t.Context(), key); readErr == nil {
					t.Fatal("pending metadata/Save reattested lost order proof")
				}
			}
		})
	}
}

func TestContentMutationPendingStopsSummaryBeforeProvider(t *testing.T) {
	al, store, _, key, _ := typedPendingLoop(t)
	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := store.AddMessage(t.Context(), key, role, "more content"); err != nil {
			t.Fatal(err)
		}
	}
	agent := al.registry.GetDefaultAgent()
	provider := &contentSummaryCountingProvider{}
	agent.Provider, agent.KeepLastMessages = provider, 2
	before, err := store.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	manager := al.contextManager.(*legacyContextManager)
	manager.summarizeSession(agent, key)
	_, changed, compactErr := manager.forceCompressionContext(t.Context(), key, 0)
	if provider.calls != 0 || changed || !errors.Is(compactErr, memory.ErrContentMutationPendingKind) {
		t.Fatal("typed Pending allowed uncommittable summary provider work", provider.calls, changed, compactErr)
	}
	after, err := store.GetSessionMeta(t.Context(), key)
	if err != nil || before.Summary != after.Summary ||
		before.ContentIdentity.StateToken != after.ContentIdentity.StateToken {
		t.Fatal("summary guard partially changed a typed source", err)
	}
}
