package agent

import (
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func (ts *turnState) backgroundProcessOwner() string {
	return ts.sessionKey + ":" + ts.turnID
}

func (ts *turnState) finishBackgroundProcesses(succeeded bool) {
	ts.backgroundMu.Lock()
	defer ts.backgroundMu.Unlock()
	if !succeeded {
		ts.backgroundFailed = true
		ts.stopBackgroundProcesses(ts.backgroundProcessOwner())
		return
	}
	if ts.parentTurnState != nil {
		ts.backgroundHandedOver = true
		ts.parentTurnState.acceptBackgroundProcesses(ts.backgroundProcessOwner())
	}
}

func (ts *turnState) acceptBackgroundProcesses(from string) {
	ts.backgroundMu.Lock()
	defer ts.backgroundMu.Unlock()
	// Serialize handover with failure cleanup, including children finishing
	// after their parent. Otherwise a late handover can recreate an orphan.
	if ts.backgroundFailed || ts.hardAbortRequested() {
		ts.stopBackgroundProcesses(from)
		return
	}
	if ts.backgroundHandedOver {
		ts.parentTurnState.acceptBackgroundProcesses(from)
		return
	}
	tools.HandOverBackgroundSessions(from, ts.backgroundProcessOwner())
}

func (ts *turnState) stopBackgroundProcesses(owner string) {
	if killed := tools.KillBackgroundSessions(owner); len(killed) > 0 {
		logger.WarnCF("agent", "Stopped background processes of a failed turn", map[string]any{
			"turn_id":  ts.turnID,
			"sessions": killed,
		})
	}
}
