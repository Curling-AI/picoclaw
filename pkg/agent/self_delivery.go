package agent

import "github.com/sipeed/picoclaw/pkg/providers"

// What a turn's own scripts send to its chat while the turn runs.
// (seucaranguejo fork — see delivery_mirror.go)
//
// The tool result carries such a send only when a synchronous exec echoed it,
// so the mirror records it in the chat's conversation too. Written the moment
// it arrives, it could land between a tool call and its result (the send
// happens while exec runs); queued like any other delivery, it would land after
// the turn's final reply, the reverse of what the user saw. So the turn holds
// it and writes it after the tool round it came from, before what it says next.
// What the turn did not write, or wrote and then lost to an abort's cut, joins
// the session's queue when the turn ends.
//
// The turn's selfDeliveries, selfWritten and selfClosed are guarded by the
// session's mirror stripe.

// holdForOwnTurn hands msg to turn turnID when it is the turn live in the
// session; false otherwise.
func (al *AgentLoop) holdForOwnTurn(sessionKey, turnID string, msg providers.Message) bool {
	if turnID == "" {
		return false
	}
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	ts := al.getActiveTurnState(sessionKey)
	if ts == nil || ts.turnID != turnID || ts.opts.NoHistory || ts.selfClosed {
		return false
	}
	ts.selfDeliveries = append(ts.selfDeliveries, msg)
	return true
}

// writeSelfDeliveries writes what the turn's scripts sent since the last call,
// after the tool results already in the history. Not once an abort was
// requested: the cut would take them; the turn's end queues them instead.
func (al *AgentLoop) writeSelfDeliveries(ts *turnState) {
	if ts.hardAbortRequested() {
		return
	}
	stripe := al.mirror.stripe(ts.sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	for _, msg := range ts.selfDeliveries[ts.selfWritten:] {
		ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
		ts.recordPersistedMessage(msg)
	}
	ts.selfWritten = len(ts.selfDeliveries)
}

// forgetWrittenSelfDeliveries: the session's history was cut back to before
// the turn, taking what writeSelfDeliveries wrote.
func (al *AgentLoop) forgetWrittenSelfDeliveries(ts *turnState) {
	stripe := al.mirror.stripe(ts.sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	ts.selfWritten = 0
}

// closeSelfDeliveries ends the hold as the turn ends: what is not in the
// history joins the session's queue, written once no turn holds the session.
func (al *AgentLoop) closeSelfDeliveries(ts *turnState) {
	stripe := al.mirror.stripe(ts.sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	ts.selfClosed = true
	left := ts.selfDeliveries[ts.selfWritten:]
	if len(left) == 0 {
		return
	}
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	for _, msg := range left {
		al.queueMirroredLocked(ts.sessionKey, msg)
	}
}
