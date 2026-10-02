package agent

import (
	"context"
	"errors"
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

// flakyVisionProvider fails its first `failures` calls and then describes the
// image: the vision sub-call failing in one iteration and working in the next.
type flakyVisionProvider struct {
	failures int
	calls    int
}

func (p *flakyVisionProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls <= p.failures {
		return nil, errors.New("vision upstream rejected the request")
	}
	return &providers.LLMResponse{Content: "a slide with a chat screenshot"}, nil
}

func (p *flakyVisionProvider) GetDefaultModel() string { return "vision-model" }

func imageCallMessages() []providers.Message {
	return []providers.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "use the attachment", Media: []string{testImageDataURL}},
	}
}

// The production fallback: delegation is on, the vision sub-call fails, and
// routeMediaTurn swaps the whole turn to the vision model. The tier yields.
func TestRouteTurnModel_FailedDelegationStaysOnVisionModel(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{resp: "unused"}, testImageDataURL)
	ts.agent.Provider = &flakyVisionProvider{failures: 1}
	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("routeTurnModel: %v", err)
	}
	if exec.llmModelName != "maestro-vision" {
		t.Fatalf("llmModelName = %q, want maestro-vision (delegation failed)", exec.llmModelName)
	}
}

// The routers run on every LLM call of the turn and the active model carries
// over between calls. A call pinned to vision must not keep the NEXT call there
// once the image is text: the tier comes back, with the main context budget.
func TestRouteTurnModel_TierReturnsOnceTheImageIsDescribed(t *testing.T) {
	p, ts, exec := mediaTierFixture(true, &recordingVisionProvider{resp: "unused"}, testImageDataURL)
	ts.agent.Provider = &flakyVisionProvider{failures: 1}
	ts.agent.ImageContextWindow = 128_000

	if err := p.routeTurnModel(context.Background(), ts, exec); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if exec.llmModelName != "maestro-vision" || exec.effectiveContextWindow != 128_000 {
		t.Fatalf("first call on %q with window %d, want maestro-vision with 128000",
			exec.llmModelName, exec.effectiveContextWindow)
	}

	// Next iteration: the pipeline rebuilds the call from the untouched working
	// set, and this time the sub-call describes the image.
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
