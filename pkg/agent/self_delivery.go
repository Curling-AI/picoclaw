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

// writeSelfDeliveries writes what the turn's scripts sent since the last call,
// after the tool results already in the history, and joins it to the last of
// those results in exec's messages: the turn's next call to the model is built
// from them, not from the history. The sends stay held while the turn's last
// message is not a tool result (a sub-turn's result can follow one): written
// there they could not join a result, and would break the next round's call.
// Not once an abort was requested either: the cut would take them. What is
// still held when the turn ends is queued.
func (al *AgentLoop) writeSelfDeliveries(ts *turnState, exec *turnExecution) {
	if ts.hardAbortRequested() {
		return
	}
	if n := len(exec.messages); n == 0 || exec.messages[n-1].Role != "tool" {
		return
	}
	stripe := al.mirror.stripe(ts.sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	for _, msg := range ts.selfDeliveries[ts.selfWritten:] {
		ts.agent.Sessions.AddFullMessage(ts.sessionKey, msg)
		ts.recordPersistedMessage(msg)
		exec.messages = joinToLastToolResult(exec.messages, msg)
	}
	ts.selfWritten = len(ts.selfDeliveries)
}

// foldSelfDeliveries is messages as a call carries them: each send of the
// turn's own script joined to the tool result before it, as
// sanitizeHistoryForProvider does. Offsets into the active turn of a call
// (its start, its elisions) count these messages, not the stored ones.
func foldSelfDeliveries(messages []providers.Message) []providers.Message {
	var out []providers.Message // nil until the first join: messages is returned as is
	for i, msg := range messages {
		done := messages[:i]
		if out != nil {
			done = out
		}
		if !joinsLastToolResult(done, msg) {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		out = joinToLastToolResult(done, msg)
	}
	if out == nil {
		return messages
	}
	return out
}

// joinsLastToolResult reports whether msg, a send of the turn's own script,
// joins the tool result that ends messages.
func joinsLastToolResult(messages []providers.Message, msg providers.Message) bool {
	n := len(messages)
	return msg.Delivery != nil && msg.Delivery.Self && n > 0 && messages[n-1].Role == "tool"
}

// joinedSelfDeliveries is what joinToLastToolResult added to a tool result's
// content: the sends of the turn's own script, which an elision of the result
// keeps (the user saw them; only the output may go).
func joinedSelfDeliveries(content string) string {
	if strings.HasPrefix(content, selfDeliveryLine) {
		return "\n\n" + content
	}
	if i := strings.Index(content, "\n\n"+selfDeliveryLine); i >= 0 {
		return content[i:]
	}
	return ""
}

// joinToLastToolResult adds msg, with its dated line, to the last message when
// it is a tool result, as sanitizeHistoryForProvider does with the history. It
// returns a new slice: the old one may be shared.
func joinToLastToolResult(messages []providers.Message, msg providers.Message) []providers.Message {
	n := len(messages)
	if n == 0 || messages[n-1].Role != "tool" {
		return messages
	}
	last := messages[n-1]
	last.Content = joinAssistantText(last.Content, historyContent(msg))
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
