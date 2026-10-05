package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Auto-delegation for image turns (seucaranguejo fork).
//
// The upstream behavior (routeMediaTurn) SWAPS the whole turn to the vision
// model: the entire conversation — often 50+ messages with accumulated
// screenshots — is shipped to a small-context vision endpoint that both bills
// per vision token and rejects large multi-image tool-heavy arrays (prod: 400
// "Invalid API parameter" / "messages parameter is illegal" on glm-4.6v).
//
// Auto-delegation instead keeps the (text-capable) main model in control and,
// for each current-turn image, makes ONE bounded sub-call to the vision model —
// [brief context + the image only] — and injects the returned text description
// back into the conversation. The main model then continues the turn over text.
// The image is analyzed once per turn (memoized), regardless of how many agentic
// iterations re-resolve it.

const (
	// dataImageURLPrefix marks a base64 image already resolved into a message's
	// Media (vs a media:// ref or a plain [image:/path] tag the model cannot see).
	dataImageURLPrefix = "data:image/"

	// maxDelegationContextChars bounds the textual brief sent alongside the
	// image(s): enough task context to interpret them without shipping the thread.
	maxDelegationContextChars = 6000

	// maxDelegationOutputTokens caps the vision description length.
	maxDelegationOutputTokens = 4096

	// toolImageFollowUpPlaceholder is the throwaway content of the synthetic
	// message that carries tool-result images; replaced wholesale by the analysis.
	toolImageFollowUpPlaceholder = "[Loaded image from tool result above]"
)

// visionDelegationSystemPrompt frames the image model as a pure describe-and-
// report step feeding another agent — not the turn's agent.
const visionDelegationSystemPrompt = "You are a vision analysis assistant serving another AI agent. " +
	"Look at the image(s) and produce a thorough, precise, factual description of everything the agent might " +
	"need: visible text (verbatim/OCR), numbers, layout, UI elements, objects, people, colors, charts and any " +
	"notable detail. Do not speculate beyond what is visible, do not omit relevant detail, and do not ask " +
	"questions. Respond with the description only."

// mediaImageTarget is a current-turn message carrying resolved image data URLs.
type mediaImageTarget struct {
	idx    int
	images []string
}

// delegationOutcome is what delegateMediaTurn did with the call's images.
type delegationOutcome int

const (
	// delegationSkipped: delegation is off, no vision model is configured, or
	// the call carries no resolved image.
	delegationSkipped delegationOutcome = iota
	// delegationDone: every image of the call is text now.
	delegationDone
	// delegationFailed: an image stayed raw because the vision upstream failed
	// for a moment (timeout, 5xx, 429, network). Another try can work.
	delegationFailed
	// delegationRefused: an image stayed raw because the vision model refused
	// it (prod: 400 "Provided image is not valid"). Resending it to the same
	// model gets the same answer.
	delegationRefused
)

