package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
)

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
