package agent

import (
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/memory"
)

// isCronRunMainTurn reports whether this turn is an automation run itself, not
// a turn it spawned: those are keyed "subturn-N", and isCronRun counts them
// too. Only the run's own final answer is delivered to the owner; a child's
// goes back to the run as a tool result. (seucaranguejo fork — MST-795)
func (ts *turnState) isCronRunMainTurn() bool {
	return ts != nil && memory.IsCronRunSessionKey(ts.sessionKey)
}

// logEmptyAutomationAnswer records an automation run that ended without an
// answer: nothing is delivered and the run fails. (seucaranguejo fork — MST-795)
func logEmptyAutomationAnswer(ts *turnState) {
	logger.WarnCF("agent", "Automation run ended without an answer; nothing delivered", map[string]any{
		"agent_id":    ts.agent.ID,
		"session_key": ts.sessionKey,
		"iteration":   ts.currentIteration(),
	})
}
