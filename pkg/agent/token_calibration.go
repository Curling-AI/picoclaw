package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

const (
	// maxPromptTokenRatio bounds how much one measured call can shrink the
	// window: halving it already compacts early.
	maxPromptTokenRatio = 2.0
	// unmeasuredRefusalRatio is the ratio refusalWindow assumes for a model
	// not measured yet: in prod Anthropic counted at least 1.17 times our estimate.
	unmeasuredRefusalRatio = 1.5
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

func (a *AgentInstance) promptTokenRatio(model string) (float64, bool) {
	if a == nil || a.promptTokenRatios == nil {
		return 0, false
	}
	v, ok := a.promptTokenRatios.Load(model)
	if !ok {
		return 0, false
	}
	ratio, ok := v.(float64)
	return ratio, ok
}

// scaleWindow is window in our estimate's units when the upstream counts the
// prompt ratio times heavier. The output reserve is already in real tokens, so
// only the prompt's share scales.
func scaleWindow(window, reserve int, ratio float64) int {
	if ratio <= 1 || window <= reserve {
		return window
	}
	return int(float64(window-reserve)/ratio) + reserve
}

// calibratedWindow is window in our estimate's units for model: when its
// tokenizer counted calls heavier than we estimated, the prompt's share of the
// window shrinks by the same ratio. It never grows past window, so a lighter
// tokenizer or an unmeasured model keeps the nominal budget. reserve is the
// output reserve the caller counts against the window (zero for history-only
// thresholds).
func (a *AgentInstance) calibratedWindow(model string, window, reserve int) int {
	ratio, _ := a.promptTokenRatio(model)
	return scaleWindow(window, reserve, ratio)
}

// refusalWindow is the window past which a failed call is taken as refused for
// its size (see isSizeRefusal). A model with no measured call yet, after a
// restart, is assumed unmeasuredRefusalRatio heavier: a session already past
// the window fails every call and would never measure one.
func (a *AgentInstance) refusalWindow(model string, window, reserve int) int {
	ratio, ok := a.promptTokenRatio(model)
	if !ok {
		ratio = unmeasuredRefusalRatio
	}
	return scaleWindow(window, reserve, ratio)
}

// expectedTurnModel is the model a turn's calls go to, before routing runs:
// the cron model, the user's tier, else the main model. Media routing may still
// move a call, and a model with no measured call keeps the nominal window.
func expectedTurnModel(ts *turnState) string {
	if ts == nil || ts.agent == nil {
		return ""
	}
	if ts.agent.Router != nil && len(ts.agent.LightCandidates) > 0 {
		// The router picks main or light after this check; either one's ratio
		// could cut history the other model had room for.
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
// 404). An auth, billing or format refusal (a 400, an image past the
// provider's size) says what it is, and compacting history would not fix it.
func isSizeRefusal(err error, pastMeasuredWindow bool) bool {
	if !pastMeasuredWindow {
		return false
	}
	if failErr := providers.ClassifyError(err, "", ""); failErr != nil {
		switch failErr.Reason {
		case providers.FailoverAuth, providers.FailoverBilling, providers.FailoverFormat:
			return false
		}
	}
	return true
}
