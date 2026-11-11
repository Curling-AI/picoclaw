package agent

import (
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// isCronRunMainTurn reports whether this turn runs in an automation run's
// session and is not a turn it spawned: those are keyed "subturn-N", and
// isCronRun counts them too. It is true for the run itself and for every other
// turn in the run's session, such as the one a late result of the run opens
// there after the run ended (processSystemMessage), whose empty answer
// therefore no longer becomes "Background task completed.". The answer of a
// turn in the run's session is delivered to the owner; a child's goes back to
// the run as a tool result. (seucaranguejo fork — MST-795)
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

// deliveredAnswer is what a turn's answer delivers to its chat. In an
// automation run's session, where a late result of the run opens a turn after
// the run ended, it is what the run's own answer would deliver
// (tools.AutomationDelivery): that session's history holds the note asking for
// NO_REPLY. In any other session it is the answer as it is. (seucaranguejo
// fork — MST-795)
func deliveredAnswer(sessionKey, channel, answer string) string {
	if !memory.IsCronRunSessionKey(sessionKey) {
		return answer
	}
	delivery, mixed := tools.AutomationDelivery(answer)
	switch {
	case delivery == "" && answer != "":
		logger.InfoCF("agent", "Automation run had nothing to deliver",
			map[string]any{"session_key": sessionKey, "channel": channel})
	case mixed:
		logger.WarnCF("agent", "Automation answer mixed NO_REPLY with text; delivering the text", map[string]any{
			"session_key":     sessionKey,
			"channel":         channel,
			"answer_chars":    len(answer),
			"delivered_chars": len(delivery),
		})
	}
	return delivery
}
