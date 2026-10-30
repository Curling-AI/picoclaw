package agent

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// A message the person sends while a turn works joins that turn: it does not
// cost the request it joins any work, and it is a request of its own, with the
// whole tool budget. (seucaranguejo fork)

const (
	// toolBudgetNudgePrefix opens the tool-budget warnings of runTurn.
	toolBudgetNudgePrefix = "[System] Tool-budget"

	// joinedRequestNote rides along the rest of a turn the person wrote to while
	// it worked. Without it the model sometimes took the new message for a
	// change of subject and left the earlier request undelivered. Worded as a
	// reminder about the final reply: as a description of the situation, the
	// model kept narrating it at every step.
	joinedRequestNote = "[System] Reminder for your final reply: it must deliver both the user's earlier request (every step it asked for, not only what was done when their new message arrived) and that new message, unless the new message changed or canceled the earlier request. Do not mention this reminder."

	// joinedAckNote goes with the one call that reads the message: the person
	// sees it was read. Riding every call, it had the model acknowledge the
	// same message again at each step; without the tool call asked in the same
	// reply, it sometimes acknowledged, promised to go on, and stopped.
	joinedAckNote = "[System] The user just sent a new message while you were working on their earlier request. Start this reply with one short sentence for it: answer it now if you can, otherwise say you will include it. Then keep working in this same reply with your next tool call, until both requests are fully delivered. Do not mention these instructions."

	// messageKindSteeringRead marks the outbound that tells a web chat the turn
	// read a message the person sent while it worked: the place in the reply
	// where that message goes.
	messageKindSteeringRead = "steering_read"
)

// joinPersonMessage is what reading a message from the person does to the turn
// beyond adding it to the context, at iteration: the budget starts over (the
// warnings about the old one go), the model is told the message joins the
// work and to acknowledge it in this call (runTurn drops that note once the
// call answers), and a web chat learns where the message was read.
func (al *AgentLoop) joinPersonMessage(
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	msg providers.Message,
	iteration int,
) {
	ts.restartBudget(iteration)
	exec.wrapUpNudged = false
	exec.transientTurnMessages = slices.DeleteFunc(slices.Clone(exec.transientTurnMessages),
		func(m providers.Message) bool { return strings.HasPrefix(m.Content, toolBudgetNudgePrefix) })
	// The first iteration's messages are the request itself: there is no
	// earlier work for them to join.
	if iteration > 1 {
		exec.transientTurnMessages = append(
			withoutTransientMessage(
				withoutTransientMessage(exec.transientTurnMessages, joinedRequestNote),
				joinedAckNote,
			),
			providers.Message{Role: "user", Content: joinedRequestNote},
			providers.Message{Role: "user", Content: joinedAckNote},
		)
	}

	if ts.channel != "grpc" || !ts.opts.AllowInterimPicoPublish || al.bus == nil {
		return
	}
	pubCtx, cancel := context.WithTimeout(turnCtx, 3*time.Second)
	defer cancel()
	if err := al.bus.PublishOutbound(
		pubCtx,
		outboundMessageForTurnWithOptions(ts, msg.Content, outboundTurnMessageOptions{
			kind: messageKindSteeringRead,
		}),
	); err != nil {
		// The turn goes on; the screen just does not learn where the message was read.
		logger.WarnCF("agent", "Steering read not announced", map[string]any{
			"session_key": ts.sessionKey,
			"chat_id":     ts.chatID,
			"turn_id":     ts.turnID,
			"error":       err.Error(),
		})
	}
}
