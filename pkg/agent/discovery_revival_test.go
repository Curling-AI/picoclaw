package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	sqlToolName = "mcp_nekt_execute_sql"
	ddlToolName = "mcp_nekt_get_relevant_tables_ddl"

	// Verbatim from greenhouse (2026-09-28): the turn ended here, nothing ran.
	announcedSQLRun = "Rodando — duas queries em paralelo (spend/requests/cache + active users):"

	// A discovery result as persisted before this change (old wording).
	legacyDiscoveryResult = "Found 2 tools:\n" +
		`[{"name":"mcp_nekt_generate_sql","description":"[MCP:nekt] Generate a SQL query"},` +
		`{"name":"mcp_nekt_execute_sql","description":"[MCP:nekt] Execute a SQL query"}]` +
		"\n\nSUCCESS: These tools have been temporarily UNLOCKED as native tools! " +
		"In your next response, you can call them directly just like any normal tool"
)

type countingTool struct {
	name  string
	calls atomic.Int32
}

func (c *countingTool) Name() string        { return c.name }
func (c *countingTool) Description() string { return "[MCP:nekt] test tool" }
func (c *countingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (c *countingTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	c.calls.Add(1)
	return tools.NewToolResult(`{"status":"succeeded"}`)
}

// stuckWithoutToolProvider mimics the prod model: while the tool it wants is
// not offered it replies with stuckReply; once offered it calls it.
type stuckWithoutToolProvider struct {
	want       string
	stuckReply string
	calls      atomic.Int32
}

func (p *stuckWithoutToolProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	// Transient nudges ride after the tool result, so look it up by call id.
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" {
			return &providers.LLMResponse{Content: "spend de setembro: $1.2M"}, nil
		}
	}
	for _, d := range defs {
		if d.Function.Name == p.want {
			return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
				ID:        "call-1",
				Type:      "function",
				Name:      p.want,
				Arguments: map[string]any{},
			}}}, nil
		}
	}
	return &providers.LLMResponse{Content: p.stuckReply}, nil
}

func (p *stuckWithoutToolProvider) GetDefaultModel() string { return "mock-model" }

// newSessionWithExpiredDiscovery reproduces the stuck session: a tool_search
// listed the tool long ago, it was never called, and its promotion expired.
func newSessionWithExpiredDiscovery(t *testing.T, provider providers.LLMProvider, tool *countingTool) *AgentLoop {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	agent.Tools.RegisterHidden(tool)

	key := directSessionKey(al)
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "user", Content: "ache os custos de LLM no Nekt"})
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{
		ID:       "search-1",
		Type:     "function",
		Function: &providers.FunctionCall{Name: tools.BM25SearchToolName, Arguments: `{"query":"nekt sql"}`},
	}}})
	agent.Sessions.AddFullMessage(key, providers.Message{
		Role:       "tool",
		ToolCallID: "search-1",
		Content:    legacyDiscoveryResult,
	})
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "assistant", Content: "Tabelas mapeadas."})
	return al
}

func TestAnnounceRetryRevivesDiscoveredButNeverCalledTool(t *testing.T) {
	tool := &countingTool{name: sqlToolName}
	provider := &stuckWithoutToolProvider{want: sqlToolName, stuckReply: announcedSQLRun}
	al := newSessionWithExpiredDiscovery(t, provider, tool)

	resp, err := al.ProcessDirect(context.Background(), "roda", "revival-announce")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("tool ran %d times, want 1 — the retry went out without the tool", got)
	}
	if !strings.Contains(resp, "$1.2M") {
		t.Errorf("response = %q, want the answer built on the tool result", resp)
	}
	for _, m := range directSessionHistory(t, al) {
		if strings.Contains(m.Content, "available again") {
			t.Errorf("the revival nudge was persisted: %q", m.Content)
		}
	}
}

func TestEmptyRetryRevivesDiscoveredButNeverCalledTool(t *testing.T) {
	tool := &countingTool{name: sqlToolName}
	provider := &stuckWithoutToolProvider{want: sqlToolName, stuckReply: ""}
	al := newSessionWithExpiredDiscovery(t, provider, tool)

	if _, err := al.ProcessDirect(context.Background(), "roda", "revival-empty"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("tool ran %d times, want 1 — the retry went out without the tool", got)
	}
}

// The model named an expired tool directly; answering "tool not found" wastes
// the round and teaches it the tool is gone.
func TestCallToExpiredDeferredToolIsExecuted(t *testing.T) {
	tool := &countingTool{name: ddlToolName}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &callOnceProvider{tool: ddlToolName}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.registry.GetDefaultAgent().Tools.RegisterHidden(tool)

	if _, err := al.ProcessDirect(context.Background(), "pega o DDL", "revival-direct-call"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("expired tool ran %d times, want 1", got)
	}
	if strings.Contains(provider.lastToolResult, "not found") {
		t.Errorf("model was told the tool does not exist: %q", provider.lastToolResult)
	}
}

