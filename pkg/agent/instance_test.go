package agent

import (
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestNewAgentInstance_UsesDefaultsTemperatureAndMaxTokens(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-instance-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         1234,
				MaxToolIterations: 5,
			},
		},
	}

	configuredTemp := 1.0
	cfg.Agents.Defaults.Temperature = &configuredTemp

	provider := &mockProvider{}
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, provider)

	if agent.MaxTokens != 1234 {
		t.Fatalf("MaxTokens = %d, want %d", agent.MaxTokens, 1234)
	}
	if agent.Temperature != 1.0 {
		t.Fatalf("Temperature = %f, want %f", agent.Temperature, 1.0)
	}
}

func TestNewAgentInstance_DefaultsTemperatureWhenZero(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-instance-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         1234,
				MaxToolIterations: 5,
			},
		},
	}

	configuredTemp := 0.0
	cfg.Agents.Defaults.Temperature = &configuredTemp

	provider := &mockProvider{}
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, provider)

	if agent.Temperature != 0.0 {
		t.Fatalf("Temperature = %f, want %f", agent.Temperature, 0.0)
	}
}

func TestNewAgentInstance_DefaultsTemperatureWhenUnset(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-instance-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         1234,
				MaxToolIterations: 5,
			},
		},
	}

	provider := &mockProvider{}
	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, provider)

	if agent.Temperature != 0.7 {
		t.Fatalf("Temperature = %f, want %f", agent.Temperature, 0.7)
	}
}

// --- Same-provider candidate injection tests ---

// newTestConfig creates a minimal config with a model_list and default model for testing.
func newTestConfig(t *testing.T, defaultModel string, modelList []config.ModelConfig) (*config.Config, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-instance-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             defaultModel,
				MaxTokens:         4096,
				MaxToolIterations: 5,
			},
		},
		ModelList: modelList,
	}
	return cfg, func() { os.RemoveAll(tmpDir) }
}

func TestNewAgentInstance_InjectsSameProviderAlternatives(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", []config.ModelConfig{
		{ModelName: "gpt-4", Model: "openai/gpt-4", APIKey: "k"},
		{ModelName: "gpt-4o-mini", Model: "openai/gpt-4o-mini", APIKey: "k"},
		{ModelName: "claude", Model: "anthropic/claude", APIKey: "k"},
	})
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// Should be: [openai/gpt-4, openai/gpt-4o-mini, ...]
	// Primary first, then same-provider alt, then any cross-provider fallbacks
	if len(agent.Candidates) < 2 {
		t.Fatalf("Candidates = %d, want at least 2", len(agent.Candidates))
	}
	if agent.Candidates[0].Provider != "openai" || agent.Candidates[0].Model != "gpt-4" {
		t.Errorf("Candidates[0] = %s/%s, want openai/gpt-4", agent.Candidates[0].Provider, agent.Candidates[0].Model)
	}
	if agent.Candidates[1].Provider != "openai" || agent.Candidates[1].Model != "gpt-4o-mini" {
		t.Errorf("Candidates[1] = %s/%s, want openai/gpt-4o-mini", agent.Candidates[1].Provider, agent.Candidates[1].Model)
	}
}

func TestNewAgentInstance_SameProviderAlts_NoDuplicates(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", []config.ModelConfig{
		{ModelName: "gpt-4", Model: "openai/gpt-4", APIKey: "k"},
		{ModelName: "gpt-4-dup", Model: "openai/gpt-4", APIKey: "k2"}, // same model, different name
		{ModelName: "gpt-4o", Model: "openai/gpt-4o", APIKey: "k"},
	})
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// gpt-4 is the primary, gpt-4-dup is a duplicate of gpt-4, gpt-4o is a real alt
	seen := make(map[string]int)
	for _, c := range agent.Candidates {
		key := providers.ModelKey(c.Provider, c.Model)
		seen[key]++
	}
	if seen["openai/gpt-4"] != 1 {
		t.Errorf("openai/gpt-4 appears %d times, want 1 (no duplicates)", seen["openai/gpt-4"])
	}
	if seen["openai/gpt-4o"] != 1 {
		t.Errorf("openai/gpt-4o appears %d times, want 1", seen["openai/gpt-4o"])
	}
}

func TestNewAgentInstance_SameProviderAlts_NoAlternativesAvailable(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", []config.ModelConfig{
		{ModelName: "gpt-4", Model: "openai/gpt-4", APIKey: "k"},
		{ModelName: "claude", Model: "anthropic/claude", APIKey: "k"},
	})
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// Only the primary — no same-provider alternatives exist, anthropic is cross-provider
	if len(agent.Candidates) != 1 {
		t.Errorf("Candidates = %d, want 1 (only primary, no same-provider alts)", len(agent.Candidates))
	}
}

func TestNewAgentInstance_SameProviderAlts_EmptyModelList(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", nil)
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// Just the primary candidate from ResolveCandidates
	if len(agent.Candidates) != 1 {
		t.Errorf("Candidates = %d, want 1", len(agent.Candidates))
	}
}

func TestNewAgentInstance_SameProviderAlts_PreservesExplicitFallbackOrder(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", []config.ModelConfig{
		{ModelName: "gpt-4", Model: "openai/gpt-4", APIKey: "k"},
		{ModelName: "gpt-4o-mini", Model: "openai/gpt-4o-mini", APIKey: "k"},
		{ModelName: "claude", Model: "anthropic/claude", APIKey: "k"},
	})
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"
	cfg.Agents.Defaults.ModelFallbacks = []string{"anthropic/claude"}

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// Expected order: primary, same-provider alt, explicit cross-provider fallback
	// [openai/gpt-4, openai/gpt-4o-mini, anthropic/claude]
	if len(agent.Candidates) != 3 {
		t.Fatalf("Candidates = %d, want 3", len(agent.Candidates))
	}
	if agent.Candidates[0].Provider != "openai" || agent.Candidates[0].Model != "gpt-4" {
		t.Errorf("Candidates[0] = %s/%s, want openai/gpt-4", agent.Candidates[0].Provider, agent.Candidates[0].Model)
	}
	if agent.Candidates[1].Provider != "openai" || agent.Candidates[1].Model != "gpt-4o-mini" {
		t.Errorf("Candidates[1] = %s/%s, want openai/gpt-4o-mini (same-provider alt before cross-provider)", agent.Candidates[1].Provider, agent.Candidates[1].Model)
	}
	if agent.Candidates[2].Provider != "anthropic" || agent.Candidates[2].Model != "claude" {
		t.Errorf("Candidates[2] = %s/%s, want anthropic/claude", agent.Candidates[2].Provider, agent.Candidates[2].Model)
	}
}

func TestNewAgentInstance_SameProviderAlts_CrossProviderNotInjected(t *testing.T) {
	cfg, cleanup := newTestConfig(t, "gpt-4", []config.ModelConfig{
		{ModelName: "gpt-4", Model: "openai/gpt-4", APIKey: "k"},
		{ModelName: "claude", Model: "anthropic/claude", APIKey: "k"},
		{ModelName: "gemini", Model: "gemini/gemini-2.0-flash", APIKey: "k"},
	})
	defer cleanup()
	cfg.Agents.Defaults.Provider = "openai"

	agent := NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})

	// Only the primary — anthropic and gemini are cross-provider, should NOT be injected
	for _, c := range agent.Candidates {
		if c.Provider != "openai" {
			t.Errorf("cross-provider model %s/%s should not be injected as same-provider alt", c.Provider, c.Model)
		}
	}
}
