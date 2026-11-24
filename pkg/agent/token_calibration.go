package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

const (
	// maxPromptTokenRatio bounds how much one measured call can shrink the
	// window: halving it already compacts early.
	maxPromptTokenRatio = 2.0
	// minCalibrationWindowShare is the share of the window a call must take to
	// calibrate it: a small call is mostly tool schemas, and the system prompt
	// does not predict how the tokenizer counts a long conversation.
	minCalibrationWindowShare = 4
)

// recordPromptTokens keeps how much heavier than our estimate the upstream
// counted a call sent to model. The chars heuristic undercounts some
// tokenizers: a session the estimate put at 85% of a 1M window was refused by
// the upstream as past it, and nothing compacted. Only large calls without
// media calibrate (images are a fixed guess in the estimate), and the heaviest
// ratio seen stays: sessions of the same model share it, and the lighter one
// writing last would hand the heavier the nominal window back. (seucaranguejo
// fork)
func (a *AgentInstance) recordPromptTokens(
	model string,
	window int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	measured int,
) {
	if a == nil || a.promptTokenRatios == nil || strings.TrimSpace(model) == "" || measured <= 0 {
		return
	}
	estimated := estimateCallTokens(messages, toolDefs, 0)
	if estimated <= 0 || estimated < window/minCalibrationWindowShare || hasMedia(messages) {
		return
	}
	ratio := min(float64(measured)/float64(estimated), maxPromptTokenRatio)
	for {
		prev, loaded := a.promptTokenRatios.LoadOrStore(model, ratio)
		if !loaded {
			return
		}
		if prevRatio, ok := prev.(float64); ok && prevRatio >= ratio {
			return
		}
		if a.promptTokenRatios.CompareAndSwap(model, prev, ratio) {
			return
		}
	}
}

func hasMedia(messages []providers.Message) bool {
	for _, m := range messages {
		if len(m.Media) > 0 {
			return true
		}
	}
	return false
}

// calibratedWindow is window in our estimate's units for model: when its
// tokenizer counted calls heavier than we estimated, the window an estimate
// must fit shrinks by the same ratio. It never grows past window, so a lighter
// tokenizer keeps the nominal budget.
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

// expectedTurnModel is the model a turn's calls go to, before routing runs:
// the cron model, the user's tier, else the main model. Media routing may still
// move a call, and a model with no measured call keeps the nominal window.
func expectedTurnModel(ts *turnState) string {
	if ts == nil || ts.agent == nil {
		return ""
	}
	if strings.HasPrefix(ts.sessionKey, CronModelSessionPrefix) {
		if len(ts.agent.CronCandidates) > 0 {
			return ts.agent.CronCandidates[0].Model
		}
	} else if tier := strings.TrimSpace(ts.modelTier); tier != "" {
		if candidates := ts.agent.TierCandidates[tier]; len(candidates) > 0 {
			return candidates[0].Model
		}
	}
	return resolvedCandidateModel(ts.agent.Candidates, ts.agent.Model)
}

// isSizeRefusal says a failed call past the window measured for its model was
// refused for its size, whatever the error says: a gateway that tried several
// upstreams reports the last one's (Vercel's last Anthropic upstream answers
// 404). An auth or billing refusal says what it is, and compacting history
// would not fix it.
func isSizeRefusal(err error, pastMeasuredWindow bool) bool {
	if !pastMeasuredWindow {
		return false
	}
	if failErr := providers.ClassifyError(err, "", ""); failErr != nil {
		switch failErr.Reason {
		case providers.FailoverAuth, providers.FailoverBilling:
			return false
		}
	}
	return true
}
