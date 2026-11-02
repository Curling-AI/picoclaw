package integrationtools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

func TestMCPDirectExecutionUsesCanonicalServerIdentityAndRechecksAuthority(t *testing.T) {
	calls, approved, observed := 0, 0, 0
	manager := &MockMCPManager{callToolFunc: func(context.Context, string, string, map[string]any) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{}, nil
	}}
	allowed := NewMCPTool(manager, "mail", &mcp.Tool{Name: "send", InputSchema: map[string]any{"type": "object"}})
	other := NewMCPTool(manager, "mail_other", &mcp.Tool{Name: "send", InputSchema: map[string]any{"type": "object"}})
	revoked := false
	policy := toolshared.ExecutionPolicy{AllowsTool: func(tool toolshared.Tool) bool {
		server, ok := tool.(interface{ MCPServerName() string })
		return ok && server.MCPServerName() == "mail"
	}, BeforeTool: func(ctx context.Context, _ toolshared.Tool) (context.Context, error) {
		approved++
		if revoked {
			return ctx, context.Canceled
		}
		return ctx, nil
	}, AfterTool: func(context.Context, toolshared.Tool, *toolshared.ToolResult) { observed++ }}
	ctx := toolshared.WithExecutionPolicy(context.Background(), policy)
	if !other.Execute(ctx, map[string]any{}).IsError || calls != 0 {
		t.Fatal("server-name prefix bypassed restriction")
	}
	if allowed.Execute(ctx, map[string]any{}).IsError || calls != 1 || observed != 1 {
		t.Fatal("allowed direct invocation was not observed once")
	}
	revoked = true
	if !allowed.Execute(context.WithoutCancel(ctx), map[string]any{}).IsError || calls != 1 || approved != 2 {
		t.Fatal("revoked authority reached MCP provider")
	}
}