// delegateMediaTurn implements auto-delegation. Images it describes become
// text in exec.callMessages even when another one of the call fails. On
// delegationDone the caller SKIPS routeMediaTurn; otherwise it falls back to
// the legacy routeMediaTurn swap (graceful degradation), and the outcome tells
// it whether the vision model is worth another try.
func (p *Pipeline) delegateMediaTurn(
	ctx context.Context,
	ts *turnState,
	exec *turnExecution,
) (delegationOutcome, error) {
	if p == nil || ts == nil || ts.agent == nil || exec == nil {
		return delegationSkipped, nil
	}
	if !ts.agent.MediaDelegation || len(ts.agent.ImageCandidates) == 0 {
		return delegationSkipped, nil
	}

	start := normalizeCurrentTurnStart(exec.callMessages, exec.currentTurnStart)
	var targets []mediaImageTarget
	for idx := start; idx < len(exec.callMessages); idx++ {
		var imgs []string
		for _, ref := range exec.callMessages[idx].Media {
			if strings.HasPrefix(ref, dataImageURLPrefix) {
				imgs = append(imgs, ref)
			}
		}
		if len(imgs) > 0 {
			targets = append(targets, mediaImageTarget{idx: idx, images: imgs})
		}
	}
	if len(targets) == 0 {
		// Path-tag-only (image not loaded yet) or no media: nothing to analyze.
		return delegationSkipped, nil
	}

	// Resolve the vision provider/model, mirroring routeMediaTurn.
	targetCandidates := append([]providers.FallbackCandidate(nil), ts.agent.ImageCandidates...)
	targetModelName := strings.TrimSpace(p.Cfg.Agents.Defaults.ImageModel)
	targetModel := resolvedCandidateModel(targetCandidates, targetModelName)
	resolvedModelName := resolvedCandidateModelName(targetCandidates, targetModelName)
	first := targetCandidates[0]
	visionProvider, err := providerForFallbackCandidate(
		ts.agent, ts.agent.Provider, targetCandidates, first,
	)
	if err != nil || visionProvider == nil {
		logger.WarnCF("agent", "Media delegation: no vision provider, falling back to swap", map[string]any{
			"agent_id": ts.agent.ID,
			"error":    fmt.Sprint(err),
		})
		return delegationSkipped, nil
	}

	brief := delegationBrief(exec.callMessages, start, targets)

	if exec.mediaAnalysisCache == nil {
		exec.mediaAnalysisCache = map[string]string{}
	}

	// Detach callMessages from exec.messages' backing array before rewriting, so
	// the next iteration re-resolves the image from the untouched working set
	// (and hits the memo cache rather than re-billing the vision model).
	rewritten := append([]providers.Message(nil), exec.callMessages...)

	analyzed := 0
	outcome := delegationDone
	for _, t := range targets {
		key := hashImages(t.images)
		analysis, ok := exec.mediaAnalysisCache[key]
		if !ok {
			a, callErr := p.callVisionDelegate(ctx, ts, visionProvider, targetModel, brief, t.images)
			if callErr != nil {
				_, transient := transientLLMRetryReason(callErr)
				logger.WarnCF("agent", "Media delegation sub-call failed", map[string]any{
					"agent_id":   ts.agent.ID,
					"model_name": resolvedModelName,
					"transient":  transient,
					"error":      callErr.Error(),
				})
				if transient {
					// The upstream is struggling: leave the remaining images to
					// the swap instead of piling more sub-calls on it.
					outcome = delegationFailed
					break
				}
				outcome = delegationRefused
				continue
			}
			analysis = a
			exec.mediaAnalysisCache[key] = analysis
		}
		msg := rewritten[t.idx]
		msg.Media = stripDataImages(msg.Media)
		msg.Content = injectVisionAnalysis(msg.Content, analysis, resolvedModelName)
		rewritten[t.idx] = msg
		analyzed++
	}
	if analyzed == 0 {
		return outcome, nil
	}

	exec.callMessages = rewritten
	logger.InfoCF("agent", "Media turn delegated to vision model", map[string]any{
		"agent_id":       ts.agent.ID,
		"model_name":     resolvedModelName,
		"images":         len(targets),
		"analyzed":       analyzed,
		"messages_count": len(exec.callMessages),
	})
	return outcome, nil
}

