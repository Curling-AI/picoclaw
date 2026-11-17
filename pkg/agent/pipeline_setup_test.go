package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// stuckCompaction is a context manager whose compaction never goes through,
// as for a session still waiting on the confirmation of an earlier one.
type stuckCompaction struct {
	ContextManager
	attempts int
}

func (m *stuckCompaction) Compact(context.Context, *CompactRequest) error {
	m.attempts++
	return errors.New("compaction awaits archive confirmation")
}

// overflowingSession stores a history a quarter past the agent's window and
// returns it with the turn about to run on it, carrying media if given.
func overflowingSession(t *testing.T, media ...string) (*Pipeline, *stuckCompaction, *turnState, []providers.Message) {
	t.Helper()
	al, agent, cleanup := newTurnCoordTestLoop(t, &mockProvider{})
	t.Cleanup(cleanup)

	opts := makeTestProcessOptsWithHistory("session-past-the-window")
	opts.Dispatch.Media = media
	history := turnsOf(2000)
	agent.Sessions.SetHistory(opts.Dispatch.SessionKey, history)

	withoutHistory := agent.ContextBuilder.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: opts.UserMessage})
	fixed := estimateCallTokens(withoutHistory, agent.Tools.ToProviderDefs(), agent.MaxTokens)
	agent.ContextWindow = fixed + historyTokens(history)*3/4

	pipeline := NewPipeline(al)
	stuck := &stuckCompaction{ContextManager: pipeline.ContextManager}
	pipeline.ContextManager = stuck
	ts := newTurnState(agent, opts, turnEventScope{turnID: "turn-1", context: newTurnContext(nil, nil, nil)})
	return pipeline, stuck, ts, history
}

func TestSetupTurn_SessionThatCannotCompactRunsAtTheSummarizationPercent(t *testing.T) {
	for name, percent := range map[string]int{"default percent": 0, "50 percent": 50} {
		t.Run(name, func(t *testing.T) {
			pipeline, stuck, ts, history := overflowingSession(t)
			if percent > 0 {
				ts.agent.SummarizeTokenPercent = percent
			}

			exec, err := pipeline.SetupTurn(context.Background(), ts)
			if err != nil {
				t.Fatalf("SetupTurn failed: %v", err)
			}

			if stuck.attempts != 1 {
				t.Fatalf("compaction attempts = %d, want 1", stuck.attempts)
			}
			tools := ts.agent.Tools.ToProviderDefs()
			kept := historyTokens(exec.history)
			// The room for history is the window less what the call takes without any.
			room := ts.agent.ContextWindow - (estimateCallTokens(exec.messages, tools, ts.agent.MaxTokens) - kept)
			limit := room * ts.agent.SummarizeTokenPercent / 100
			if perTurn := historyTokens(history) / 2000; kept > limit || kept <= limit-perTurn {
				t.Fatalf("history kept for the call = %d tokens, want the most that fits %d%% of the room for it (%d)",
					kept, ts.agent.SummarizeTokenPercent, limit)
			}
			if stored := ts.agent.Sessions.GetHistory(ts.sessionKey); len(stored) != len(history)+1 {
				t.Fatalf("stored history has %d messages, want all %d and the new prompt: the cut is for the call only",
					len(stored), len(history))
			}
		})
	}
}

func TestSetupTurn_CutCallStillStartsTheTurnAtItsOwnMessage(t *testing.T) {
	image := "data:image/png;base64,abc123"
	pipeline, _, ts, history := overflowingSession(t, image)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	if len(exec.history) == 0 || len(exec.history) >= len(history) {
		t.Fatalf("history for the call = %d messages, want a cut of the %d stored", len(exec.history), len(history))
	}
	turn := currentTurnMessages(exec.messages, exec.currentTurnStart)
	if len(turn) != 1 || len(turn[0].Media) != 1 || turn[0].Media[0] != image {
		t.Fatalf("current turn of the cut call = %d messages, want the one message with the turn's image", len(turn))
	}
}

func TestSetupTurn_StoppedTurnIsNotFittedToTheWindow(t *testing.T) {
	pipeline, stuck, ts, history := overflowingSession(t)
	ctx, stop := context.WithCancel(context.Background())
	stop()

	exec, err := pipeline.SetupTurn(ctx, ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	if stuck.attempts != 0 || len(exec.history) != len(history) {
		t.Fatalf("compaction attempts = %d and history for the call = %d messages, want none and all %d",
			stuck.attempts, len(exec.history), len(history))
	}
	stored := ts.agent.Sessions.GetHistory(ts.sessionKey)
	if len(stored) != len(history)+1 || stored[len(stored)-1].Content != ts.userMessage {
		t.Fatalf("stored history has %d messages, want the %d before and the prompt of the stopped turn",
			len(stored), len(history))
	}
}
