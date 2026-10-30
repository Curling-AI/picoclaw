//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestSeahorseHardAbortPreservesCanonicalDeliveries(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "existing only"
		if queued {
			name = "concurrent delivery"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{
				Agents: config.AgentsConfig{
					Defaults: config.AgentDefaults{
						Workspace:         t.TempDir(),
						ModelName:         "test-model",
						MaxTokens:         4096,
						MaxToolIterations: 10,
						ContextManager:    "seahorse",
					},
				},
			}
			provider := &resultBlockingProvider{started: make(chan struct{}, 1)}
			loop := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
			agent := loop.registry.GetDefaultAgent()
			manager, ok := loop.contextManager.(*seahorseContextManager)
			if !ok {
				t.Fatal("real Seahorse context manager unavailable")
			}
			defer manager.engine.Close()
			writer, ok := agent.Sessions.(session.HistoryReceiptStore)
			if !ok {
				t.Fatal("durable JSONL receipt store unavailable")
			}
			key := "sk_v1_seahorse_delivery_restore"
			at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			receipts := make([]providers.Message, 0, 2)
			for range 2 {
				receipt, appendErr := writer.AppendHistoryReceipt(
					t.Context(),
					key,
					providers.Message{
						Role:      "assistant",
						Content:   "same delivery",
						CreatedAt: &at,
						Delivery:  &providers.Delivery{Origin: "cron:original", At: at},
					},
				)
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				if receipt.Message.Delivery == nil || uuid.Validate(receipt.Message.Delivery.ID) != nil {
					t.Fatal("fixture lacks durable occurrence identity", receipt)
				}
				receipts = append(receipts, receipt.Message)
			}
			if receipts[0].Delivery.ID == receipts[1].Delivery.ID {
				t.Fatal("identical deliveries reused one occurrence")
			}
			const summary = "source-owned summary"
			agent.Sessions.SetSummary(key, summary)
			before := agent.Sessions.GetHistory(key)
			manager.bootstrapSession(t.Context(), key)
			assembled, assembleErr := manager.Assemble(
				t.Context(),
				&AssembleRequest{SessionKey: key, Budget: 100000, MaxTokens: 4096},
			)
			if assembleErr != nil {
				t.Fatal(assembleErr)
			}
			if len(assembled.History) != 2 || assembled.History[0].Delivery != nil {
				t.Fatal("fixture did not exercise Seahorse's model-only projection", assembled)
			}
			loop.SetDeliverySessionResolver(func(string, string) string { return key })
			turnCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, turnErr := loop.ProcessDirectWithChannel(turnCtx, "abort this input", key, "test", "chat")
				done <- turnErr
			}()
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				t.Fatal("provider did not start after Seahorse setup")
			}
			if queued {
				loop.MirrorDelivery(
					tools.ExternalDelivery{
						Origin:  "cron:queued",
						Channel: "test",
						ChatID:  "chat",
						Text:    "queued reminder",
					},
				)
			}
			if abortErr := loop.HardAbort(key); abortErr != nil {
				t.Fatal(abortErr)
			}
			select {
			case turnErr := <-done:
				if turnErr != nil {
					t.Fatal(turnErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("hard abort did not finish")
			}
			after := agent.Sessions.GetHistory(key)
			want := len(before)
			if queued {
				want++
			}
			if len(after) != want {
				t.Fatal("abort lost retained or queued deliveries", len(after), want)
			}
			if !reflect.DeepEqual(after[:len(before)], before) {
				t.Errorf(
					"Seahorse rollback replaced canonical deliveries, origin/time or occurrence IDs: before=%+v after=%+v",
					before,
					after[:len(before)],
				)
			}
			if actual := agent.Sessions.GetSummary(key); actual != summary {
				t.Errorf("rollback replaced canonical summary with assembled model context: %q", actual)
			}
			if queued {
				last := after[len(after)-1]
				if last.Content != "queued reminder" || last.Delivery == nil || last.Delivery.Origin != "cron:queued" ||
					uuid.Validate(last.Delivery.ID) != nil ||
					last.Delivery.ID == receipts[0].Delivery.ID ||
					last.Delivery.ID == receipts[1].Delivery.ID {
					t.Fatal("abort lost or reassigned concurrent delivery", last)
				}
			}
			confirmed, readErr := writer.ReadHistoryWithReceipts(t.Context(), key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for i := range receipts {
				if confirmed[i].Message.HistoryID != receipts[i].HistoryID ||
					confirmed[i].Message.HistorySequence != receipts[i].HistorySequence {
					t.Fatal("rollback invented new order evidence for retained delivery", confirmed[i], receipts[i])
				}
			}
		})
	}
}

type restoreFrameBackend struct {
	*session.JSONLBackend
	frameError error
}

func (b *restoreFrameBackend) ReadHistoryContentFrame(ctx context.Context, key string) (memory.ContentFrame, error) {
	if b.frameError != nil {
		return memory.ContentFrame{}, b.frameError
	}
	return b.JSONLBackend.ReadHistoryContentFrame(ctx, key)
}

