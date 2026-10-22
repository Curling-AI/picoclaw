package agent

import (
	"context"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// DirectContinuation is a session whose direct turn (ProcessDirect*) ended
// with messages still queued for it: the chat its reply goes to, and the
// choices of the run the continuation belongs to (the tier and the forced
// skills), which a continuation would otherwise lose. (seucaranguejo fork)
type DirectContinuation struct {
	SessionKey string
	Channel    string
	ChatID     string
	ModelTier  string
	Skills     []string
}

// ContinueDirect is Continue for a direct turn's session. The turn it runs gets
// what a direct turn gets: live output on the chat, and the run's tier and
// skills. Continue's goes out silent, on the main model.
//
// ran tells "read nothing" apart from a turn that ran and ended without text,
// as a handled reply (a connector card, the message tool) does:
//   - (zero Result, false, nil): no message queued, or another continuation of
//     the session is already reading them;
//   - ErrSessionBusy (ran false): a turn holds the session, and the messages
//     stay queued: that turn reads them if it still polls, and otherwise
//     whoever drains the queue once it ends does (Run, or the caller of a
//     direct turn);
//   - ErrTurnAborted (ran true): the continuation was aborted (a /stop, a hard
//     abort) before it finished.
//
// The Result is the turn's terminal one, as ProcessDirectWithResult reports
// it: a turn that ran out of answers or of tool rounds ends on its fallback
// text with Status Failed and no error, so the caller cannot take a nil error
// for a completed turn.
//
// (seucaranguejo fork)
func (al *AgentLoop) ContinueDirect(ctx context.Context, req DirectContinuation) (turnresult.Result, bool, error) {
	asDirect := func(opts processOptions) (processOptions, error) {
		opts.AllowInterimPicoPublish = true
		opts.ModelTier = req.ModelTier
		opts.ForcedSkills = append(opts.ForcedSkills, req.Skills...)
		// runAgentLoopResult resolves the profile of every turn; resolving it
		// here too, before the queue is read, makes a bad one fail with the
		// messages still queued.
		return resolveTurnProfileOptions(al.GetConfig(), opts)
	}
	result, ran, err := al.continueSession(ctx, req.SessionKey, req.Channel, req.ChatID, asDirect)
	if err == nil && ran && result.Status == turnresult.Canceled {
		return turnresult.Result{Status: turnresult.Canceled}, true, ErrTurnAborted
	}
	return result, ran, err
}

// continueSession is Continue's body, shared with ContinueDirect (an upstream
// change to Continue lands here): it runs the turn with shape applied to its
// options, and reports whether it ran one. shape runs before the queue is
// read, so a failure there leaves the messages queued. (seucaranguejo fork)
func (al *AgentLoop) continueSession(
	ctx context.Context,
	sessionKey, channel, chatID string,
	shape func(processOptions) (processOptions, error),
) (turnresult.Result, bool, error) {
	// Claim the session with a unique placeholder to prevent a TOCTOU race where two
	// concurrent Continue calls for the same session both pass the active-turn
	// check and create parallel turns. The placeholder is replaced by the real
	// turnState inside runAgentLoop → registerActiveTurn.
	// The early exits release it through releaseSessionTurnState so a delivery
	// mirrored while it was held still reaches the chat session. (seucaranguejo
	// fork — ver delivery_mirror.go)
	placeholder := &turnState{
		turnID: "pending-continue-" + sessionKey + "-" + fmt.Sprintf("%d", al.turnSeq.Add(1)),
		phase:  TurnPhaseSetup,
	}
	if _, loaded := al.activeTurnStates.LoadOrStore(sessionKey, placeholder); loaded {
		if active := al.GetActiveTurnBySession(sessionKey); active != nil {
			return turnresult.Result{}, false, fmt.Errorf("%w: turn %s is still active for session %q",
				ErrSessionBusy, active.TurnID, sessionKey)
		}
		// Another Continue just claimed the slot; let it handle the steering.
		return turnresult.Result{}, false, nil
	}

	if err := al.ensureHooksInitialized(ctx); err != nil {
		al.releaseSessionTurnState(sessionKey, placeholder)
		return turnresult.Result{}, false, err
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		al.releaseSessionTurnState(sessionKey, placeholder)
		return turnresult.Result{}, false, err
	}

	agent := al.agentForSession(sessionKey)
	if agent == nil {
		al.releaseSessionTurnState(sessionKey, placeholder)
		return turnresult.Result{}, false, fmt.Errorf("no agent available for session %q", sessionKey)
	}

	var scope *session.SessionScope
	if metaStore, ok := agent.Sessions.(session.MetadataAwareSessionStore); ok {
		scope = metaStore.GetSessionScope(sessionKey)
	}
	opts := continuationOptions(sessionKey, channel, chatID, scope)
	if shape != nil {
		var err error
		if opts, err = shape(opts); err != nil {
			al.releaseSessionTurnState(sessionKey, placeholder)
			return turnresult.Result{}, false, err
		}
	}

	opts.InitialSteeringMessages = al.dequeueSteeringMessagesForScopeWithFallback(sessionKey)
	if len(opts.InitialSteeringMessages) == 0 {
		al.releaseSessionTurnState(sessionKey, placeholder)
		return turnresult.Result{}, false, nil
	}

	if tool, ok := agent.Tools.Get("message"); ok {
		if resetter, ok := tool.(interface{ ResetSentInRound(sessionKey string) }); ok {
			resetter.ResetSentInRound(sessionKey)
		}
	}

	result, err := al.runAgentLoopResult(ctx, agent, opts)
	return result, true, err
}
