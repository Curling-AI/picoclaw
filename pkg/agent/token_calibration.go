package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// maxPromptTokenRatio bounds how much one measured call can shrink the window.
// Past it the upstream is counting what the estimate never sees (images, a
// cached prefix reported apart), and halving the window already compacts early.
const maxPromptTokenRatio = 2.0

// recordPromptTokens keeps how much heavier than our estimate the upstream
// counted a call sent to model. The chars heuristic undercounts some
// tokenizers: a session the estimate put at 85% of a 1M window was refused by
// the upstream as past it, and nothing compacted. (seucaranguejo fork)
func (a *AgentInstance) recordPromptTokens(
	model string,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	measured int,
) {
	if a == nil || a.promptTokenRatios == nil || strings.TrimSpace(model) == "" || measured <= 0 {
		return
	}
	estimated := estimateCallTokens(messages, toolDefs, 0)
	if estimated <= 0 {
		return
	}
	a.promptTokenRatios.Store(model, min(float64(measured)/float64(estimated), maxPromptTokenRatio))
}

// calibratedWindow is window in our estimate's units for model: when its
// tokenizer counted the last call heavier than we estimated, the window an
// estimate must fit shrinks by the same ratio. It never grows past window, so a
// lighter tokenizer keeps the nominal budget.
func (a *AgentInstance) calibratedWindow(model string, window int) int {
	if a == nil || a.promptTokenRatios == nil || window <= 0 {
		return window
	}
	v, ok := a.promptTokenRatios.Load(model)
	if !ok {
		return window
	}
	ratio, ok := v.(float64)
	if !ok || ratio <= 1 {
		return window
	}
	return int(float64(window) / ratio)
}

// expectedTurnModel is the model a turn's first call goes to, before routing
// runs: the user's tier, else the main model. Media and cron routing may still
// move it, and a model with no measured call keeps the nominal window.
func expectedTurnModel(ts *turnState) string {
	if ts == nil || ts.agent == nil {
		return ""
	}
	tier := strings.TrimSpace(ts.modelTier)
	if tier != "" && !strings.HasPrefix(ts.sessionKey, CronModelSessionPrefix) {
		if candidates := ts.agent.TierCandidates[tier]; len(candidates) > 0 {
			return candidates[0].Model
		}
	}
	return resolvedCandidateModel(ts.agent.Candidates, ts.agent.Model)
}
