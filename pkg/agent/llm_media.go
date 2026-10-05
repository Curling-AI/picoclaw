package agent

import (
	"context"
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

// unreadableImageNote stands in for an image the vision model refused, so
// the model answers knowing there was one.
const unreadableImageNote = "[An image was attached here, but it could not be read.]"

// imageNotSeenNote stands in for an image the model of the call cannot see.
const imageNotSeenNote = "[An image was attached here, but the model answering cannot see images.]"

// stripImages drops the resolved images of every message, keeps any other
// attachment, and leaves note where an image was. It reports whether it
// removed anything.
func stripImages(messages []providers.Message, note string) ([]providers.Message, bool) {
	var stripped []providers.Message
	for i, msg := range messages {
		if len(dataImages(msg.Media)) == 0 {
			continue
		}
		if stripped == nil {
			stripped = append([]providers.Message(nil), messages...)
		}
		stripped[i] = withImageText(msg, note)
	}
	if stripped == nil {
		return messages, false
	}
	return stripped, true
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

// visionUnsupportedModelError explains a model refusing image input. Only
// the image model itself, or a config without one, is an image_model problem.
func visionUnsupportedModelError(modelName string, onImageModel, imageModelConfigured bool) error {
	modelName = strings.TrimSpace(modelName)
	if onImageModel {
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
	if imageModelConfigured {
		return fmt.Errorf("active model %q does not support image input", modelName)
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

// messagesContainCurrentTurnMediaTurn reports whether the current turn carries
// media the VISION model should serve: images (resolved data URLs or path
// tags) and audio/video. Plain document attachments (docx/xlsx/pdf → "file")
// deliberately do NOT count: they are read via tools/skills, and routing a
// document-only turn to the vision model shipped the whole agentic turn to
// the (weaker) image model for no reason — observed in prod as users
// uploading spreadsheets and getting empty responses/errors right after,
// because gemini-2.5-flash-lite was suddenly driving a 40-tool turn.
func messagesContainCurrentTurnMediaTurn(messages []providers.Message) bool {
	for _, msg := range messages {
		for _, ref := range msg.Media {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			if strings.HasPrefix(ref, "data:") {
				// Resolved payloads: only visual/audio ones route.
				if strings.HasPrefix(ref, dataImageURLPrefix) ||
					strings.HasPrefix(ref, "data:audio/") ||
					strings.HasPrefix(ref, "data:video/") {
					return true
				}
				continue
			}
			// Plain path/ref: classify by filename. "file" (documents,
			// archives, unknown) stays on the main model.
			if t := inferMediaType(ref, ""); t == "image" || t == "audio" || t == "video" {
				return true
			}
		}
		if resolvedImagePathTagRegex.MatchString(msg.Content) {
			return true
		}
	}
	return false
}

// routeMediaTurn reports whether THIS call carries media that needs eyes. Such
// a call stays on the model routed here — the vision model, or the main model
// when none is configured, the only one the config then trusts with images —
// and the user's tier must not move it. (seucaranguejo fork)
func (p *Pipeline) routeMediaTurn(ts *turnState, exec *turnExecution) (bool, error) {
	if p == nil || ts == nil || ts.agent == nil || exec == nil ||
		!messagesContainCurrentTurnMediaTurn(currentTurnMessages(exec.callMessages, exec.currentTurnStart)) {
		return false, nil
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
	case len(ts.agent.Candidates) > 0:
		// No image model: the main model is the only one the config trusts with
		// images. The active model can be the light one, or a tier an earlier
		// call of the turn picked; both come back to it.
		targetCandidates = append([]providers.FallbackCandidate(nil), ts.agent.Candidates...)
		targetModelName = strings.TrimSpace(ts.agent.Model)
		routeReason = "main_model_for_media"
		if exec.usedLight {
			routeReason = "bypass_light_model_for_media"
		}
	default:
		return true, nil
	}

	if len(targetCandidates) == 0 {
		return true, nil
	}

	targetModel := resolvedCandidateModel(targetCandidates, targetModelName)
	targetProvider := exec.activeProvider
	firstCandidate := targetCandidates[0]
	if provider, err := providerForFallbackCandidate(
		ts.agent,
		ts.agent.Provider,
		ts.agent.Candidates,
		firstCandidate,
	); err != nil {
		return false, err
	} else if provider != nil {
		targetProvider = provider
	}

	resolvedModelName := resolvedCandidateModelName(targetCandidates, targetModelName)
	if sameCandidateSet(exec.activeCandidates, targetCandidates) &&
		exec.activeModel == targetModel &&
		exec.llmModelName == resolvedModelName {
		return true, nil
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

	return true, nil
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

// CronSessionPrefix marca um turno de cron comum. Mesma sincronia manual do
// CronModelSessionPrefix com tools.cron, e aqui ele serve para achar o uuid por
// execução que não pode entrar na chave de cache (ver promptCacheScopeForSession).
const CronSessionPrefix = "agent:cron-"

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
		firstCandidate,
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

// routeTurnModel picks the model for this LLM call. The order is the contract:
// the routers that enforce a CAPABILITY (vision, cron) run before the user's
// tier, which is only a preference. (seucaranguejo fork)
func (p *Pipeline) routeTurnModel(ctx context.Context, ts *turnState, exec *turnExecution) error {
	// Auto-delegation: for image turns, prefer a bounded vision sub-call over
	// swapping the whole turn to the vision model. Falls back to routeMediaTurn's
	// swap when disabled or when nothing was delegated.
	delegation, err := p.delegateMediaTurn(ctx, ts, exec)
	if err != nil {
		return err
	}
	needsEyes := false
	switch delegation {
	case delegationDone:
	case delegationRejected:
		// The vision model rejected the sub-call for a reason that is not the
		// image and would reject this call too: the image goes to the main
		// model, or to the tier picked below, and one that cannot see gets the
		// call again without it (CallLLM).
		if err = p.routeOffVisionModel(ts, exec); err != nil {
			return err
		}
	default:
		if needsEyes, err = p.routeMediaTurn(ts, exec); err != nil {
			return err
		}
	}
	if err := p.routeCronModelTurn(ts, exec); err != nil {
		return err
	}
	// Decided per call, not from the active model: that one carries over from
	// the previous call, and a call that needed eyes does not make the next
	// one need them. An image the vision model refused is a note by now; one
	// left raw by an upstream failing for a moment stays with the vision model,
	// whose retries can still read it.
	if needsEyes {
		return nil
	}
	return p.routeModelTierTurn(ts, exec)
}

// routeOffVisionModel puts the call on the main model, whatever an earlier
// call of the turn left active. (seucaranguejo fork)
func (p *Pipeline) routeOffVisionModel(ts *turnState, exec *turnExecution) error {
	if p == nil || ts == nil || ts.agent == nil || exec == nil || len(ts.agent.Candidates) == 0 {
		return nil
	}
	changed, err := p.activateModel(ts, exec, ts.agent.Candidates, strings.TrimSpace(ts.agent.Model))
	if err != nil || !changed {
		return err
	}
	logger.InfoCF("agent", "Media turn routing selected model", map[string]any{
		"agent_id":   ts.agent.ID,
		"reason":     "vision_rejected_the_call",
		"model":      exec.activeModel,
		"model_name": exec.llmModelName,
	})
	return nil
}

// activateModel makes candidates the model of this call, with the agent's
// context budget rather than the vision model's, and reports whether anything
// changed. (seucaranguejo fork)
func (p *Pipeline) activateModel(
	ts *turnState,
	exec *turnExecution,
	candidates []providers.FallbackCandidate,
	modelName string,
) (bool, error) {
	candidates = append([]providers.FallbackCandidate(nil), candidates...)
	targetModel := resolvedCandidateModel(candidates, modelName)
	targetProvider := exec.activeProvider
	if provider, err := providerForFallbackCandidate(
		ts.agent,
		ts.agent.Provider,
		candidates,
		candidates[0],
	); err != nil {
		return false, err
	} else if provider != nil {
		targetProvider = provider
	}

	resolvedModelName := resolvedCandidateModelName(candidates, modelName)
	if sameCandidateSet(exec.activeCandidates, candidates) &&
		exec.activeModel == targetModel &&
		exec.llmModelName == resolvedModelName {
		return false, nil
	}

	exec.activeCandidates = candidates
	exec.activeModel = targetModel
	exec.activeProvider = targetProvider
	exec.activeModelConfig = resolveActiveModelConfig(
		p.Cfg,
		ts.agent.Workspace,
		candidates,
		targetModel,
		p.Cfg.Agents.Defaults.Provider,
	)
	exec.llmModelName = resolvedModelName
	exec.usedLight = false
	// A vision budget routeMediaTurn pinned on an earlier call belongs to the
	// vision model, not to the model taking over.
	exec.effectiveContextWindow = 0
	return true, nil
}

// routeModelTierTurn swaps the active model to the tier the USER picked in the
// composer (turnState.modelTier, armed via SetPendingModelTier). Third sibling
// of routeMediaTurn/routeCronModelTurn, and the same swap.
//
// It gives way to both of them, on purpose: a call carrying an image that
// routeMediaTurn kept on a model that can see it, and a cron turn pinned to the
// cron model, are CAPABILITY constraints, while the tier is a user preference.
// Preference does not get to override capability — a photo sent while "Ultra"
// is selected still has to go to a model that can see it. routeTurnModel
// enforces the media half; an image the vision model refused is a note and
// needs nobody's eyes.
//
// Carrying media is not the same as needing vision: a document is read through
// tools, and an image delegateMediaTurn already described is text by now. Both
// stay on the main model, and the main model is the picked tier.
// (seucaranguejo fork)
func (p *Pipeline) routeModelTierTurn(ts *turnState, exec *turnExecution) error {
	if p == nil || ts == nil || ts.agent == nil || exec == nil {
		return nil
	}
	tier := strings.TrimSpace(ts.modelTier)
	if tier == "" || len(ts.agent.TierCandidates) == 0 {
		return nil
	}
	// Cron pins its own model; a cron session never carries a user tier anyway,
	// but the guard keeps it true if that ever changes.
	if strings.HasPrefix(ts.sessionKey, CronModelSessionPrefix) {
		return nil
	}
	targetCandidates := ts.agent.TierCandidates[tier]
	if len(targetCandidates) == 0 {
		// Tier desconhecido (config mudou entre o envio e o turno): fica no
		// modelo principal em vez de errar — o usuário não perde a mensagem.
		logger.WarnCF("agent", "Unknown model tier; staying on the main model", map[string]any{
			"agent_id": ts.agent.ID,
			"tier":     tier,
		})
		return nil
	}
	changed, err := p.activateModel(ts, exec, targetCandidates, targetCandidates[0].Model)
	if err != nil || !changed {
		return err
	}

	logger.InfoCF("agent", "Model tier routing selected model", map[string]any{
		"agent_id":    ts.agent.ID,
		"tier":        tier,
		"model":       exec.activeModel,
		"model_name":  exec.llmModelName,
		"session_key": ts.sessionKey,
	})

	return nil
}
