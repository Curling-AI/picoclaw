package tools

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

type accountTool struct {
	name, server string
	safety       toolshared.RepeatSafety
	result       *ToolResult
	received     map[string]any
}

func (a *accountTool) Name() string        { return a.name }
func (a *accountTool) Description() string { return "" }
func (a *accountTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"known": map[string]any{"type": "string"}}}
}
func (a *accountTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	a.received = args
	return a.result
}
func (a *accountTool) MCPServerName() string                 { return a.server }
func (a *accountTool) RepeatSafety() toolshared.RepeatSafety { return a.safety }

func failedWith(signal toolshared.MCPAccountSignal) *ToolResult {
	result := ErrorResult("failed")
	result.MCPAccount = signal
	return result
}

func TestRegistry_MissingAccountTakesTheConnectorOut(t *testing.T) {
	reg := NewToolRegistry()
	gmail := &accountTool{name: "mcp_gmail_a", server: "gmail", result: failedWith(toolshared.MCPAccountMissing)}
	reg.Register(gmail)
	reg.Register(&accountTool{name: "mcp_gmail_b", server: "gmail", result: NewToolResult("ok")})
	reg.Register(&accountTool{name: "mcp_notion_a", server: "notion", result: NewToolResult("ok")})
	before := reg.Version()

	result := reg.Execute(context.Background(), "mcp_gmail_a", nil)

	if !strings.Contains(result.ForLLM, "connected again") {
		t.Fatalf("the model must learn what to tell the user: %s", result.ForLLM)
	}
	if reg.HasRegistered("mcp_gmail_a") || reg.HasRegistered("mcp_gmail_b") {
		t.Fatal("every tool of the connector must go")
	}
	if !reg.HasRegistered("mcp_notion_a") {
		t.Fatal("other connectors stay")
	}
	if reg.Version() == before {
		t.Fatal("the prompt cache must notice the tools changed")
	}
}

func TestRegistry_RejectionTakesTwoInARow(t *testing.T) {
	reg := NewToolRegistry()
	tool := &accountTool{name: "mcp_ig_a", server: "ig", result: failedWith(toolshared.MCPAccountRejected)}
	reg.Register(tool)

	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if !reg.HasRegistered("mcp_ig_a") {
		t.Fatal("one 401 may be a token about to refresh")
	}

	tool.result = NewToolResult("ok")
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	tool.result = failedWith(toolshared.MCPAccountRejected)
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if !reg.HasRegistered("mcp_ig_a") {
		t.Fatal("a success in between starts the count over")
	}

	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if reg.HasRegistered("mcp_ig_a") {
		t.Fatal("two 401s in a row take it out")
	}
}

func TestRegistry_UnknownArgs(t *testing.T) {
	args := func() map[string]any { return map[string]any{"known": "k", "stray": "s"} }

	reader := &accountTool{name: "mcp_x_read", server: "x", safety: toolshared.RepeatReadOnly, result: NewToolResult("ok")}
	writer := &accountTool{name: "mcp_x_send", server: "x", safety: toolshared.RepeatUnsafe, result: NewToolResult("ok")}
	reg := NewToolRegistry()
	reg.Register(reader)
	reg.Register(writer)

	result := reg.Execute(context.Background(), "mcp_x_read", args())
	if result.IsError || !strings.Contains(result.ForLLM, "stray") {
		t.Fatalf("a read drops the argument and says so: %s", result.ForLLM)
	}
	if _, leaked := reader.received["stray"]; leaked {
		t.Fatal("the dropped argument must not reach the tool")
	}

	result = reg.Execute(context.Background(), "mcp_x_send", args())
	if !result.IsError || !strings.Contains(result.ForLLM, `unexpected property "stray" (accepted: known)`) {
		t.Fatalf("a connector tool that changes things keeps refusing, naming what it accepts: %s", result.ForLLM)
	}
	if writer.received != nil {
		t.Fatal("the refused call must not run")
	}
}
