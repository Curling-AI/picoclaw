package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

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

// writeSelfDeliveries writes, between two rounds of tools, what the turn's
// scripts sent since the last call: after the round's results in the history,
// and joined to the last of them in exec's messages, since the turn's next
// call to the model is built from those, not from the history. The sends stay
// held while the round does not end on a tool result (a sub-turn's result can
// follow one), where they would join a message of the user's instead of the
// round's.
func (al *AgentLoop) writeSelfDeliveries(ts *turnState, exec *turnExecution) {
	if n := len(exec.messages); n == 0 || exec.messages[n-1].Role != "tool" {
		return
	}
	al.writeHeldSelfDeliveries(ts, exec)
}

// writeSelfDeliveriesBeforeReply writes what the turn still holds right before
// its reply, whatever the turn's last message is: after the reply would be the
// reverse of what the user saw. The history joins each to the message before
// it, a tool result or the user's (steering, a sub-turn's result), never to the
// reply.
func (al *AgentLoop) writeSelfDeliveriesBeforeReply(ts *turnState, exec *turnExecution) {
	al.writeHeldSelfDeliveries(ts, exec)
}

// writeHeldSelfDeliveries writes the held sends. Not once an abort was
// requested: the cut would take them; the turn's end queues them instead.
func (al *AgentLoop) writeHeldSelfDeliveries(ts *turnState, exec *turnExecution) {
	if ts.hardAbortRequested() {
		return
	}
	stripe := al.mirror.stripe(ts.sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	for _, msg := range ts.selfDeliveries[ts.selfWritten:] {
		ts.appendHistoryMessage(msg)
		ts.recordPersistedMessage(msg)
		if joinsLastMessage(exec.messages, msg) {
			exec.messages = joinToLastMessage(exec.messages, msg)
		}
	}
	ts.selfWritten = len(ts.selfDeliveries)
}

// foldSelfDeliveries is messages as a call carries them: each send of the
// turn's own script joined to the message before it, as
// sanitizeHistoryForProvider does. Offsets into the active turn of a call
// (its start, its elisions) count these messages, not the stored ones.
func foldSelfDeliveries(messages []providers.Message) []providers.Message {
	var out []providers.Message // nil until the first join: messages is returned as is
	for i, msg := range messages {
		done := messages[:i]
		if out != nil {
			done = out
		}
		if !joinsLastMessage(done, msg) {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		out = joinToLastMessage(done, msg)
	}
	if out == nil {
		return messages
	}
	return out
}

// joinsLastMessage reports whether msg, a send of the turn's own script, joins
// the message that ends messages: a tool result, or a message of the user's.
// Alone it would break the next round's tool call (whose predecessor must be
// a tool result or the user), or swallow the turn's reply under the script's
// dated line.
func joinsLastMessage(messages []providers.Message, msg providers.Message) bool {
	n := len(messages)
	if msg.Delivery == nil || !msg.Delivery.Self || n == 0 {
		return false
	}
	role := messages[n-1].Role
	return role == "tool" || role == "user"
}

// joinToLastMessage adds msg, with its dated line, to the last message, and
// records what it added in JoinedSends: an elision of a tool result keeps the
// sends (the user saw them; only the output may go), and the output is no
// one's to trust, so they are never looked for in it. It returns a new slice:
// the old one may be shared.
func joinToLastMessage(messages []providers.Message, msg providers.Message) []providers.Message {
	n := len(messages)
	last := messages[n-1]
	before := strings.TrimSpace(last.Content)
	last.Content = joinAssistantText(before, historyContent(msg))
	last.JoinedSends += last.Content[len(before):]
	return append(messages[:n-1:n-1], last)
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
