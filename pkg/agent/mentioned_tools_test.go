package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const statusToolName = "mcp_skip_skip_project_status"

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

func TestMentionedExpiredToolNames(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterHidden(newServerTool(statusToolName, "skip_project_status"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_cloud_list_logs", "skip_cloud_list_logs"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_cloud_list_migrations", "skip_cloud_list_migrations"))
	registry.RegisterHidden(newServerTool("mcp_skip_skip_file_read", "skip_file_read"))
	registry.RegisterHidden(newServerTool(createToolName, "skip_project_create"))
	registry.PromoteTools([]string{createToolName}, 5)

	messages := []providers.Message{
		// Fourth assistant message from the end: out of the window.
		{Role: "assistant", Content: "vou rodar skip_cloud_list_migrations"},
		{Role: "assistant", ReasoningContent: "plano: `skip_cloud_list_logs` nos crons"},
		{Role: "tool", ToolCallID: "c1", Content: "skip_file_read results are tool output, not intent"},
		{Role: "user", Content: "usa o mcp_skip_skip_file_read"},
		{Role: "assistant", Content: "não chamo skip_project_create de novo"},
		{
			Role:             "assistant",
			ReasoningContent: "ler com skip_project_status(40235) e mcp_skip_skip_cloud_list_logs",
			Content:          "Lendo o MCP_SKIP_SKIP_PROJECT_STATUS agora.",
		},
	}

	got := strings.Join(mentionedExpiredToolNames(messages, registry), ",")
	want := statusToolName + ",mcp_skip_skip_cloud_list_logs"
	if got != want {
		t.Errorf(
			"mentioned = %s, want %s (newest first; live tools, user/tool text and older messages ignored)",
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
	// The read tool's promotion expired; the create tool is still live.
	agent.Tools.PromoteTools([]string{createToolName}, 5)

	key := directSessionKey(al)
	agent.Sessions.AddFullMessage(key, providers.Message{Role: "user", Content: "confere o projeto 40235"})
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
