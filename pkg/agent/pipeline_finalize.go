// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Finalize handles turn finalization, either:
// - Early return when allResponsesHandled=true (ExecuteTools already finalized)
// - Normal finalization for allResponsesHandled=false (sets finalContent, saves session, compact)
func (p *Pipeline) Finalize(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	turnStatus TurnEndStatus,
	finalContent string,
) (turnResult, error) {
	al := p.al
	ts.reportExecutionStage("preparing")

	// When allResponsesHandled=true, ExecuteTools already finalized
	// (added handledToolResponseSummary, saved session, set phase to Completed).
	// But still check for hard abort - if requested, abort the turn.
	if exec.allResponsesHandled {
		if ts.hardAbortRequested() {
			return al.abortTurn(ts)
		}
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			modelName:    exec.llmModelName,
			status:       turnStatus,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, nil
	}

	ts.setPhase(TurnPhaseFinalizing)
	ts.setFinalContent(finalContent)
	if !ts.opts.NoHistory {
		// Sent while the reply was being written: before the reply, as the user
		// got them. (seucaranguejo fork — see self_delivery.go)
		al.writeSelfDeliveriesBeforeReply(ts, exec)
		// Coordinator-synthesized fallback text (empty-response/tool-limit
		// boilerplate) is shown to the user but kept OUT of session history:
		// persisted fallbacks read as the model's own prior replies and make
		// it reproduce the failure on every following turn of the session.
		// The empty end of a silent turn is not a reply either. (seucaranguejo fork)
		if !exec.finalIsFallback && (finalContent != "" || !ts.opts.Silent) {
			p.persistAssistantReply(turnCtx, ts, assistantReply(exec, finalContent))
		}
		if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
			al.emitEvent(
				runtimeevents.KindAgentError,
				ts.eventMeta("runTurn", "turn.error"),
				ErrorPayload{
					Stage:   "session_save",
					Message: err.Error(),
				},
			)
			cancelConfiguredStreamingLLM(turnCtx, exec)
			return turnResult{status: TurnEndStatusError}, err
		}
	}

	if !ts.opts.NoHistory && ts.opts.EnableSummary {
		al.contextManager.Compact(
			turnCtx,
			&CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonSummarize,
				Budget:     ts.agent.ContextWindow,
			},
		)
	}

	contextUsage := computeContextUsage(ts.agent, ts.sessionKey)
	streamErr := finalizeConfiguredStreamingLLM(turnCtx, ts, exec, finalContent, contextUsage)
	// If streaming never became visible, keep the legacy Pico interim publish path
	// so the final answer is still delivered outside normal SendResponse.
	if ((streamErr != nil && !isConfiguredStreamingVisibleError(streamErr)) || exec.streamingFallback) &&
		!ts.opts.SendResponse && ts.opts.AllowInterimPicoPublish && finalContent != "" {
		msg := outboundMessageForTurnWithOptions(ts, finalContent, outboundTurnMessageOptions{
			modelName: exec.llmModelName,
		})
		msg.ContextUsage = contextUsage
		markFinalOutbound(&msg)
		_ = al.bus.PublishOutbound(turnCtx, msg)
	}
	if streamErr != nil && isConfiguredStreamingVisibleError(streamErr) {
		ts.setPhase(TurnPhaseCompleted)
		return turnResult{
			finalContent: finalContent,
			status:       TurnEndStatusError,
			followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
		}, streamErr
	}
	ts.setPhase(TurnPhaseCompleted)
	return turnResult{
		finalContent: finalContent,
		modelName:    exec.llmModelName,
		status:       turnStatus,
		followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
	}, nil
}

// assistantReply is the model's reply of this call, as the session keeps it.
func assistantReply(exec *turnExecution, content string) providers.Message {
	return providers.Message{
		Role:             "assistant",
		Content:          content,
		ModelName:        exec.llmModelName,
		ReasoningContent: responseReasoningContent(exec.response),
		LLMCall:          llmCallRecord(exec.llmRequestID, exec.response),
	}
}

// persistAssistantReply writes a reply into the session history: the turn's
// answer, or a reply the person watched being streamed before the message
// they sent meanwhile (keepWatchedReply). (seucaranguejo fork)
func (p *Pipeline) persistAssistantReply(turnCtx context.Context, ts *turnState, msg providers.Message) {
	ts.appendHistoryMessage(msg)
	ts.recordPersistedMessage(msg)
	ts.ingestMessage(turnCtx, p.al, msg)
}