// redescribeImages puts back the description this turn already has for every
// image a rebuild of the call (context retry) brought back raw, wherever it
// sits in the call. Memo only: no vision call.
func (p *Pipeline) redescribeImages(ts *turnState, exec *turnExecution) {
	if p == nil || ts == nil || ts.agent == nil || exec == nil || len(exec.mediaAnalysisCache) == 0 {
		return
	}
	modelName := resolvedCandidateModelName(
		ts.agent.ImageCandidates,
		strings.TrimSpace(p.Cfg.Agents.Defaults.ImageModel),
	)
	var rewritten []providers.Message
	for i, msg := range exec.callMessages {
		var images []string
		for _, ref := range msg.Media {
			if strings.HasPrefix(ref, dataImageURLPrefix) {
				images = append(images, ref)
			}
		}
		analysis, ok := exec.mediaAnalysisCache[hashImages(images)]
		if len(images) == 0 || !ok {
			continue
		}
		if rewritten == nil {
			rewritten = append([]providers.Message(nil), exec.callMessages...)
		}
		msg.Media = stripDataImages(msg.Media)
		msg.Content = injectVisionAnalysis(msg.Content, analysis, modelName)
		rewritten[i] = msg
	}
	if rewritten != nil {
		exec.callMessages = rewritten
	}
}

// callVisionDelegate makes the bounded, one-shot vision sub-call.
func (p *Pipeline) callVisionDelegate(
	ctx context.Context,
	ts *turnState,
	provider providers.LLMProvider,
	model string,
	brief string,
	images []string,
) (string, error) {
	userContent := "Analyze the attached image(s)."
	if strings.TrimSpace(brief) != "" {
		userContent = "Context — what the main agent is doing right now:\n" + brief +
			"\n\nAnalyze the attached image(s) with that context in mind."
	}
	msgs := []providers.Message{
		{Role: "system", Content: visionDelegationSystemPrompt},
		{Role: "user", Content: userContent, Media: append([]string(nil), images...)},
	}
	opts := map[string]any{
		"max_tokens":       maxDelegationOutputTokens,
		"temperature":      0.2,
		"prompt_cache_key": promptCacheKeyForSession(ts.promptCacheScope(), "vision"),
	}
	resp, err := provider.Chat(ctx, msgs, nil, model, opts)
	if err != nil {
		return "", err
	}
	// Out-of-pipeline call: report usage through the hooks so the meter sees
	// the vision sub-call (billed by the provider either way).
	notifyBackgroundLLM(ctx, p.al, "vision", model, resp)
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("vision delegate returned empty content")
	}
	return strings.TrimSpace(resp.Content), nil
}

// delegationBrief concatenates the current turn's textual messages (excluding
// the image-carrying ones) — the task the vision model is being asked to serve —
// truncated to a bounded size.
func delegationBrief(messages []providers.Message, start int, targets []mediaImageTarget) string {
	skip := make(map[int]bool, len(targets))
	for _, t := range targets {
		skip[t.idx] = true
	}
	var b strings.Builder
	for idx := start; idx < len(messages) && b.Len() < maxDelegationContextChars; idx++ {
		if skip[idx] {
			continue
		}
		c := strings.TrimSpace(messages[idx].Content)
		if c == "" {
			continue
		}
		b.WriteString(messages[idx].Role)
		b.WriteString(": ")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > maxDelegationContextChars {
		out = out[:maxDelegationContextChars]
	}
	return out
}

// hashImages produces a stable memo key for a set of image data URLs.
func hashImages(images []string) string {
	h := sha256.New()
	for _, img := range images {
		h.Write([]byte(img))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stripDataImages removes resolved image data URLs from a message's Media,
// preserving any non-image refs. Returns nil when nothing remains.
func stripDataImages(media []string) []string {
	if len(media) == 0 {
		return media
	}
	kept := make([]string, 0, len(media))
	for _, ref := range media {
		if !strings.HasPrefix(ref, dataImageURLPrefix) {
			kept = append(kept, ref)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// injectVisionAnalysis replaces a throwaway image placeholder with the analysis,
// or appends the analysis to real content.
func injectVisionAnalysis(content, analysis, modelName string) string {
	label := "[Vision analysis"
	if strings.TrimSpace(modelName) != "" {
		label += " (" + modelName + ")"
	}
	label += "]:\n" + analysis

	content = strings.TrimSpace(content)
	if content == "" || content == toolImageFollowUpPlaceholder {
		return label
	}
	return content + "\n\n" + label
}
