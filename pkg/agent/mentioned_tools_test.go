package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// serverNamedCountingTool is an MCP-like tool the model may call by the
// server's own name (skip_project_status) as well as the registry name.
type serverNamedCountingTool struct {
	countingTool
	serverName string
}

func (s *serverNamedCountingTool) ServerToolName() string { return s.serverName }

func newServerTool(name, serverName string) *serverNamedCountingTool {
	return &serverNamedCountingTool{countingTool: countingTool{name: name}, serverName: serverName}
}

// discoveryExchange is a tool_search call and its result listing names.
func discoveryExchange(t *testing.T, id string, names ...string) []providers.Message {
	t.Helper()
	listed := make([]tools.ToolSearchResult, len(names))
	for i, name := range names {
		listed[i] = tools.ToolSearchResult{Name: name, Description: "[MCP:skip] test"}
	}
	body, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal discovery result: %v", err)
	}
	return []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{
			ID: id, Type: "function", Function: &providers.FunctionCall{Name: tools.BM25SearchToolName},
		}}},
		{Role: "tool", ToolCallID: id, Content: fmt.Sprintf("Found %d tools:\n%s", len(names), body)},
	}
}

func TestMentionedExpiredToolNames(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterHidden(newServerTool(statusToolName, "skip_project_status"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_cloud_list_logs", "skip_cloud_list_logs"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_cloud_list_migrations", "skip_cloud_list_migrations"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_file_read", "skip_file_read"))
	registry.RegisterHidden(newServerTool("mcp_drive_files_get", "files.get"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_project_delete", "skip_project_delete"))
	registry.RegisterHidden(newServerTool(createToolName, "skip_project_create"))
	registry.PromoteTools([]string{createToolName}, 5)

	messages := discoveryExchange(t, "s1",
		statusToolName, "mcp_skip_skip_cloud_list_logs", "mcp_skip_skip_cloud_list_migrations",
		"mcp_skip_skip_file_read", "mcp_drive_files_get", createToolName)
	messages = append(messages,
		// Fourth assistant message with text from the end: out of the window.
		providers.Message{Role: "assistant", Content: "vou rodar skip_cloud_list_migrations"},
		providers.Message{Role: "assistant", ReasoningContent: "plano: `skip_cloud_list_logs` e depois files.get."},
		providers.Message{Role: "tool", ToolCallID: "c1", Content: "skip_file_read output is not intent"},
		providers.Message{Role: "user", Content: "usa o mcp_skip_skip_file_read"},
		// Never listed by a tool_search: not an expired promotion.
		providers.Message{Role: "assistant", Content: "não chamo skip_project_create nem skip_project_delete"},
		// Tool calls only, no text: doesn't use up the window.
		providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c2", Name: "exec"}}},
		providers.Message{
			Role:             "assistant",
			ReasoningContent: "ler com skip_project_status(40235) e mcp_skip_skip_cloud_list_logs",
			Content:          "Lendo o projeto agora.",
		},
	)

	got := strings.Join(mentionedExpiredToolNames(messages, registry), ",")
	want := statusToolName + ",mcp_skip_skip_cloud_list_logs,mcp_drive_files_get"
	if got != want {
		t.Errorf(
			"mentioned = %s, want %s (newest first; live, undiscovered, user/tool text and older mentions ignored)",
			got,
			want,
		)
	}
}

// siblingMisfireProvider mimics the prod model: it wants the read tool; while
// that tool is not offered it emits the closest one that is.
type siblingMisfireProvider struct{}

func (siblingMisfireProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" {
			return &providers.LLMResponse{Content: "versão 10e62f0, árvore limpa"}, nil
		}
	}
	offered := map[string]bool{}
	for _, d := range defs {
		offered[d.Function.Name] = true
	}
	for _, name := range []string{statusToolName, createToolName} {
		if offered[name] {
			return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
				ID: "call-1", Type: "function", Name: name, Arguments: map[string]any{},
			}}}, nil
		}
	}
	return &providers.LLMResponse{Content: "sem ferramenta"}, nil
}

func (siblingMisfireProvider) GetDefaultModel() string { return "mock-model" }

