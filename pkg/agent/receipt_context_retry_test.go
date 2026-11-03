package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestMatchingTurnMessageTailIgnoresInputReceiptIdentity(t *testing.T) {
	prompt := providers.Message{
		Role: "user", Content: "question", HistoryID: uuid.NewString(), HistoryRunID: uuid.NewString(),
		HistoryOrderVersion: 1, HistoryStreamID: uuid.NewString(), HistorySequence: 1,
	}
	history := []providers.Message{{Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}}
	persisted := []providers.Message{prompt, history[1]}
	if matched := matchingTurnMessageTail(history, persisted); matched != 2 {
		t.Fatalf("matched %d active messages, want 2 despite their persistence receipt", matched)
	}
	if persisted[0].HistoryID != prompt.HistoryID || persisted[0].HistorySequence != 1 {
		t.Fatal("comparison modified the retained receipt")
	}
}

func TestContextRetryWithPromptReceiptKeepsActiveTurn(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		name := "active turn only"
		if earlier {
			name = "with earlier history"
		}
		t.Run(name, func(t *testing.T) {
			cfg := windowedConfig(t)
			cfg.Tools.MaxResultChars = 10_000_000
			turn := newWindowedTurn(t, cfg, &bigOutputTool{size: 200_000}, 20_000)
			const session = "sk_v1_receipt_context_guard"
			if earlier {
				turn.run(t, session, "earlier question")
			}
			turn.al.SetSessionHistoryReceiptObserver(func(string, []memory.CommittedHistoryMessage) {})
			turn.provider.toolRounds = 1
			ctx := providers.WithHistoryRun(context.Background(), uuid.NewString())
			response, err := turn.al.ProcessDirectWithChannel(ctx, "take a snapshot", session, "test", session)
			if err != nil || response != "done" {
				t.Fatalf("context retry lost the active turn: response=%q, err=%v", response, err)
			}
			if calls := turn.tool.calls.Load(); calls != 1 {
				t.Fatalf("tool ran %d times, want one retained result", calls)
			}
			history := turn.agent.Sessions.GetHistory(session)
			if results := toolMessages(history); len(results) != 1 || results[0].Content != bigOutput(200_000, 1) {
				t.Fatal("context retry lost or changed the active tool result")
			}
			if earlier && (len(history) == 0 || history[0].Content != "earlier question") {
				t.Fatal("context retry dropped earlier conversation history")
			}
		})
	}
}
