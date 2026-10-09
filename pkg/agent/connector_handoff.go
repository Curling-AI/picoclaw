package agent

import (
	"errors"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A failed prerequisite check stops the batch but must remain retryable, rather
// than completing the connector resolution as though its requested action ran.
func finishFailedToolHandoff(
	ts *turnState,
	exec *turnExecution,
	messages []providers.Message,
	remaining []providers.ToolCall,
	forUser string,
) ToolControl {
	exec.handoffError = errors.New("tool stopped the turn after a failed prerequisite check")
	return persistToolHandoff(ts, exec, messages, remaining, forUser)
}

// finishToolHandoff persists a balanced tool batch before ending the turn.
// Steering stays queued for an explicit new turn; it cannot cross the handoff.
func finishToolHandoff(
	ts *turnState,
	exec *turnExecution,
	messages []providers.Message,
	remaining []providers.ToolCall,
) ToolControl {
	return persistToolHandoff(ts, exec, messages, remaining, handledToolResponseSummary)
}

func persistToolHandoff(
	ts *turnState,
	exec *turnExecution,
	messages []providers.Message,
	remaining []providers.ToolCall,
	summary string,
) ToolControl {
	for _, call := range remaining {
		msg := providers.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Content:    "Skipped: waiting for the user's connector resolution.",
		}
		messages = append(messages, msg)
		if !ts.opts.NoHistory {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
			ts.recordPersistedMessage(msg)
		}
	}
	if !ts.opts.NoHistory {
		if summary != "" {
			msg := providers.Message{Role: "assistant", Content: summary, ModelName: exec.llmModelName}
			messages = append(messages, msg)
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
			ts.recordPersistedMessage(msg)
		}
		if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
			exec.handoffError = errors.Join(exec.handoffError, fmt.Errorf("save tool handoff: %w", err))
		}
	}
	exec.messages = messages
	exec.allResponsesHandled = true
	exec.finalContent = ""
	if exec.handoffError == nil {
		ts.setPhase(TurnPhaseCompleted)
	}
	return ToolControlBreak
}
