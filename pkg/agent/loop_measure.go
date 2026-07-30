package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
)

// Medição de checkpoint de um Loop.
//
// O turno roda no POD porque é lá que os conectores estão — é de lá que sai o
// número. Mas quem COMPARA o número com a meta e grava o veredito é o
// control-plane: o pod é onde o agente executa código arbitrário, e um veredito
// escrito lá é um veredito que o agente influencia.
//
// Por isso este turno é restrito: ele observa e responde um número. Não manda
// mensagem, não agenda, não escreve memória, não delega — porque qualquer uma
// dessas seria uma forma de "medir" que na verdade AGE.

// measurementDeniedTools são as ferramentas que um turno de medição não pode
// ter. É uma lista de NEGAÇÃO convertida em allowlist na hora, e não uma
// allowlist escrita à mão, porque o conjunto de ferramentas cresce (MCP registra
// dezenas por conector) e uma allowlist estática silenciosamente cortaria
// justamente os conectores de onde o número vem.
//
// spawn/subagent estão aqui por um motivo específico: sem essa negação, o
// turno contornaria a restrição inteira delegando a um filho irrestrito.
var measurementDeniedTools = []string{
	// Agem no mundo
	"message", "send_file", "send_tts", "webhook",
	// Agem no próprio assistente
	"cron", "memory", "install_skill", "share",
	// Execução arbitrária
	"exec", "shell", "write_file", "edit_file", "append_file",
	// Delegação — a saída de emergência da restrição
	"spawn", "subagent", "spawn_status", "delegate",
}

// measurementProfile monta o perfil restrito a partir das ferramentas que o
// agente REALMENTE tem, menos as negadas.
func measurementProfile(agent *AgentInstance) config.EffectiveTurnProfile {
	profile := config.EffectiveTurnProfile{
		Enabled: true,
		// Sem histórico: a medição não é conversa, e histórico anterior só
		// convidaria o modelo a repetir um número já dito em vez de medir.
		HistoryMode: config.TurnProfileModeOff,
		// Prompt e skills FICAM: é deles que vem saber qual conector consultar e
		// como. Tirá-los tornaria a medição impossível, não mais segura.
		SystemPromptMode: config.TurnProfileModeDefault,
		SkillsMode:       config.TurnProfileModeDefault,
		ToolsMode:        config.TurnProfileModeCustom,
	}
	if agent == nil || agent.Tools == nil {
		profile.AllowedTools = []string{}
		return profile
	}
	for _, def := range agent.Tools.ToProviderDefs() {
		name := def.Function.Name
		if slices.Contains(measurementDeniedTools, name) {
			continue
		}
		profile.AllowedTools = append(profile.AllowedTools, name)
	}
	return profile
}

// MeasurementRequest descreve uma medição pedida pelo control-plane.
type MeasurementRequest struct {
	// SessionKey isola a medição. Deve ser própria (não a de uma conversa), para
	// o turno não aparecer no histórico do usuário.
	SessionKey string
	// Criterion é o critério em texto livre, como o usuário o escreveu
	// ("soma dos negócios ganhos no HubSpot no mês").
	Criterion string
	// Loop dá contexto: as instruções, a memória e as skills do loop costumam
	// conter o baseline e como consultá-lo.
	Loop LoopScope
}

// MeasureCheckpoint executa a medição e devolve a resposta CRUA do agente.
//
// Devolve texto, não número: quem interpreta é o control-plane, e ele grava a
// saída crua junto do valor. Se a medição for "o agente disse", o card precisa
// mostrar essas palavras — sem elas, um número inventado é indistinguível de um
// medido.
func (al *AgentLoop) MeasureCheckpoint(ctx context.Context, req MeasurementRequest) (string, error) {
	if al == nil {
		return "", fmt.Errorf("agent loop unavailable")
	}
	if strings.TrimSpace(req.Criterion) == "" {
		return "", fmt.Errorf("measurement criterion is required")
	}
	if err := al.ensureHooksInitialized(ctx); err != nil {
		return "", err
	}
	if err := al.ensureMCPInitialized(ctx); err != nil {
		return "", err
	}
	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		return "", fmt.Errorf("no default agent for measurement")
	}

	sessionKey := strings.TrimSpace(req.SessionKey)
	if sessionKey == "" {
		sessionKey = "loop-measure"
	}

	return al.runAgentLoop(ctx, agent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: measurementPrompt(req.Criterion),
		},
		Loop:                 req.Loop,
		TurnProfile:          measurementProfile(agent),
		DefaultResponse:      "",
		EnableSummary:        false,
		SendResponse:         false,
		SuppressToolFeedback: true,
		NoHistory:            true,
	})
}

// measurementPrompt pede o número E o caminho até ele.
//
// A origem não é enfeite: sem ela não dá para distinguir "consultei o CRM e
// somei" de "estimei". O control-plane grava as duas coisas, e a tela mostra as
// palavras cruas quando a origem for o próprio agente.
func measurementPrompt(criterion string) string {
	return "Measure this, and only this: " + strings.TrimSpace(criterion) + "\n\n" +
		"Use your connectors and read-only tools to get the CURRENT value. Do not act on " +
		"anything, do not send messages, do not change any record — only read.\n\n" +
		"Answer in exactly two lines and nothing else:\n" +
		"VALUE: <the number alone, no currency symbol, no thousands separator, dot as decimal>\n" +
		"SOURCE: <where the number came from — the connector/query you ran, or the words " +
		"\"estimate\" if you could not measure it>\n\n" +
		"If you cannot measure it, answer VALUE: unknown and explain in SOURCE."
}
