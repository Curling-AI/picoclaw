package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// tierFixture builds a pipeline whose agent knows the three user-facing tiers,
// with the turn starting on the main model.
func tierFixture(tier, sessionKey string) (*Pipeline, *turnState, *turnExecution) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.ModelTiers = map[string]string{
		"otimizado": "deepseek-v4-flash",
		"pro":       "glm-5.2",
		"ultra":     "kimi-k3",
	}
	p := &Pipeline{Cfg: cfg}
	agent := &AgentInstance{
		ID:         "tier-agent",
		Provider:   &recordingVisionProvider{resp: "ok"},
		Candidates: []providers.FallbackCandidate{{Provider: "openai", Model: "glm-5.2"}},
		TierCandidates: map[string][]providers.FallbackCandidate{
			"otimizado": {{Provider: "openai", Model: "deepseek-v4-flash"}},
			"pro":       {{Provider: "openai", Model: "glm-5.2"}},
			"ultra":     {{Provider: "openai", Model: "kimi-k3"}},
		},
	}
	ts := &turnState{agent: agent, modelTier: tier, sessionKey: sessionKey}
	exec := &turnExecution{
		activeCandidates: agent.Candidates,
		activeModel:      "glm-5.2",
		llmModelName:     "glm-5.2",
	}
	return p, ts, exec
}

func TestRouteModelTierTurn_SwapsToPickedTier(t *testing.T) {
	p, ts, exec := tierFixture("ultra", "agent:web-abc")
	if err := p.routeModelTierTurn(ts, exec); err != nil {
		t.Fatalf("routeModelTierTurn: %v", err)
	}
	if exec.llmModelName != "kimi-k3" {
		t.Fatalf("llmModelName = %q, want kimi-k3", exec.llmModelName)
	}
	// É o llmModelName que pipeline_finalize grava em Message.ModelName, ou
	// seja: a proveniência por mensagem sai daqui de graça.
	if len(exec.activeCandidates) != 1 {
		t.Fatalf("um tier tem que ser candidato ÚNICO (mais de um vira cadeia de "+
			"fallback e mata o streaming); veio %d", len(exec.activeCandidates))
	}
}

func TestRouteModelTierTurn_NoopCases(t *testing.T) {
	cases := []struct {
		name       string
		tier       string
		sessionKey string
	}{
		{"sem tier escolhido", "", "agent:web-abc"},
		{"tier desconhecido", "turbo", "agent:web-abc"},
		// Cron é restrição de CAPACIDADE; o tier é preferência, e preferência
		// não sobrepõe capacidade. A visão está nos TestRouteTurnModel_*.
		{"sessão de cron", "ultra", CronModelSessionPrefix + "job-1-uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ts, exec := tierFixture(tc.tier, tc.sessionKey)
			if err := p.routeModelTierTurn(ts, exec); err != nil {
				t.Fatalf("routeModelTierTurn: %v", err)
			}
			if exec.llmModelName != "glm-5.2" {
				t.Fatalf("deveria ficar no modelo principal, foi para %q", exec.llmModelName)
			}
		})
	}
}

// Tiers desligados (control-plane sem a tabela): o roteador não pode nem tentar.
func TestRouteModelTierTurn_DisabledWhenNoTiers(t *testing.T) {
	p, ts, exec := tierFixture("ultra", "agent:web-abc")
	ts.agent.TierCandidates = nil
	if err := p.routeModelTierTurn(ts, exec); err != nil {
		t.Fatalf("routeModelTierTurn: %v", err)
	}
	if exec.llmModelName != "glm-5.2" {
		t.Fatalf("sem tiers configurados nada troca, foi para %q", exec.llmModelName)
	}
}

// O tier é armado por sessão e consumido UMA vez — o mesmo contrato do
// pendingSkills. Sem isso, uma escolha vazaria para os turnos seguintes mesmo
// depois de o usuário voltar ao default.
func TestPendingModelTier_IsPerTurn(t *testing.T) {
	al := &AgentLoop{}
	al.SetPendingModelTier("agent:web-abc", "ultra")

	if got := al.takePendingModelTier("agent:web-abc"); got != "ultra" {
		t.Fatalf("primeira leitura = %q, want ultra", got)
	}
	if got := al.takePendingModelTier("agent:web-abc"); got != "" {
		t.Fatalf("segunda leitura deveria vir vazia, veio %q", got)
	}
	// Sessão errada não enxerga o tier de outra.
	al.SetPendingModelTier("agent:web-abc", "pro")
	if got := al.takePendingModelTier("agent:web-xyz"); got != "" {
		t.Fatalf("tier vazou entre sessões: %q", got)
	}
	// Entradas vazias não armam nada.
	al.SetPendingModelTier("", "ultra")
	al.SetPendingModelTier("agent:web-zzz", "")
	if got := al.takePendingModelTier("agent:web-zzz"); got != "" {
		t.Fatalf("tier vazio não deveria armar: %q", got)
	}
}

