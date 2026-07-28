package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// tierFixture builds a pipeline whose agent knows the three user-facing tiers,
// with the turn starting on the main model.
func tierFixture(tier, sessionKey string, media []string) (*Pipeline, *turnState, *turnExecution) {
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
	ts := &turnState{agent: agent, modelTier: tier, sessionKey: sessionKey, media: media}
	exec := &turnExecution{
		activeCandidates: agent.Candidates,
		activeModel:      "glm-5.2",
		llmModelName:     "glm-5.2",
	}
	return p, ts, exec
}

func TestRouteModelTierTurn_SwapsToPickedTier(t *testing.T) {
	p, ts, exec := tierFixture("ultra", "agent:web-abc", nil)
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
		media      []string
	}{
		{"sem tier escolhido", "", "agent:web-abc", nil},
		{"tier desconhecido", "turbo", "agent:web-abc", nil},
		// Visão e cron são restrição de CAPACIDADE; o tier é preferência, e
		// preferência não sobrepõe capacidade.
		{"turno com mídia", "ultra", "agent:web-abc", []string{"uploads/foto.png"}},
		{"sessão de cron", "ultra", CronModelSessionPrefix + "job-1-uuid", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ts, exec := tierFixture(tc.tier, tc.sessionKey, tc.media)
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
	p, ts, exec := tierFixture("ultra", "agent:web-abc", nil)
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