type callOnceProvider struct {
	tool           string
	lastToolResult string
}

func (p *callOnceProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if last := messages[len(messages)-1]; last.Role == "tool" {
		p.lastToolResult = last.Content
		return &providers.LLMResponse{Content: "done"}, nil
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID:        "call-1",
		Type:      "function",
		Name:      p.tool,
		Arguments: map[string]any{},
	}}}, nil
}

func (p *callOnceProvider) GetDefaultModel() string { return "mock-model" }

func TestDiscoveredToolNamesFromMessages(t *testing.T) {
	search := func(id string) providers.Message {
		return providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{
			ID: id, Function: &providers.FunctionCall{Name: tools.BM25SearchToolName},
		}}}
	}
	result := func(id, names string) providers.Message {
		return providers.Message{Role: "tool", ToolCallID: id, Content: "Found 2 tools:\n" + names}
	}
	messages := []providers.Message{
		search("s1"),
		result("s1", `[{"name":"old_a"},{"name":"shared"}]`),
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "x1", Name: "exec"}}},
		// Same shape, but it answers exec, not a discovery call.
		result("x1", `[{"name":"not_discovered"},{"name":"other"}]`),
		search("s2"),
		result("s2", `[{"name":"new_a"},{"name":"shared"}]`),
	}

	got := strings.Join(discoveredToolNamesFromMessages(messages), ",")
	if want := "new_a,shared,old_a"; got != want {
		t.Errorf("discovered = %s, want %s (newest first, deduped, discovery results only)", got, want)
	}
}

// promotedElsewhereProvider is stuckWithoutToolProvider in a session where,
// right after this turn seeded its tools, another conversation's tool_search
// promotes the same tool: its TTL is above zero, but the turn does not offer it.
type promotedElsewhereProvider struct {
	stuckWithoutToolProvider
	registry *tools.ToolRegistry
	once     sync.Once
}

func (p *promotedElsewhereProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.once.Do(func() { p.registry.PromoteTools([]string{p.want}, 5) })
	return p.stuckWithoutToolProvider.Chat(ctx, messages, defs, model, opts)
}

// The heal only offered what ReviveExpired returned (TTL
// <= 0), so a tool promoted by another session after this turn seeded was
// never offered and the retry resent the same tools array.
func TestAnnounceRetryOffersADiscoveredToolPromotedElsewhere(t *testing.T) {
	tool := &countingTool{name: sqlToolName}
	provider := &promotedElsewhereProvider{
		stuckWithoutToolProvider: stuckWithoutToolProvider{want: sqlToolName, stuckReply: announcedSQLRun},
	}
	al := newSessionWithExpiredDiscovery(t, provider, tool)
	provider.registry = al.registry.GetDefaultAgent().Tools

	if _, err := al.ProcessDirect(context.Background(), "roda", "revival-promoted-elsewhere"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("tool ran %d times, want 1 — the retry went out without the tool", got)
	}
}

// The cap and the nudge count only what the turn does not offer yet: telling
// the model an offered tool was missing is false, and it spends the cap.
func TestReviveDiscoveredTools_NamesOnlyWhatTheTurnLacks(t *testing.T) {
	al := newSessionWithExpiredDiscovery(t, &countingReplyProvider{}, &countingTool{name: sqlToolName})
	agent := al.registry.GetDefaultAgent()
	agent.Tools.RegisterHidden(&countingTool{name: "mcp_nekt_generate_sql"})
	ts := newTurnState(agent, processOptions{}, turnEventScope{})
	ts.offerTools([]string{"mcp_nekt_generate_sql"})
	exec := &turnExecution{messages: agent.Sessions.GetHistory(directSessionKey(al))}

	NewPipeline(al).reviveDiscoveredTools(ts, exec, 1)

	if len(exec.transientTurnMessages) != 1 {
		t.Fatalf("nudges = %d, want 1", len(exec.transientTurnMessages))
	}
	nudge := exec.transientTurnMessages[0].Content
	if !strings.Contains(nudge, sqlToolName) || strings.Contains(nudge, "mcp_nekt_generate_sql") {
		t.Fatalf("nudge %q, want only the tool the turn lacked", nudge)
	}
	if _, ok := ts.offeredToolSet()[sqlToolName]; !ok {
		t.Fatal("the lacking tool was not offered")
	}
}
