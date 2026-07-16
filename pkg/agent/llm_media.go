package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

var resolvedImagePathTagRegex = regexp.MustCompile(`\[image:[^\s\]][^\]]*\]`)

func messagesContainMedia(messages []providers.Message) bool {
	for _, msg := range messages {
		for _, ref := range msg.Media {
			if strings.TrimSpace(ref) != "" {
				return true
			}
		}
	}
	return false
}

func stripMessageMedia(messages []providers.Message) []providers.Message {
	if !messagesContainMedia(messages) {
		return messages
	}
	stripped := make([]providers.Message, len(messages))
	for i, msg := range messages {
		stripped[i] = msg
		stripped[i].Media = nil
	}
	return stripped
}

func isVisionUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// OpenRouter (and OpenAI-compatible) style.
	if strings.Contains(msg, "no endpoints found that support image input") {
		return true
	}

	// Common provider variants.
	if strings.Contains(msg, "does not support image input") ||
		strings.Contains(msg, "does not support image inputs") ||
		strings.Contains(msg, "does not support images") ||
		strings.Contains(msg, "image input is not supported") ||
		strings.Contains(msg, "images are not supported") ||
		strings.Contains(msg, "does not support vision") ||
		strings.Contains(msg, "unsupported content type: image_url") {
		return true
	}

	// Some providers return a generic "invalid" message that still mentions image_url.
	if strings.Contains(msg, "image_url") && strings.Contains(msg, "invalid") {
		return true
	}

	// DeepSeek and other strict providers reject the image_url field at the
	// JSON schema level with an "unknown variant" error rather than a semantic
	// "not supported" message.
	if strings.Contains(msg, "unknown variant") && strings.Contains(msg, "image_url") {
		return true
	}

	return false
}

func visionUnsupportedModelError(modelName string, imageModelConfigured bool) error {
	modelName = strings.TrimSpace(modelName)
	if imageModelConfigured {
		if modelName != "" {
			return fmt.Errorf(
				"selected vision model %q does not support image input; update agents.defaults.image_model to a multimodal model",
				modelName,
			)
		}
		return fmt.Errorf(
			"selected vision model does not support image input; update agents.defaults.image_model to a multimodal model",
		)
	}
	if modelName != "" {
		return fmt.Errorf(
			"active model %q does not support image input; configure agents.defaults.image_model with a multimodal model",
			modelName,
		)
	}
	return fmt.Errorf(
		"the active model does not support image input; configure agents.defaults.image_model with a multimodal model",
	)
}

func sameCandidateSet(a, b []providers.FallbackCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].StableKey() != b[i].StableKey() {
			return false
		}
	}
	return true
}

func messagesContainCurrentTurnMediaTurn(messages []providers.Message) bool {
	for _, msg := range messages {
		if len(msg.Media) > 0 {
			return true
		}
		if resolvedImagePathTagRegex.MatchString(msg.Content) {
			return true
		}
	}
	return false
}

func (p *Pipeline) routeMediaTurn(ts *turnState, exec *turnExecution) error {
	if p == nil || ts == nil || ts.agent == nil || exec == nil ||
		!messagesContainCurrentTurnMediaTurn(currentTurnMessages(exec.callMessages, exec.currentTurnStart)) {
		return nil
	}

	var targetCandidates []providers.FallbackCandidate
	var targetModelName string
	var routeReason string

	switch {
	case len(ts.agent.ImageCandidates) > 0:
		targetCandidates = append([]providers.FallbackCandidate(nil), ts.agent.ImageCandidates...)
		targetModelName = strings.TrimSpace(p.Cfg.Agents.Defaults.ImageModel)
		routeReason = "configured_image_model"
		// The vision model's context window is typically far smaller than the main
		// model's (e.g. glm-4.6v 128K vs glm-5.2 1M). Pin the turn's context budget
		// to it so compaction/trim targets the real limit — otherwise a big
		// document read is never compacted and the provider 400s "Prompt exceeds
		// max length". Set before the early-return so it applies on every media
		// iteration. (seucaranguejo fork)
		if ts.agent.ImageContextWindow > 0 {
			exec.effectiveContextWindow = ts.agent.ImageContextWindow
		}
	case exec.usedLight && len(ts.agent.Candidates) > 0:
		targetCandidates = append([]providers.FallbackCandidate(nil), ts.agent.Candidates...)
		targetModelName = strings.TrimSpace(ts.agent.Model)
		routeReason = "bypass_light_model_for_media"
	default:
		return nil
	}

	if len(targetCandidates) == 0 {
		return nil
	}

	targetModel := resolvedCandidateModel(targetCandidates, targetModelName)
	targetProvider := exec.activeProvider
	firstCandidate := targetCandidates[0]
	if provider, err := providerForFallbackCandidate(
		ts.agent,
		ts.agent.Provider,
		targetCandidates,
		firstCandidate.Provider,
		firstCandidate.Model,
	); err != nil {
		return err
	} else if provider != nil {
		targetProvider = provider
	}

	resolvedModelName := resolvedCandidateModelName(targetCandidates, targetModelName)
	if sameCandidateSet(exec.activeCandidates, targetCandidates) &&
		exec.activeModel == targetModel &&
		exec.llmModelName == resolvedModelName {
		return nil
	}

	exec.activeCandidates = targetCandidates
	exec.activeModel = targetModel
	exec.activeProvider = targetProvider
	exec.activeModelConfig = resolveActiveModelConfig(
		p.Cfg,
		ts.agent.Workspace,
		targetCandidates,
		targetModel,
		p.Cfg.Agents.Defaults.Provider,
	)
	exec.llmModelName = resolvedModelName
	exec.usedLight = false

	logger.InfoCF("agent", "Media turn routing selected model", map[string]any{
		"agent_id":       ts.agent.ID,
		"reason":         routeReason,
		"model":          exec.activeModel,
		"model_name":     exec.llmModelName,
		"candidates":     len(exec.activeCandidates),
		"messages_count": len(exec.callMessages),
	})

	return nil
}

