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
// returns it with the turn about to run on it.
func overflowingSession(t *testing.T) (*Pipeline, *stuckCompaction, *turnState, []providers.Message) {
	t.Helper()
	al, agent, cleanup := newTurnCoordTestLoop(t, &mockProvider{})
	t.Cleanup(cleanup)

	opts := makeTestProcessOptsWithHistory("session-past-the-window")
	history := turnsOf(2000)
	agent.Sessions.SetHistory(opts.Dispatch.SessionKey, history)

	withoutHistory := agent.ContextBuilder.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: opts.UserMessage})
	fixed := estimateCallTokens(withoutHistory, agent.Tools.ToProviderDefs(), agent.MaxTokens)
	agent.ContextWindow = fixed + historyTokens(history)*3/4
	agent.SummarizeTokenPercent = 50

	pipeline := NewPipeline(al)
	stuck := &stuckCompaction{ContextManager: pipeline.ContextManager}
	pipeline.ContextManager = stuck
	ts := newTurnState(agent, opts, turnEventScope{turnID: "turn-1", context: newTurnContext(nil, nil, nil)})
	return pipeline, stuck, ts, history
}

func TestSetupTurn_SessionThatCannotCompactRunsAtTheSummarizationThreshold(t *testing.T) {
	pipeline, stuck, ts, history := overflowingSession(t)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	if stuck.attempts != 1 {
		t.Fatalf("compaction attempts = %d, want 1", stuck.attempts)
	}
	threshold := ts.agent.ContextWindow * ts.agent.SummarizeTokenPercent / 100
	perTurn := historyTokens(history) / 2000
	if kept := historyTokens(exec.history); kept > threshold || kept <= threshold-perTurn {
		t.Fatalf("history kept for the call = %d tokens, want the most that fits the threshold of %d", kept, threshold)
	}
	if isOverContextBudget(ts.agent.ContextWindow, exec.messages, ts.agent.Tools.ToProviderDefs(), ts.agent.MaxTokens) {
		t.Fatal("the call is still past the context window")
	}
	if stored := ts.agent.Sessions.GetHistory(ts.sessionKey); len(stored) != len(history)+1 {
		t.Fatalf("stored history has %d messages, want all %d and the new prompt: the cut is for the call only",
			len(stored), len(history))
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
