package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type connectorTestTool struct {
	*mcpTestTool
	signal tools.MCPAccountSignal
}

func (c connectorTestTool) MCPServerName() string { return "gmail" }

func (c connectorTestTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	c.calls.Add(1)
	result := tools.ErrorResult("failed")
	result.MCPAccount = c.signal
	return result
}

// connectorOfferRecorder keeps the tools each round was offered.
type connectorOfferRecorder struct {
	stepProvider
	offered [][]string
	last    []providers.Message
}

func (r *connectorOfferRecorder) Chat(
	ctx context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Function.Name)
	}
	r.offered = append(r.offered, names)
	r.last = messages
	return r.stepProvider.Chat(ctx, messages, defs, model, options)
}

func runConnectorTurn(
	t *testing.T,
	signal tools.MCPAccountSignal,
	steps [][]stepCall,
) (*AgentLoop, *connectorOfferRecorder, connectorTestTool) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	recorder := &connectorOfferRecorder{stepProvider: stepProvider{steps: steps}}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), recorder)
	tool := connectorTestTool{mcpTestTool: newMCPTestTool("mcp_gmail_send", tools.RepeatReadOnly), signal: signal}
	al.registry.GetDefaultAgent().Tools.Register(tool)
	if _, err := al.ProcessDirect(context.Background(), "go", "connector-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	return al, recorder, tool
}

func TestMissingAccountTakesTheConnectorOutForTheTurnOnly(t *testing.T) {
	call := stepCall{tool: "mcp_gmail_send"}
	al, recorder, tool := runConnectorTurn(t, tools.MCPAccountMissing, oneCallPerStep(call, call))

	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("the second call must not reach the connector, got %d calls", got)
	}
	if !slices.Contains(recorder.offered[0], "mcp_gmail_send") ||
		slices.Contains(recorder.offered[1], "mcp_gmail_send") {
		t.Fatalf("the tool must stop being offered after the failure: %v", recorder.offered)
	}
	if !al.registry.GetDefaultAgent().Tools.HasRegistered("mcp_gmail_send") {
		t.Fatal("the shared registry serves other sessions and readiness: it must not change")
	}

	recorder.offered = nil
	if _, err := al.ProcessDirect(context.Background(), "connected now", "connector-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if !slices.Contains(recorder.offered[0], "mcp_gmail_send") {
		t.Fatalf("the next turn must offer it again, a reconnect does not reach the pod: %v", recorder.offered)
	}
}

func TestRetiredConnectorStaysOutOfDiscovery(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	recorder := &connectorOfferRecorder{stepProvider: stepProvider{steps: oneCallPerStep(
		stepCall{tool: "mcp_gmail_send"},
		stepCall{tool: tools.RegexSearchToolName, args: map[string]any{"pattern": "gmail"}},
	)}}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), recorder)
	registry := al.registry.GetDefaultAgent().Tools
	registry.Register(tools.NewRegexSearchTool(registry, 5, 5))
	registry.RegisterHidden(connectorTestTool{
		mcpTestTool: newMCPTestTool("mcp_gmail_send", tools.RepeatReadOnly), signal: tools.MCPAccountMissing,
	})
	if _, err := al.ProcessDirect(context.Background(), "go", "connector-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	var searched string
	for _, message := range recorder.last {
		if message.Role == "tool" {
			searched = message.Content
		}
	}
	if !strings.Contains(searched, "No tools found") {
		t.Fatalf("the search must not announce the retired connector: %q", searched)
	}
}

func TestRejectionsCountOncePerRound(t *testing.T) {
	call := stepCall{tool: "mcp_gmail_send"}
	_, _, tool := runConnectorTurn(t, tools.MCPAccountRejected, [][]stepCall{{call, call}, {call}, {call}})

	if got := tool.calls.Load(); got != 3 {
		t.Fatalf("two parallel 401s are one strike, the next round the second: want 3 calls, got %d", got)
	}
}

func TestConnectorAccountsSuccessResetsTheCount(t *testing.T) {
	var accounts connectorAccounts
	rejected := &tools.ToolResult{IsError: true, MCPAccount: tools.MCPAccountRejected}

	if accounts.note("gmail", rejected, 1) {
		t.Fatal("one 401 may be a token about to refresh")
	}
	accounts.note("gmail", tools.NewToolResult("ok"), 2)
	if accounts.note("gmail", rejected, 3) {
		t.Fatal("a success in between starts the count over")
	}
	if !accounts.note("gmail", rejected, 4) || !accounts.isRetired("gmail") {
		t.Fatal("two 401s in a row take it out")
	}
	if accounts.isRetired("notion") || accounts.isRetired("") {
		t.Fatal("other connectors and other tools stay")
	}
}

// The failure of a real MCP server has to reach the turn as a missing account.
func TestMCPFailureCarriesTheAccountSignal(t *testing.T) {
	sdkServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	sdkmcp.AddTool(
		sdkServer,
		&sdkmcp.Tool{Name: "list_messages", Description: "List test messages"},
		func(context.Context, *sdkmcp.CallToolRequest, map[string]any) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "No connected account found for toolkit gmail"}},
			}, nil, nil
		},
	)
	server := httptest.NewServer(
		sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return sdkServer }, nil),
	)
	defer server.Close()
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	defer al.Close()
	cfg.Tools.MCP.Enabled = true
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{"gmail": {Enabled: true, Type: "http", URL: server.URL}}
	if err := al.ensureMCPInitialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent := al.GetRegistry().GetDefaultAgent()
	index := slices.IndexFunc(
		agent.Tools.List(),
		func(name string) bool { return strings.HasSuffix(name, "list_messages") },
	)
	if index < 0 {
		t.Fatalf("connector tool not registered: %v", agent.Tools.List())
	}

	result := agent.Tools.Execute(context.Background(), agent.Tools.List()[index], map[string]any{})

	if result.MCPAccount != tools.MCPAccountMissing {
		t.Fatalf("got signal %v: %s", result.MCPAccount, result.ForLLM)
	}
	if !al.ConnectorReady("gmail") {
		t.Fatal("a failed call must not leave the connector flow stuck in applying")
	}
}