func TestRestorePointFrameFailurePreservesDurableHistory(t *testing.T) {
	for _, priorCapture := range []bool{false, true} {
		name := "initial capture unavailable"
		if priorCapture {
			name = "refresh preserves last reliable capture"
		}
		t.Run(name, func(t *testing.T) {
			loop, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			key := "sk_v1_capture_failure"
			store, err := memory.NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			backend := &restoreFrameBackend{JSONLBackend: session.NewJSONLBackend(store)}
			agent.Sessions = backend
			backend.AddFullMessage(key, providers.Message{
				Role: "assistant", Content: "durable delivery",
				Delivery: &providers.Delivery{Origin: "cron:restore", At: time.Now()},
			})
			backend.SetSummary(key, "durable summary")
			before := backend.GetHistory(key)
			if !priorCapture {
				backend.frameError = errors.New("capture unavailable")
			}
			opts := makeTestProcessOpts(key)
			opts.Dispatch = DispatchRequest{SessionKey: key}
			ts := newTurnState(agent, opts, turnEventScope{turnID: "test-restore"})
			backend.frameError = errors.New("capture unavailable")
			ts.refreshRestorePointFromSession(agent)
			if (ts.restorePointHistory != nil) != priorCapture {
				t.Fatal("failed capture replaced reliable state or invented empty restore point")
			}
			loop.activeTurnStates.Store(key, ts)
			defer loop.activeTurnStates.Delete(key)
			loop.rollBackAbortedTurn(key, ts)
			if err := ts.restoreSession(agent); err != nil {
				t.Fatal(err)
			}
			if after := backend.GetHistory(key); !reflect.DeepEqual(after, before) {
				t.Fatal("failed frame capture erased or reassigned durable history", before, after)
			}
			if actual := backend.GetSummary(key); actual != "durable summary" {
				t.Fatal("failed capture erased durable summary", actual)
			}
		})
	}
}

func TestRestorePointValidEmptyHistoryStillRollsBack(t *testing.T) {
	loop, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	key := "sk_v1_empty_restore"
	opts := makeTestProcessOpts(key)
	opts.Dispatch = DispatchRequest{SessionKey: key}
	ts := newTurnState(agent, opts, turnEventScope{turnID: "test-empty-restore"})
	if ts.restorePointHistory == nil {
		t.Fatal("valid empty history was treated as unavailable")
	}
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "user", Content: "aborted input"})
	loop.activeTurnStates.Store(key, ts)
	defer loop.activeTurnStates.Delete(key)
	loop.rollBackAbortedTurn(key, ts)
	if err := ts.restoreSession(agent); err != nil {
		t.Fatal(err)
	}
	if after := agent.Sessions.GetHistory(key); len(after) != 0 {
		t.Fatal("valid empty restore point retained aborted input", after)
	}
}

func TestRestorePointLegacyBackend(t *testing.T) {
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	key := "legacy-restore"
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "assistant", Content: "legacy history"})
	agent.Sessions.SetSummary(key, "legacy summary")
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unsupported := &restoreFrameBackend{
		JSONLBackend: session.NewJSONLBackend(store),
		frameError:   memory.ErrHistoryUnsupported,
	}
	unsupported.AddFullMessage(key, providers.Message{Role: "assistant", Content: "legacy history"})
	unsupported.SetSummary(key, "legacy summary")
	for _, backend := range []session.SessionStore{
		&legacyOnlyContentSessions{SessionStore: agent.Sessions},
		unsupported,
	} {
		history, summary, readErr := readRestorePoint(backend, key)
		if readErr != nil || summary != "legacy summary" || len(history) != 1 ||
			history[0].Content != "legacy history" {
			t.Fatal("legacy restore fallback changed", history, summary, readErr)
		}
	}
}

func TestRestorePointCaptureRecoveryUpdatesAbortBoundary(t *testing.T) {
	loop, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	key := "sk_v1_restore_capture_recovers"
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &restoreFrameBackend{
		JSONLBackend: session.NewJSONLBackend(store),
		frameError:   errors.New("temporarily unavailable"),
	}
	agent.Sessions = backend
	backend.AddFullMessage(key, providers.Message{Role: "assistant", Content: "durable before capture"})
	before, err := backend.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || len(before) != 1 {
		t.Fatal("fixture lacks durable order proof", before, err)
	}
	opts := makeTestProcessOpts(key)
	opts.Dispatch = DispatchRequest{SessionKey: key}
	ts := newTurnState(agent, opts, turnEventScope{turnID: "test-capture-recovery"})
	backend.frameError = nil
	ts.refreshRestorePointFromSession(agent)
	backend.AddFullMessage(key, providers.Message{Role: "user", Content: "aborted input"})
	loop.activeTurnStates.Store(key, ts)
	defer loop.activeTurnStates.Delete(key)
	loop.rollBackAbortedTurn(key, ts)
	if restoreErr := ts.restoreSession(agent); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	after, err := backend.ReadHistoryWithReceipts(t.Context(), key)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("recovered capture used stale abort boundary or lost retained proof", before, after, err)
	}
}
