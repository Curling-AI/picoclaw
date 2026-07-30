package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// O turno de medição observa e responde um número. Se ele puder mandar mensagem,
// agendar, escrever memória ou delegar, ele deixa de medir e passa a AGIR — que
// é exatamente o que a separação pod-mede / control-plane-julga existe para
// impedir.
func TestMeasurementProfile_NegaFerramentasQueAgem(t *testing.T) {
	registry := tools.NewToolRegistry()
	for _, tool := range []tools.Tool{
		fakeTool{name: "web_fetch"},
		fakeTool{name: "read_file"},
		fakeTool{name: "hubspot_search_deals"}, // conector MCP: é daqui que vem o número
		fakeTool{name: "message"},
		fakeTool{name: "cron"},
		fakeTool{name: "memory"},
		fakeTool{name: "exec"},
		fakeTool{name: "spawn"},
		fakeTool{name: "subagent"},
		fakeTool{name: "create_loop"},
	} {
		registry.Register(tool)
	}

	profile := measurementProfile(&AgentInstance{Tools: registry})

	if !profile.Enabled || profile.ToolsMode != config.TurnProfileModeCustom {
		t.Fatalf("perfil = %+v, want custom habilitado", profile)
	}
	// Histórico off: medição não é conversa, e histórico convidaria o modelo a
	// repetir um número já dito em vez de medir.
	if profile.HistoryMode != config.TurnProfileModeOff {
		t.Fatalf("HistoryMode = %q, want off", profile.HistoryMode)
	}

	for _, denied := range []string{"message", "cron", "memory", "exec", "spawn", "subagent", "create_loop"} {
		if slices.Contains(profile.AllowedTools, denied) {
			t.Fatalf("ferramenta que AGE ficou permitida: %q", denied)
		}
	}
	// E as de leitura têm de sobrar — sem elas não há medição, só restrição.
	for _, allowed := range []string{"web_fetch", "read_file", "hubspot_search_deals"} {
		if !slices.Contains(profile.AllowedTools, allowed) {
			t.Fatalf("ferramenta de leitura foi cortada: %q", allowed)
		}
	}
}

// A allowlist é derivada das ferramentas REAIS do agente. Uma lista estática
// cortaria em silêncio os conectores MCP, que nascem por conector e mudam.
func TestMeasurementProfile_SemFerramentasNaoExplode(t *testing.T) {
	if got := measurementProfile(nil); len(got.AllowedTools) != 0 {
		t.Fatalf("AllowedTools = %v, want vazio", got.AllowedTools)
	}
}

// O prompt precisa pedir o número E a origem: sem a origem não dá para
// distinguir "consultei o CRM" de "estimei", e um número estimado não pode
// parecer medido.
func TestMeasurementPrompt_PedeValorEOrigem(t *testing.T) {
	p := measurementPrompt("soma dos negócios ganhos no HubSpot")
	for _, want := range []string{"soma dos negócios ganhos no HubSpot", "VALUE:", "SOURCE:", "only read"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt não contém %q:\n%s", want, p)
		}
	}
}

// Perfil escolhido pelo chamador vence a config: sem isso, a config do
// assistente devolveria as ferramentas de escrita justamente ao turno restrito.
func TestResolveTurnProfileOptions_NaoSobrescreveOPerfilExplicito(t *testing.T) {
	explicit := config.EffectiveTurnProfile{
		Enabled:      true,
		ToolsMode:    config.TurnProfileModeCustom,
		AllowedTools: []string{"read_file"},
	}
	cfg := &config.Config{}
	cfg.Agents.Defaults.TurnProfile = config.TurnProfileConfig{
		Enabled: true,
		Tools:   config.TurnProfileBlock{Mode: config.TurnProfileModeDefault},
	}

	got, err := resolveTurnProfileOptions(cfg, processOptions{TurnProfile: explicit})
	if err != nil {
		t.Fatalf("resolveTurnProfileOptions: %v", err)
	}
	if !slices.Equal(got.TurnProfile.AllowedTools, []string{"read_file"}) {
		t.Fatalf("a config sobrescreveu o perfil restrito: %+v", got.TurnProfile)
	}
}

type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "fake" }
func (f fakeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (f fakeTool) Execute(_ context.Context, _ map[string]any) *tools.ToolResult {
	return tools.NewToolResult("")
}
