package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
)

// The user is told to reconnect, and a reconnect changes nothing in the pod's
// config: readiness has to hold and the tools have to come back on their own.
func TestConnectorWithoutAccount_StaysReadyAndComesBackNextTurn(t *testing.T) {
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
	var toolName string
	for _, name := range agent.Tools.List() {
		if strings.HasSuffix(name, "list_messages") {
			toolName = name
		}
	}
	if toolName == "" {
		t.Fatalf("connector tool not registered: %v", agent.Tools.List())
	}

	result := agent.Tools.Execute(context.Background(), toolName, map[string]any{})

	if !strings.Contains(result.ForLLM, "connected again") {
		t.Fatalf("the MCP failure must reach the registry as a missing account: %s", result.ForLLM)
	}
	if agent.Tools.HasRegistered(toolName) {
		t.Fatal("the connector should be out for the rest of the turn")
	}
	if !al.ConnectorReady("gmail") {
		t.Fatal("a retired connector must not leave the connector flow stuck in applying")
	}

	if _, err := al.ProcessDirect(context.Background(), "next turn", "session-next-turn"); err != nil {
		t.Fatal(err)
	}
	if !agent.Tools.HasRegistered(toolName) {
		t.Fatal("a new turn must bring the connector back")
	}
}

func TestConnectorReady_RequiresHandshakeAndCurrentRegisteredTools(t *testing.T) {
	sdkServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	sdkmcp.AddTool(
		sdkServer,
		&sdkmcp.Tool{Name: "list_messages", Description: "List test messages"},
		func(context.Context, *sdkmcp.CallToolRequest, map[string]any) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{}, nil, nil
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
	cfg.Tools.MCP.Discovery.Enabled = true
	cfg.Tools.MCP.Discovery.UseBM25 = true
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{"gmail": {Enabled: true, Type: "http", URL: server.URL}}
	if al.ConnectorReady("gmail") {
		t.Fatal("config alone reported ready")
	}
	if err := al.ensureMCPInitialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !al.ConnectorReady("gmail") {
		t.Fatal("handshaken connector with discoverable tools was not ready")
	}
	if al.ConnectorReady("gcalendar") {
		t.Fatal("another connector inherited readiness")
	}
	cfg.Tools.MCP.Servers["gmail"] = config.MCPServerConfig{Enabled: false}
	if al.ConnectorReady("gmail") {
		t.Fatal("disabled connector inherited old connection")
	}
}