// mediaTierFixture mirrors the production shape: the main model is the default
// tier, the user picked an extra model, and a vision model is configured. The
// turn runs through routeTurnModel, the same router sequence as the pipeline.
func mediaTierFixture(
	delegation bool,
	vision *recordingVisionProvider,
	media ...string,
) (*Pipeline, *turnState, *turnExecution) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.ImageModel = "maestro-vision"
	main := []providers.FallbackCandidate{{Provider: "openai", Model: "maestro-flash"}}
	agent := &AgentInstance{
		ID:              "tier-agent",
		Provider:        vision,
		MediaDelegation: delegation,
		Model:           "maestro-flash",
		Candidates:      main,
		ImageCandidates: []providers.FallbackCandidate{{Provider: "openai", Model: "maestro-vision"}},
		TierCandidates: map[string][]providers.FallbackCandidate{
			"flash":       main,
			"gpt-6.1-sol": {{Provider: "openai", Model: "gpt-6.1-sol"}},
		},
	}
	ts := &turnState{agent: agent, modelTier: "gpt-6.1-sol", sessionKey: "agent:web-abc", media: media}
	exec := &turnExecution{
		activeCandidates: main,
		activeModel:      "maestro-flash",
		llmModelName:     "maestro-flash",
		currentTurnStart: 1,
		callMessages: []providers.Message{
			{Role: "system", Content: "system prompt"},
			{Role: "user", Content: "use the attachment", Media: media},
		},
	}
	return &Pipeline{Cfg: cfg}, ts, exec
}

// A document never goes to the vision model (it is read through tools), so the
// turn is a text turn for the main model and must follow the picked tier.
func TestRouteTurnModel_DocumentAttachmentFollowsPickedTier(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{resp: "unused"}, "uploads/relatorio.pdf")
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "gpt-6.1-sol" {
		t.Fatalf("llmModelName = %q, want gpt-6.1-sol (document turns are text turns)", exec.llmModelName)
	}
}

// Delegation already turned the image into text; the main model drives the
// turn, so the main model is the one the user picked.
func TestRouteTurnModel_DelegatedImageFollowsPickedTier(t *testing.T) {
	vision := &recordingVisionProvider{resp: "a slide with a chat screenshot"}
	p, ts, exec := mediaTierFixture(true, vision, testImageDataURL)
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if vision.calls != 1 {
		t.Fatalf("vision calls = %d, want 1 (the image is delegated)", vision.calls)
	}
	if exec.llmModelName != "gpt-6.1-sol" {
		t.Fatalf("llmModelName = %q, want gpt-6.1-sol (the image was delegated)", exec.llmModelName)
	}
}

// Without delegation the whole turn is swapped to the vision model, and that
// is a capability: the picked tier must not take the image away from it.
func TestRouteTurnModel_SwappedImageStaysOnVisionModel(t *testing.T) {
	p, ts, exec := mediaTierFixture(false, &recordingVisionProvider{resp: "unused"}, testImageDataURL)
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "maestro-vision" {
		t.Fatalf("llmModelName = %q, want maestro-vision (vision outranks the tier)", exec.llmModelName)
	}
}

func imageCallMessages() []providers.Message {
	return []providers.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "use the attachment", Media: []string{testImageDataURL}},
	}
}

// The production fallback: delegation is on and the vision model refuses the
// image (prod: 400 "Provided image is not valid" on tool screenshots). Sending
// the same image to the same model only repeats the refusal and ends the turn,
// so the picked tier takes the call, with the main context budget.
func TestRouteTurnModel_RefusedImageFollowsPickedTier(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{failures: 1}, testImageDataURL)
	ts.agent.ImageContextWindow = 128_000
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "gpt-6.1-sol" {
		t.Fatalf("llmModelName = %q, want gpt-6.1-sol (the vision model refused the image)", exec.llmModelName)
	}
	if exec.effectiveContextWindow != 0 {
		t.Fatalf("effectiveContextWindow = %d, want 0 (the call left the vision model)", exec.effectiveContextWindow)
	}
}

// A vision upstream that failed for a moment can still read the image on the
// swap's retries: the call stays on the vision model, as before the tier.
func TestRouteTurnModel_TransientDelegationFailureStaysOnVisionModel(t *testing.T) {
	vision := &recordingVisionProvider{errAt: map[int]error{1: errVisionUnavailable}}
	p, ts, exec := mediaTierFixture(true, vision, testImageDataURL)
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "maestro-vision" {
		t.Fatalf("llmModelName = %q, want maestro-vision (the upstream failed, the image is not refused)",
			exec.llmModelName)
	}
}