// turnContextWindow returns the context budget for the model actually serving
// this turn: the (smaller) image-model window when a media turn routed to the
// vision model, else the agent's default. Keeps compaction from targeting the
// main model's huge window while the request is going to a small-context vision
// model. (seucaranguejo fork)
func turnContextWindow(ts *turnState, exec *turnExecution) int {
	if exec != nil && exec.effectiveContextWindow > 0 {
		return exec.effectiveContextWindow
	}
	if ts != nil && ts.agent != nil {
		return ts.agent.ContextWindow
	}
	return 0
}

// CronModelSessionPrefix marks a cron-job turn that must run on
// agents.defaults.cron_model. cron's ExecuteJob encodes it in the session key
// (the only per-turn signal the pipeline already carries) — keep in sync with
// the literal in pkg/tools/cron.go.
const CronModelSessionPrefix = "agent:cronmodel-"

// routeCronModelTurn swaps the active model to the agent's pre-built
// CronCandidates for cron jobs whose session key carries CronModelSessionPrefix
// (i.e. the job opted in via Payload.Model). Mirrors routeMediaTurn's swap, but
// keyed on the session key rather than media presence — cron curation turns
// carry no media.
func (p *Pipeline) routeCronModelTurn(ts *turnState, exec *turnExecution) error {
	if p == nil || ts == nil || ts.agent == nil || exec == nil {
		return nil
	}
	if !strings.HasPrefix(ts.sessionKey, CronModelSessionPrefix) || len(ts.agent.CronCandidates) == 0 {
		return nil
	}

	targetCandidates := append([]providers.FallbackCandidate(nil), ts.agent.CronCandidates...)
	targetModelName := strings.TrimSpace(p.Cfg.Agents.Defaults.CronModel)

	targetModel := resolvedCandidateModel(targetCandidates, targetModelName)
	targetProvider := exec.activeProvider
	firstCandidate := targetCandidates[0]
	if provider, err := providerForFallbackCandidate(
		ts.agent,
		ts.agent.Provider,
		targetCandidates,
		firstCandidate.Provider,
		firstCandidate.Model,
	); err != nil {
		return err
	} else if provider != nil {
		targetProvider = provider
	}

	resolvedModelName := resolvedCandidateModelName(targetCandidates, targetModelName)
	if sameCandidateSet(exec.activeCandidates, targetCandidates) &&
		exec.activeModel == targetModel &&
		exec.llmModelName == resolvedModelName {
		return nil
	}

	exec.activeCandidates = targetCandidates
	exec.activeModel = targetModel
	exec.activeProvider = targetProvider
	exec.activeModelConfig = resolveActiveModelConfig(
		p.Cfg,
		ts.agent.Workspace,
		targetCandidates,
		targetModel,
		p.Cfg.Agents.Defaults.Provider,
	)
	exec.llmModelName = resolvedModelName
	exec.usedLight = false

	logger.InfoCF("agent", "Cron model routing selected model", map[string]any{
		"agent_id":    ts.agent.ID,
		"model":       exec.activeModel,
		"model_name":  exec.llmModelName,
		"session_key": ts.sessionKey,
	})

	return nil
}
