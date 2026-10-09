package agent

import (
	"context"
	"errors"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// The result of async work (a spawn) arrives as a system message naming the
// session of the conversation that launched it. It belongs to that
// conversation, not to the main session: a turn of main does not know what the
// conversation was doing and, running beside the conversation's own turns, can
// act on the same resources unseen.
//
// What happens to it depends on two things, decided in routeBackgroundResult:
//
//   - the conversation is in a turn: the result is parked and handled again when
//     that turn ends, as if it had arrived then. It never enters the steering
//     queue, where a /stop, a full queue or a direct turn that does not drain it
//     would lose it, and where it would read as the user interrupting a tool
//     batch;
//   - the conversation is idle: a chat that takes late replies (Telegram,
//     WhatsApp, Discord...) gets a turn in the conversation, which answers there;
//     a web run, whose stream ended with the run, gets the result written into
//     the conversation for its next turn, with no turn of its own.

// maxParkedResults bounds the results waiting for one conversation's turn. Far
// above what a turn launches; past it the oldest goes, loudly.
const maxParkedResults = 100

// backgroundResultTarget is the conversation a system message's result goes
// to, with the agent that owns it. ok is false for a message that names no
// session or comes from an internal chat, which keep the main session, and for
// one that names a conversation that no longer exists (cleared or deleted while
// the work ran), which processSystemMessage drops.
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

// originTakesLateReplies reports whether the chat the work was launched from
// can receive a reply after the turn that launched it ended. Only a web run
// cannot: its stream closes with the run. Deciding on the resolver's answer
// instead would silence every chat it cannot map (Discord, WhatsApp groups).
func originTakesLateReplies(chatID string) bool {
	channel, _ := parseSystemOrigin(chatID)
	return channel != tools.WebRunChannel
}

// routeBackgroundResult handles a result whose conversation is busy (parked)
// or whose chat takes no late reply (written now). false = the caller runs a
// turn in the conversation, or the main session handles it.
func (al *AgentLoop) routeBackgroundResult(msg bus.InboundMessage) bool {
	sessionKey, agent, ok := al.backgroundResultTarget(msg)
	if !ok {
		return false
	}
	if al.parkIfBusy(sessionKey, msg) {
		return true
	}
	if originTakesLateReplies(msg.ChatID) {
		return false
	}
	deferred, err := al.deliverToConversation(agent, sessionKey, backgroundNote(msg))
	if err != nil {
		return true // writeMirrored logged it
	}
	logger.InfoCF("agent", "Recorded background result in its conversation", map[string]any{
		"sender_id":   msg.SenderID,
		"chat_id":     msg.ChatID,
		"session_key": sessionKey,
		"content_len": len(msg.Content),
		"deferred":    deferred,
	})
	return true
}

// parkIfBusy parks msg when a turn holds the session.
func (al *AgentLoop) parkIfBusy(sessionKey string, msg bus.InboundMessage) bool {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	if !al.mirrorBusyLocked(sessionKey) {
		return false
	}
	al.parkResultLocked(sessionKey, msg)
	return true
}

// parkBackgroundResult holds a result whose conversation turned out to be busy
// after routing (the session was claimed in between). The claimant may already
// have ended and released an empty list by the time the result is parked, so a
// session found idle hands the result straight back.
func (al *AgentLoop) parkBackgroundResult(sessionKey string, msg bus.InboundMessage) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	al.mirror.mu.Lock()
	al.parkResultLocked(sessionKey, msg)
	idle := !al.mirrorBusyLocked(sessionKey)
	al.mirror.mu.Unlock()
	stripe.Unlock()
	if idle {
		al.releaseParkedResults(sessionKey)
	}
}

// parkResultLocked queues msg for when the session's turn ends. Requires mu.
func (al *AgentLoop) parkResultLocked(sessionKey string, msg bus.InboundMessage) {
	if al.mirror.parked == nil {
		al.mirror.parked = make(map[string][]bus.InboundMessage)
	}
	queue := append(al.mirror.parked[sessionKey], msg)
	if dropped := len(queue) - maxParkedResults; dropped > 0 {
		queue = queue[dropped:]
		logger.ErrorCF("agent", "Dropped oldest background results waiting for a turn",
			map[string]any{"session_key": sessionKey, "dropped": dropped})
	}
	al.mirror.parked[sessionKey] = queue
	logger.InfoCF("agent", "Background result waits for the conversation's turn to end", map[string]any{
		"sender_id":   msg.SenderID,
		"chat_id":     msg.ChatID,
		"session_key": sessionKey,
		"waiting":     len(queue),
	})
}

