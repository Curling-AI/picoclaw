package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers/common"
)

func hasSmartFallback(err error) bool {
	var refusal *common.HTTPError
	return errors.As(err, &refusal) && refusal != nil && refusal.StatusCode == 404 &&
		refusal.ErrorCode == "model_not_found" && refusal.FallbackTier == "flash"
}

func (p *Pipeline) fallbackToSmart(ctx context.Context, ts *turnState, exec *turnExecution) (bool, error) {
	candidates := ts.agent.TierCandidates["flash"]
	if len(candidates) == 0 {
		return false, nil
	}
	from := exec.llmModelName
	changed, err := p.activateModel(ts, exec, candidates, candidates[0].Model)
	if err != nil || !changed {
		return false, err
	}
	ts.modelTier = "flash"
	exec.llmModel = exec.activeModel
	delete(exec.llmOpts, "thinking_level")
	delete(exec.llmOpts, "reasoning_effort")
	delete(exec.llmOpts, "native_search")
	exec.useNativeSearch = false
	applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)
	logger.WarnCF("agent", "Removed model replaced with ONE Smart", map[string]any{"session_key": ts.sessionKey, "from_model": from, "model": exec.llmModelName})
	if ts.channel == "grpc" && ts.opts.AllowInterimPicoPublish && p.al != nil && p.al.bus != nil {
		payload, err := json.Marshal(map[string]string{"fromTier": from, "toTier": "flash", "reason": "model_not_found"})
		if err != nil {
			return false, err
		}
		notifyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := p.al.bus.PublishOutbound(notifyCtx, outboundMessageForTurnWithOptions(ts, string(payload), outboundTurnMessageOptions{kind: "model_fallback"})); err != nil {
			logger.WarnCF("agent", "Model fallback notification failed", map[string]any{"error": err.Error()})
		}
	}
	return true, nil
}
