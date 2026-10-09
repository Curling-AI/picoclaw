package agent

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// The result of async work (a spawn) arrives as a system message naming the
// session of the conversation that launched it. It belongs to that
// conversation, not to the main session: a turn of main does not know what the
// conversation was doing and, running beside the conversation's own turns, can
// act on the same resources unseen.

// backgroundResultTarget is the conversation a system message's result goes
// to, with the agent that owns it. ok is false for a message that names no
// session, comes from an internal chat, or names a conversation that no longer
// exists (cleared or deleted while the work ran): those keep the main session.
func (al *AgentLoop) backgroundResultTarget(msg bus.InboundMessage) (string, *AgentInstance, bool) {
	if msg.Channel != "system" || !isExplicitSessionKey(msg.SessionKey) {
		return "", nil, false
	}
	if originChannel, _ := parseSystemOrigin(msg.ChatID); constants.IsInternalChannel(originChannel) {
		return "", nil, false
	}
	sessionKey := msg.SessionKey
	agent := al.agentForSession(sessionKey)
	if agent == nil || !hasConversation(agent.Sessions, sessionKey) {
		return "", nil, false
	}
	return sessionKey, agent, true
}

// recordBackgroundResult writes a result into its conversation, without a
// turn, when the conversation's chat cannot receive a late reply (a web run's
// stream ends with the run): a turn there would act unseen and race the user's
// next message on the same history. If the conversation is in a turn, the note
// waits for it to end, the way the delivery mirror defers its writes. The next
// turn of the conversation reads it.
//
// false = not handled: the chat receives replies, so the caller runs a turn in
// the conversation, or queues the result into the live one, whose reply can
// mention it.
func (al *AgentLoop) recordBackgroundResult(msg bus.InboundMessage) bool {
	sessionKey, agent, ok := al.backgroundResultTarget(msg)
	if !ok || al.originReachable(msg.ChatID) {
		return false
	}
	note := providers.Message{Role: "user", Content: systemMessageContent(msg)}

	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	busy := al.mirrorBusyLocked(sessionKey)
	var batch []providers.Message
	if busy {
		al.queueMirroredLocked(sessionKey, note)
	} else {
		batch = append(al.mirror.pending[sessionKey], note)
		delete(al.mirror.pending, sessionKey)
	}
	al.mirror.mu.Unlock()
	if !busy {
		al.writeMirrored(agent, sessionKey, batch)
	}
	logger.InfoCF("agent", "Recorded background result in its conversation", map[string]any{
		"sender_id":   msg.SenderID,
		"chat_id":     msg.ChatID,
		"session_key": sessionKey,
		"deferred":    busy,
		"content_len": len(note.Content),
	})
	return true
}

// originReachable reports whether a late reply can reach the chat the work was
// launched from. With a delivery resolver, a chat it maps to no conversation is
// not one (a web run id). Without a resolver every chat counts as reachable.
func (al *AgentLoop) originReachable(chatID string) bool {
	al.mirror.mu.Lock()
	resolve := al.mirror.resolve
	al.mirror.mu.Unlock()
	if resolve == nil {
		return true
	}
	channel, originChatID := parseSystemOrigin(chatID)
	return resolve(channel, originChatID) != ""
}
