package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type saveOnlyInputBackend struct {
	session.SessionStore
	saveError error
	saves     int
}

func (b *saveOnlyInputBackend) Save(key string) error {
	b.saves++
	if b.saveError != nil {
		return b.saveError
	}
	return b.SessionStore.Save(key)
}

func TestContentInputLegacySaveConfirmsBeforeNotificationAndIngest(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "failed"}[failed], func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			dir := t.TempDir()
			backend := &saveOnlyInputBackend{SessionStore: session.NewSessionManager(dir)}
			failure := errors.New("legacy persistence failed")
			if failed {
				backend.saveError = failure
			}
			agent.Sessions = backend
			key := "agent:legacy-input-save"
			observed := false
			al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
			al.SetSessionHistoryContentInputObserver(func(_ string, entries []memory.ContentEntry) {
				if failed {
					t.Fatal("memory-only fallback announced a durable input")
				}
				persisted := session.NewSessionManager(dir).GetHistory(key)
				if len(persisted) != 1 || len(entries) != 1 || persisted[0].Content != entries[0].Message.Content {
					t.Fatal("input callback ran before actual legacy Save", persisted, entries)
				}
				observed = true
			})
			err := al.writeMirrored(agent, key, []providers.Message{{Role: "assistant", Content: "save-gated body"}})
			if (failed && !errors.Is(err, failure)) || (!failed && err != nil) || backend.saves != 1 ||
				observed == failed {
				t.Fatal("legacy input persistence gate failed or Save repeated", backend.saves, observed, err)
			}
		})
	}
}

type countedContentInputBackend struct {
	*session.JSONLBackend
	inputs, qualified int
}

func (b *countedContentInputBackend) AppendHistoryContentInput(
	ctx context.Context,
	key string,
	msg providers.Message,
) (memory.ContentAppendResult, error) {
	b.inputs++
	return b.JSONLBackend.AppendHistoryContentInput(ctx, key, msg)
}

func (b *countedContentInputBackend) AppendHistoryContent(
	ctx context.Context,
	key string,
	msg providers.Message,
) (memory.ContentAppendResult, error) {
	b.qualified++
	return b.JSONLBackend.AppendHistoryContent(ctx, key, msg)
}

func TestContentInputObserverPrecedesIngestAndPreservesQualifiedSubscribers(t *testing.T) {
	for _, qualified := range []bool{false, true} {
		for _, lostOrder := range []bool{false, true} {
			name := map[bool]string{false: "accepted/", true: "qualified/"}[qualified] + map[bool]string{false: "healthy", true: "lost-order"}[lostOrder]
			t.Run(name, func(t *testing.T) {
				al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
				defer cleanup()
				dir := t.TempDir()
				store, err := memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := &countedContentInputBackend{JSONLBackend: session.NewJSONLBackend(store)}
				agent.Sessions = backend
				key := "sk_v1_accepted_observer"
				backend.AddMessage(key, "user", "prior")
				frame, err := backend.ReadHistoryContentFrame(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = backend.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation); err != nil {
					t.Fatal(err)
				}
				if lostOrder {
					if err = os.Remove(filepath.Join(dir, key+".jsonl.order-witness")); err != nil {
						t.Fatal(err)
					}
				}
				observed := false
				al.contextManager = &receiptBeforeIngestManager{observed: &observed, t: t}
				var accepted []memory.ContentEntry
				var qualifiedBatch []memory.ContentAppendResult
				al.SetSessionHistoryContentInputObserver(
					func(_ string, entries []memory.ContentEntry) { accepted = entries; observed = true },
				)
				if qualified {
					al.SetSessionHistoryContentCommitObserver(
						func(_ string, results []memory.ContentAppendResult) { qualifiedBatch = results },
					)
				}
				proofs := 0
				al.SetSessionHistoryReceiptObserver(
					func(_ string, receipts []memory.CommittedHistoryMessage) { proofs += len(receipts) },
				)
				if err = al.writeMirrored(
					agent,
					key,
					[]providers.Message{
						{
							Role:     "assistant",
							Content:  "one accepted response",
							Delivery: &providers.Delivery{Origin: "cron:one"},
						},
					},
				); err != nil {
					t.Fatal(err)
				}
				if len(accepted) != 1 || accepted[0].Message.Content != "one accepted response" ||
					accepted[0].Ref != nil ||
					accepted[0].ReceiptBinding != nil ||
					!observed {
					t.Fatal("input wake-up was qualified, late or absent", accepted)
				}
				if qualified {
					if backend.qualified != 1 || backend.inputs != 0 || len(qualifiedBatch) != 1 ||
						qualifiedBatch[0].Entry.Ref == nil ||
						qualifiedBatch[0].BeforeState == "" ||
						qualifiedBatch[0].AfterState == "" {
						t.Fatal(
							"qualified subscriber was downgraded or duplicated",
							backend.inputs,
							backend.qualified,
							qualifiedBatch,
						)
					}
				} else if backend.inputs != 1 || backend.qualified != 0 || len(qualifiedBatch) != 0 {
					t.Fatal("ordinary wake-up requested full qualification", backend.inputs, backend.qualified)
				}
				if (lostOrder && proofs != 0) || (!lostOrder && proofs != 1) {
					t.Fatal("independent order was invented or discarded", lostOrder, proofs)
				}
				history, err := store.GetHistory(t.Context(), key)
				if err != nil || len(history) != 2 || history[1].Content != "one accepted response" {
					t.Fatal("observer lanes duplicated/lost input", history, err)
				}
			})
		}
	}
}
