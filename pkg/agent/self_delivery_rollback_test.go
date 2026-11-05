//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

type rollbackOutcomeBackend struct {
	*restoreFrameBackend
	replaceError       error
	saveError          error
	appliedBeforeError bool
}

func (b *rollbackOutcomeBackend) SetHistoryContext(ctx context.Context, key string, history []providers.Message) error {
	if b.replaceError != nil && !b.appliedBeforeError {
		return b.replaceError
	}
	if err := b.JSONLBackend.SetHistoryContext(ctx, key, history); err != nil {
		return err
	}
	return b.replaceError
}

func (b *rollbackOutcomeBackend) Save(key string) error {
	if b.saveError != nil {
		return b.saveError
	}
	return b.JSONLBackend.Save(key)
}

func TestSelfDeliveryAbortAccountsForActualHistoryReplacement(t *testing.T) {
	for _, hardAbort := range []bool{false, true} {
		for _, outcome := range []string{"no snapshot", "partial record before capture", "replacement failed", "replacement applied with error", "save failed after replacement", "restored"} {
			name := outcome + "/abort"
			if hardAbort {
				name = outcome + "/hard abort"
			}
			t.Run(name, func(t *testing.T) {
				loop, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
				defer cleanup()
				key := "sk_v1_self_delivery_rollback"
				dir := t.TempDir()
				store, err := memory.NewJSONLStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend := &rollbackOutcomeBackend{
					restoreFrameBackend: &restoreFrameBackend{JSONLBackend: session.NewJSONLBackend(store)},
				}
				agent.Sessions = backend
				backend.AddMessage(key, "user", "before this turn")
				backend.SetSummary(key, "canonical summary before turn")
				if outcome == "no snapshot" {
					backend.frameError = errors.New("frame capture unavailable")
				}
				if outcome == "partial record before capture" {
					path := filepath.Join(dir, key+".jsonl")
					raw, readErr := os.ReadFile(path)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if writeErr := os.WriteFile(
						path,
						append(raw, []byte(`{"role":"user","content":"partial`)...),
						0o600,
					); writeErr != nil {
						t.Fatal(writeErr)
					}
				}
				opts := makeTestProcessOpts(key)
				opts.Dispatch = DispatchRequest{SessionKey: key}
				ts := newTurnState(agent, opts, turnEventScope{turnID: "self-delivery-turn"})
				if outcome == "partial record before capture" && ts.restorePointHistory != nil {
					t.Fatal("partial record did not exercise unavailable canonical capture")
				}
				loop.registerActiveTurn(ts)
				msg := providers.Message{
					Role:     "assistant",
					Content:  "one durable self delivery",
					Delivery: &providers.Delivery{Origin: key, At: time.Now(), Self: true},
				}
				if !loop.holdForOwnTurn(key, ts.turnID, msg) {
					t.Fatal("fixture did not hold self delivery")
				}
				loop.writeHeldSelfDeliveries(ts, &turnExecution{})
				before := backend.GetHistory(key)
				if len(before) != 2 || before[1].Delivery == nil || uuid.Validate(before[1].Delivery.ID) != nil {
					t.Fatal("fixture did not commit delivery identity", before)
				}
				original := before[1].Delivery.ID
				backend.SetSummary(key, "summary during turn")
				switch outcome {
				case "replacement failed":
					backend.replaceError = errors.New("replacement failed before write")
				case "replacement applied with error":
					backend.appliedBeforeError = true
					backend.replaceError = errors.Join(memory.ErrHistoryInputCommitted, syscall.EIO)
				case "save failed after replacement":
					backend.saveError = errors.New("save failed after replacement")
				}
				if hardAbort {
					if abortErr := loop.HardAbort(key); abortErr != nil {
						t.Fatal(abortErr)
					}
				}
				_, abortErr := loop.abortTurn(ts)
				wantError := outcome == "replacement failed" || outcome == "replacement applied with error" ||
					outcome == "save failed after replacement"
				if (abortErr != nil) != wantError {
					t.Errorf("abort result=%v, want error=%v", abortErr, wantError)
				}
				if outcome == "replacement applied with error" && !errors.Is(abortErr, syscall.EIO) {
					t.Error("applied rewrite lost original operational error", abortErr)
				}
				backend.replaceError = nil
				backend.saveError = nil
				loop.clearActiveTurn(ts)
				loop.flushMirroredDeliveries(key)
				after := backend.GetHistory(key)
				wantSummary := "canonical summary before turn"
				if outcome == "no snapshot" || outcome == "partial record before capture" ||
					outcome == "replacement failed" {
					wantSummary = "summary during turn"
				}
				if actual := backend.GetSummary(key); actual != wantSummary {
					t.Error("rollback summary did not follow applied history", actual, wantSummary)
				}
				if len(after) != 2 || after[1].Content != msg.Content || after[1].Delivery == nil ||
					uuid.Validate(after[1].Delivery.ID) != nil {
					t.Fatalf("abort/close/flush lost or duplicated a delivery: %+v", after)
				}
				if (outcome == "no snapshot" || outcome == "partial record before capture" || outcome == "replacement failed") &&
					after[1].Delivery.ID != original {
					t.Fatal(
						"an occurrence still durable before abort was assigned another ID",
						original,
						after[1].Delivery.ID,
					)
				}
			})
		}
	}
}
