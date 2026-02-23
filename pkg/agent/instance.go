package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// AgentInstance represents a fully configured agent with its own workspace,
// session manager, context builder, and tool registry.
type AgentInstance struct {
	ID             string
	Name           string
	Model          string
	Fallbacks      []string
	Workspace      string
	MaxIterations          int
	ToolErrorNudgeThreshold int
	MaxTokens              int
	Temperature    float64
	ContextWindow  int

	// Memory management thresholds
	MaxHistoryMessages            int // 0 = disabled (no message count trigger)
	SummarizationThresholdPercent int // percentage of ContextWindow (default 90)
	KeepLastMessages              int // messages to keep after summarization (default 6)

	Provider       providers.LLMProvider
	Sessions       *session.SessionManager
	ContextBuilder *ContextBuilder
	Tools          *tools.ToolRegistry
	Subagents      *config.SubagentsConfig
	SkillsFilter   []string
	Candidates     []providers.FallbackCandidate
}

// NewAgentInstance creates an agent instance from config.
func NewAgentInstance(
	agentCfg *config.AgentConfig,
	defaults *config.AgentDefaults,
	cfg *config.Config,
	provider providers.LLMProvider,
) *AgentInstance {
	workspace := resolveAgentWorkspace(agentCfg, defaults)
	os.MkdirAll(workspace, 0o755)

	model := resolveAgentModel(agentCfg, defaults)
	fallbacks := resolveAgentFallbacks(agentCfg, defaults)

	restrict := defaults.RestrictToWorkspace
	toolsRegistry := tools.NewToolRegistry()
	toolsRegistry.Register(tools.NewReadFileTool(workspace, restrict))
	toolsRegistry.Register(tools.NewWriteFileTool(workspace, restrict))
	toolsRegistry.Register(tools.NewListDirTool(workspace, restrict))
	toolsRegistry.Register(tools.NewExecToolWithConfig(workspace, restrict, cfg))
	toolsRegistry.Register(tools.NewEditFileTool(workspace, restrict))
	toolsRegistry.Register(tools.NewAppendFileTool(workspace, restrict))

	sessionsDir := filepath.Join(workspace, "sessions")
	sessionsManager := session.NewSessionManager(sessionsDir)

	contextBuilder := NewContextBuilder(workspace)
	contextBuilder.SetToolsRegistry(toolsRegistry)

	agentID := routing.DefaultAgentID
	agentName := ""
	var subagents *config.SubagentsConfig
	var skillsFilter []string

	if agentCfg != nil {
		agentID = routing.NormalizeAgentID(agentCfg.ID)
		agentName = agentCfg.Name
		subagents = agentCfg.Subagents
		skillsFilter = agentCfg.Skills
	}

	maxIter := defaults.MaxToolIterations
	if maxIter == 0 {
		maxIter = 20
	}

	errorNudgeThreshold := defaults.ToolErrorNudgeThreshold
	if errorNudgeThreshold <= 0 {
		errorNudgeThreshold = 4
	}

	maxTokens := defaults.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}

	contextWindow := defaults.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 131072
	}

	// Model-specific context window takes highest priority
	if mc, err := cfg.GetModelConfig(model); err == nil && mc.ContextWindow > 0 {
		contextWindow = mc.ContextWindow
	}

	temperature := 0.7
	if defaults.Temperature != nil {
		temperature = *defaults.Temperature
	}

	// Memory management defaults
	summarizationPct := defaults.SummarizationThresholdPercent
	if summarizationPct <= 0 {
		summarizationPct = 90
	}
	keepLast := defaults.KeepLastMessages
	if keepLast <= 0 {
		keepLast = 6
	}

	// Resolve fallback candidates
	modelCfg := providers.ModelConfig{
		Primary:   model,
		Fallbacks: fallbacks,
	}
	candidates := providers.ResolveCandidates(modelCfg, defaults.Provider)

	// Inject same-provider alternatives from model_list after the primary.
	// If the primary model fails (e.g. 503), we try another model from the
	// same provider before falling back to a different provider entirely.
	if len(candidates) > 0 && len(cfg.ModelList) > 0 {
		primaryProvider := candidates[0].Provider
		seen := make(map[string]bool)
		for _, c := range candidates {
			seen[providers.ModelKey(c.Provider, c.Model)] = true
		}

		var sameProviderAlts []providers.FallbackCandidate
		for _, mc := range cfg.ModelList {
			ref := providers.ParseModelRef(mc.Model, "")
			if ref == nil || ref.Provider != primaryProvider {
				continue
			}
			key := providers.ModelKey(ref.Provider, ref.Model)
			if seen[key] {
				continue
			}
			seen[key] = true
			sameProviderAlts = append(sameProviderAlts, providers.FallbackCandidate{
				Provider: ref.Provider,
				Model:    ref.Model,
			})
		}

		if len(sameProviderAlts) > 0 {
			enhanced := make([]providers.FallbackCandidate, 0, len(candidates)+len(sameProviderAlts))
			enhanced = append(enhanced, candidates[0])          // primary
			enhanced = append(enhanced, sameProviderAlts...)     // same-provider alternatives
			enhanced = append(enhanced, candidates[1:]...)       // cross-provider fallbacks
			candidates = enhanced
		}
	}

	return &AgentInstance{
		ID:             agentID,
		Name:           agentName,
		Model:          model,
		Fallbacks:      fallbacks,
		Workspace:      workspace,
		MaxIterations:          maxIter,
		ToolErrorNudgeThreshold: errorNudgeThreshold,
		MaxTokens:      maxTokens,
		Temperature:    temperature,
		ContextWindow:  contextWindow,
		MaxHistoryMessages:            defaults.MaxHistoryMessages,
		SummarizationThresholdPercent: summarizationPct,
		KeepLastMessages:              keepLast,
		Provider:       provider,
		Sessions:       sessionsManager,
		ContextBuilder: contextBuilder,
		Tools:          toolsRegistry,
		Subagents:      subagents,
		SkillsFilter:   skillsFilter,
		Candidates:     candidates,
	}
}

// resolveAgentWorkspace determines the workspace directory for an agent.
func resolveAgentWorkspace(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
		return expandHome(strings.TrimSpace(agentCfg.Workspace))
	}
	if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" || routing.NormalizeAgentID(agentCfg.ID) == "main" {
		return expandHome(defaults.Workspace)
	}
	home, _ := os.UserHomeDir()
	id := routing.NormalizeAgentID(agentCfg.ID)
	return filepath.Join(home, ".picoclaw", "workspace-"+id)
}

// resolveAgentModel resolves the primary model for an agent.
func resolveAgentModel(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && agentCfg.Model != nil && strings.TrimSpace(agentCfg.Model.Primary) != "" {
		return strings.TrimSpace(agentCfg.Model.Primary)
	}
	return defaults.Model
}

// resolveAgentFallbacks resolves the fallback models for an agent.
func resolveAgentFallbacks(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) []string {
	if agentCfg != nil && agentCfg.Model != nil && agentCfg.Model.Fallbacks != nil {
		return agentCfg.Model.Fallbacks
	}
	return defaults.ModelFallbacks
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}