func TestPlannedToolThatExpiredIsOfferedAgain(t *testing.T) {
	status := newServerTool(statusToolName, "skip_project_status")
	create := newServerTool(createToolName, "skip_project_create")
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), siblingMisfireProvider{})
	agent := al.registry.GetDefaultAgent()
	agent.Tools.RegisterHidden(status)
	agent.Tools.RegisterHidden(create)
	// Both were listed by a tool_search; the read tool's promotion expired,
	// the create tool is still live.
	agent.Tools.PromoteTools([]string{createToolName}, 5)

	key := directSessionKey(al)
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "user", Content: "confere o projeto 40235"})
	for _, m := range discoveryExchange(t, "search-1", statusToolName, createToolName) {
		agent.Sessions.AddFullMessage(key, m)
	}
	agent.Sessions.AddFullMessage(key, providers.Message{
		Role:             "assistant",
		ReasoningContent: "Leitura em lote: skip_project_status(40235) primeiro.",
		Content:          "Vou ler o 40235, somente leitura.",
	})

	if _, err := al.ProcessDirect(context.Background(), "continue", "mention-heal"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := create.calls.Load(); got != 0 {
		t.Fatalf("create ran %d times: the planned read tool was not offered", got)
	}
	if got := status.calls.Load(); got != 1 {
		t.Fatalf("status ran %d times, want 1", got)
	}
}

func TestMentionsMatchWhateverTheCase(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterHidden(newServerTool(statusToolName, "skip_project_status"))
	messages := append(discoveryExchange(t, "s1", statusToolName),
		providers.Message{Role: "assistant", Content: "Lendo com SKIP_PROJECT_STATUS agora."})

	if got := mentionedExpiredToolNames(messages, registry); len(got) != 1 || got[0] != statusToolName {
		t.Fatalf("mentioned = %v, want [%s]", got, statusToolName)
	}
}

// offerRecorder makes a few rounds of a harmless call and records which tools
// each round offered.
type offerRecorder struct {
	rounds  int
	offered map[string]bool
}

func (p *offerRecorder) Chat(
	_ context.Context,
	_ []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	for _, d := range defs {
		p.offered[d.Function.Name] = true
	}
	if p.rounds == 0 {
		return &providers.LLMResponse{Content: "fim"}, nil
	}
	p.rounds--
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID: fmt.Sprintf("noop-%d", p.rounds), Type: "function", Name: "noop", Arguments: map[string]any{},
	}}}, nil
}

func (*offerRecorder) GetDefaultModel() string { return "mock-model" }

// A text naming more tools than a discovery page revives one page per turn,
// not one per round.
func TestNamedToolsAreRevivedOnePagePerTurn(t *testing.T) {
	const named = maxRevivedDiscoveredTools + 4
	provider := &offerRecorder{rounds: 3, offered: map[string]bool{}}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	agent.Tools.Register(&countingTool{name: "noop"})
	names := make([]string, named)
	for i := range names {
		names[i] = fmt.Sprintf("mcp_lib_tool_%02d", i)
		agent.Tools.RegisterHidden(newServerTool(names[i], fmt.Sprintf("tool_%02d", i)))
	}
	key := directSessionKey(al)
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "user", Content: "usa a biblioteca"})
	for _, m := range discoveryExchange(t, "search-1", names...) {
		agent.Sessions.AddFullMessage(key, m)
	}
	agent.Sessions.AddFullMessage(key, providers.Message{
		Role: "assistant", Content: "Vou usar " + strings.Join(names, ", ") + ".",
	})

	if _, err := al.ProcessDirect(context.Background(), "continue", "mention-budget"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	offered := 0
	for _, name := range names {
		if provider.offered[name] {
			offered++
		}
	}
	if offered != maxRevivedDiscoveredTools {
		t.Fatalf("%d named tools offered over the turn, want %d", offered, maxRevivedDiscoveredTools)
	}
}

// The revival before a retry and the mention heal share one page per turn.
func TestRetryRevivalSharesTheTurnBudget(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &offerRecorder{offered: map[string]bool{}})
	agent := al.registry.GetDefaultAgent()
	names := make([]string, maxRevivedDiscoveredTools)
	for i := range names {
		names[i] = fmt.Sprintf("mcp_lib_tool_%02d", i)
		agent.Tools.RegisterHidden(newServerTool(names[i], fmt.Sprintf("tool_%02d", i)))
	}
	ts := &turnState{agent: agent, discoveryRevivals: maxRevivedDiscoveredTools - 3}
	exec := &turnExecution{messages: discoveryExchange(t, "search-1", names...)}

	NewPipeline(al).reviveDiscoveredTools(ts, exec, 1)

	visible := 0
	for _, name := range names {
		if _, ok := agent.Tools.Get(name); ok {
			visible++
		}
	}
	if visible != 3 {
		t.Fatalf("%d tools revived before the retry, want the 3 left in the turn's budget", visible)
	}
	if ts.discoveryRevivals != maxRevivedDiscoveredTools {
		t.Errorf("turn budget used = %d, want %d", ts.discoveryRevivals, maxRevivedDiscoveredTools)
	}
}