// releaseParkedResults hands the results parked for a session back to the
// inbound bus once no turn holds it, in arrival order, so each is routed as if
// it had just arrived; after a /stop they become notes instead. Called where a
// turn ends, next to the mirror flush.
func (al *AgentLoop) releaseParkedResults(sessionKey string) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	al.mirror.mu.Lock()
	if al.mirrorBusyLocked(sessionKey) {
		al.mirror.mu.Unlock()
		stripe.Unlock()
		return
	}
	parked := al.mirror.parked[sessionKey]
	quiet := al.mirror.quiet[sessionKey]
	delete(al.mirror.parked, sessionKey)
	delete(al.mirror.quiet, sessionKey)
	al.mirror.mu.Unlock()
	if quiet {
		al.writeNotesLocked(sessionKey, parked)
	}
	stripe.Unlock()
	if quiet || len(parked) == 0 {
		return
	}

	// Off the turn-end path: the inbound queue may be full, and its consumer
	// is the loop this turn may be running on. A result the loop does not take
	// in time (full or closed bus) is written into the conversation instead of
	// being lost.
	go func() {
		for _, msg := range parked {
			ctx, cancel := context.WithTimeout(context.Background(), backgroundPublishTimeout)
			err := al.bus.PublishInbound(ctx, msg)
			cancel()
			if err == nil {
				continue
			}
			logger.ErrorCF(
				"agent",
				"Parked background result could not go back to the loop; writing it into the conversation",
				map[string]any{"session_key": sessionKey, "sender_id": msg.SenderID, "error": err.Error()},
			)
			al.recordBackgroundNote(msg)
		}
	}()
}

// backgroundPublishTimeout bounds how long a background result waits for room
// on the inbound queue before it is written into the conversation instead.
var backgroundPublishTimeout = 30 * time.Second

// deliverAsyncResult hands the result of async work (a spawn) to the loop, to
// be routed like any inbound message. Work the user stopped is written as a
// note instead: a stop means quiet, and its result may land after the stopped
// turn is gone, when nothing else keeps it from opening a turn. A result the
// loop does not take in time is written as a note too, rather than lost.
func (al *AgentLoop) deliverAsyncResult(msg bus.InboundMessage, workErr error) {
	if errors.Is(workErr, ErrSubTurnParentCanceled) {
		al.recordBackgroundNote(msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backgroundPublishTimeout)
	defer cancel()
	if err := al.bus.PublishInbound(ctx, msg); err != nil {
		logger.ErrorCF("agent", "Async result could not reach the loop; writing it into the conversation",
			map[string]any{"session_key": msg.SessionKey, "sender_id": msg.SenderID, "error": err.Error()})
		al.recordBackgroundNote(msg)
	}
}

// parkedResultsToNotes writes the results parked for a session into the
// conversation, for its next turn to read, instead of handing them back to the
// loop. A stop means the user wants the agent quiet: results that arrived
// during the stopped turn must not each open a turn of their own afterwards.
// With the turn still alive (it is being stopped), they stay parked, with what
// parks until it ends, and become notes then, after its rollback: queued behind
// the turn now, they would be cut by the cap of the mirror queue.
func (al *AgentLoop) parkedResultsToNotes(sessionKey string) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	if al.mirrorBusyLocked(sessionKey) {
		if al.mirror.quiet == nil {
			al.mirror.quiet = make(map[string]bool)
		}
		al.mirror.quiet[sessionKey] = true
		al.mirror.mu.Unlock()
		return
	}
	parked := al.mirror.parked[sessionKey]
	delete(al.mirror.parked, sessionKey)
	al.mirror.mu.Unlock()
	al.writeNotesLocked(sessionKey, parked)
}

// writeNotesLocked writes results into their conversation as notes, all of
// them, after what still waited for a flush. Requires the session's stripe,
// with no turn holding the session.
func (al *AgentLoop) writeNotesLocked(sessionKey string, results []bus.InboundMessage) {
	var agent *AgentInstance
	notes := make([]providers.Message, 0, len(results))
	for _, msg := range results {
		if _, owner, ok := al.backgroundResultTarget(msg); ok {
			agent = owner
			notes = append(notes, backgroundNote(msg))
		}
	}
	if len(notes) == 0 {
		return
	}
	al.mirror.mu.Lock()
	batch := append(al.mirror.pending[sessionKey], notes...)
	delete(al.mirror.pending, sessionKey)
	al.mirror.mu.Unlock()
	_ = al.writeMirrored(agent, sessionKey, batch) // logged there
}

// recordBackgroundNote writes a result into its conversation for the next turn
// to read, without a turn of its own. A result whose conversation is gone (or
// that names none) is dropped, as processSystemMessage drops it.
func (al *AgentLoop) recordBackgroundNote(msg bus.InboundMessage) {
	sessionKey, agent, ok := al.backgroundResultTarget(msg)
	if !ok {
		logger.WarnCF("agent", "Dropped background result with no conversation to write it into",
			map[string]any{
				"sender_id":   msg.SenderID,
				"session_key": msg.SessionKey,
				"content_len": len(msg.Content),
			})
		return
	}
	_, _ = al.deliverToConversation(agent, sessionKey, backgroundNote(msg))
}

func backgroundNote(msg bus.InboundMessage) providers.Message {
	return providers.Message{Role: "user", Content: systemMessageContent(msg)}
}