// Partial delegation follows the image left raw: refused goes to the tier,
// failed for a moment stays on the vision model.
func TestRouteTurnModel_PartialDelegationFollowsTheRawImage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"second refused", errVisionRefusedImage, "gpt-6.1-sol"},
		{"second unavailable", errVisionUnavailable, "maestro-vision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vision := &recordingVisionProvider{resp: "a receipt", errAt: map[int]error{2: tc.err}}
			p, ts, exec := mediaTierFixture(true, vision)
			exec.callMessages = twoImageMessages()
			if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
				t.Fatalf("routeTurnModel: %v", err)
			}
			if exec.llmModelName != tc.want {
				t.Fatalf("llmModelName = %q, want %q", exec.llmModelName, tc.want)
			}
		})
	}
}

// With no tier picked nothing else claims the call, and the swap to the vision
// model stays the fallback it was before.
func TestRouteTurnModel_RefusedImageWithoutTierFallsBackToVisionSwap(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{failures: 1}, testImageDataURL)
	ts.modelTier = ""
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "maestro-vision" {
		t.Fatalf("llmModelName = %q, want maestro-vision (no tier, legacy swap)", exec.llmModelName)
	}
}

// The routers run on every LLM call of the turn and the active model carries
// over between calls. A call pinned to vision must not keep the NEXT call there
// once the image is text: the tier comes back, with the main context budget.
func TestRouteTurnModel_TierReturnsOnceTheImageIsDescribed(t *testing.T) {
	vision := &recordingVisionProvider{resp: "a slide with a chat screenshot"}
	p, ts, exec := mediaTierFixture(true, vision)
	ts.agent.ImageContextWindow = 128_000
	// First call: the image is only a path tag, so there is nothing to delegate
	// yet and routeMediaTurn swaps the call to the vision model.
	exec.callMessages = []providers.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "what is on [image:/workspace/slide.png]?"},
	}

	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if exec.llmModelName != "maestro-vision" || exec.effectiveContextWindow != 128_000 {
		t.Fatalf("first call on %q with window %d, want maestro-vision with 128000",
			exec.llmModelName, exec.effectiveContextWindow)
	}

	// Next iteration: the image is resolved, and the sub-call describes it.
	exec.callMessages = imageCallMessages()
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if exec.llmModelName != "gpt-6.1-sol" {
		t.Fatalf("second call on %q, want gpt-6.1-sol (the image is text now)", exec.llmModelName)
	}
	if exec.effectiveContextWindow != 0 {
		t.Fatalf("effectiveContextWindow = %d, want 0 (the vision budget left with the vision model)",
			exec.effectiveContextWindow)
	}
}

// An image a TOOL loaded (no attachment from the user) still needs eyes. Before,
// the tier only checked the user's attachments and took this one off vision.
func TestRouteTurnModel_ToolLoadedImageStaysOnVisionModel(t *testing.T) {
	p, ts, exec := mediaTierFixture(false, &recordingVisionProvider{resp: "unused"})
	exec.callMessages = append(exec.callMessages,
		providers.Message{Role: "tool", Content: "Image loaded: slide.png"},
		providers.Message{Role: "user", Content: toolImageFollowUpPlaceholder, Media: []string{testImageDataURL}},
	)
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "maestro-vision" {
		t.Fatalf("llmModelName = %q, want maestro-vision (a tool loaded an image)", exec.llmModelName)
	}
}

// A multimodal main model can be the image model too. A text turn is then
// "on the vision candidates" from the start, and still follows the pick.
func TestRouteTurnModel_VisionModelSharedWithMainStillFollowsTier(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{resp: "unused"})
	ts.agent.ImageCandidates = ts.agent.Candidates
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "gpt-6.1-sol" {
		t.Fatalf("llmModelName = %q, want gpt-6.1-sol (a text turn needs no vision)", exec.llmModelName)
	}
}

// Without an image model, the main model is the only one the config trusts
// with images: an image call goes to it instead of going raw to the tier —
// from the main model itself, from the light model, or from the tier an
// earlier call of the turn picked (a document call, say, before a tool loaded
// the image).
func TestRouteTurnModel_ImageWithoutVisionModelStaysOnMainModel(t *testing.T) {
	cases := []struct {
		name     string
		onLight  bool
		previous string
	}{
		{"main model", false, ""},
		{"light model bypassed", true, "light-model"},
		{"tier of an earlier call", false, "gpt-6.1-sol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{resp: "unused"}, testImageDataURL)
			ts.agent.ImageCandidates = nil
			exec.usedLight = tc.onLight
			if tc.previous != "" {
				exec.activeCandidates = []providers.FallbackCandidate{{Provider: "openai", Model: tc.previous}}
				exec.activeModel = tc.previous
				exec.llmModelName = tc.previous
			}
			if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
				t.Fatalf("routeTurnModel: %v", err)
			}
			if exec.llmModelName != "maestro-flash" {
				t.Fatalf("llmModelName = %q, want maestro-flash (no image model; only the main model sees)",
					exec.llmModelName)
			}
		})
	}
}
