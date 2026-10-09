package agent

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	heldToolName    = "mcp_skip_skip_file_write"
	foreignToolName = "mcp_gmail_send_email"
)

// registryChurnProvider plays a turn of three calls and, between its calls,
// does to the shared registry what concurrent sessions do in prod: another
// conversation's tool_search promotes a tool, and their iterations tick the
// TTL until this turn's tool expires.
type registryChurnProvider struct {
	registry *tools.ToolRegistry
	mu       sync.Mutex
	toolSets [][]string
}

func (p *registryChurnProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	p.mu.Lock()
	p.toolSets = append(p.toolSets, names)
	n := len(p.toolSets)
	p.mu.Unlock()

	if n < 3 {
		p.registry.PromoteTools([]string{foreignToolName}, 5)
		for range 10 {
			p.registry.TickTTL()
		}
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "call_read",
			Type:      "function",
			Name:      "read_file",
			Arguments: map[string]any{"path": "README.md"},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "feito"}, nil
}

func (p *registryChurnProvider) GetDefaultModel() string { return "mock-model" }

func TestTurnToolsDoNotMoveWithOtherSessions(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &registryChurnProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	registry := al.registry.GetDefaultAgent().Tools
	provider.registry = registry
	registry.RegisterHidden(&countingTool{name: heldToolName})
	registry.RegisterHidden(&countingTool{name: foreignToolName})
	registry.PromoteTools([]string{heldToolName}, 5)

	if _, err := al.ProcessDirect(context.Background(), "edite o arquivo", "offered-tools-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if len(provider.toolSets) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(provider.toolSets))
	}
	first := provider.toolSets[0]
	if !slices.Contains(first, heldToolName) {
		t.Fatalf("first call did not offer the promoted tool: %v", first)
	}
	for i, set := range provider.toolSets[1:] {
		if !reflect.DeepEqual(set, first) {
			t.Errorf("call %d offered %v, want the same tools as the first call %v", i+2, set, first)
		}
	}
}

// The turn's own discovery still widens what it offers.
func TestTurnToolsGrowWithWhatTheTurnUses(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterHidden(&countingTool{name: heldToolName})
	registry.RegisterHidden(&countingTool{name: foreignToolName})
	registry.PromoteTools([]string{heldToolName}, 5)

	ts := &turnState{}
	ts.seedOfferedTools(registry)
	registry.PromoteTools([]string{foreignToolName}, 5)
	ts.seedOfferedTools(registry) // only the first seed counts

	names := func() []string {
		defs := registry.ToProviderDefsFor(ts.offeredToolSet())
		out := make([]string, 0, len(defs))
		for _, d := range defs {
			out = append(out, d.Function.Name)
		}
		return out
	}
	if got := names(); !slices.Equal(got, []string{heldToolName}) {
		t.Fatalf("offered = %v, want only the tool visible at the start", got)
	}
	ts.offerTools([]string{foreignToolName})
	if got := names(); !slices.Equal(got, []string{foreignToolName, heldToolName}) {
		t.Fatalf("offered = %v, want both after the turn used the second one", got)
	}
}

// searchAfterOtherSessionProvider: another session promotes a tool after this
// turn started, then this turn searches for it and is told it is unlocked.
type searchAfterOtherSessionProvider struct {
	registry *tools.ToolRegistry
	mu       sync.Mutex
	toolSets [][]string
}

func (p *searchAfterOtherSessionProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	p.mu.Lock()
	p.toolSets = append(p.toolSets, names)
	n := len(p.toolSets)
	p.mu.Unlock()
	if n == 1 {
		p.registry.PromoteTools([]string{foreignToolName}, 5)
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID: "search", Type: "function", Name: tools.RegexSearchToolName,
			Arguments: map[string]any{"pattern": "gmail"},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "ok"}, nil
}

func (p *searchAfterOtherSessionProvider) GetDefaultModel() string { return "mock-model" }

// Local review (MST-277): a before/after diff of the registry missed a tool another
// session had already promoted, so the search said "unlocked" and the next
// call still did not offer it. The turn now offers what the search returned.
func TestTurnToolsOfferWhatTheTurnsSearchReturned(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &searchAfterOtherSessionProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	registry := al.registry.GetDefaultAgent().Tools
	provider.registry = registry
	registry.Register(tools.NewRegexSearchTool(registry, 5, 8))
	registry.RegisterHidden(&countingTool{name: foreignToolName})

	if _, err := al.ProcessDirect(context.Background(), "mande o email", "search-offered-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if len(provider.toolSets) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.toolSets))
	}
	if slices.Contains(provider.toolSets[0], foreignToolName) {
		t.Fatalf("the first call already offered %s", foreignToolName)
	}
	if !slices.Contains(provider.toolSets[1], foreignToolName) {
		t.Errorf("the search unlocked %s but the next call did not offer it: %v", foreignToolName, provider.toolSets[1])
	}
}
