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

	reg.TickTTL()
	tool.result = NewToolResult("ok")
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	reg.TickTTL()
	tool.result = failedWith(toolshared.MCPAccountRejected)
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if !reg.HasRegistered("mcp_ig_a") {
		t.Fatal("a success in between starts the count over")
	}

	reg.TickTTL()
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if reg.HasRegistered("mcp_ig_a") {
		t.Fatal("two 401s in a row take it out")
	}
}

func TestRegistry_ParallelRejectionsCountOnce(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&accountTool{name: "mcp_ig_a", server: "ig", result: failedWith(toolshared.MCPAccountRejected)})

	reg.Execute(context.Background(), "mcp_ig_a", nil)
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if !reg.HasRegistered("mcp_ig_a") {
		t.Fatal("calls of one round all see the same token")
	}

	reg.TickTTL()
	reg.Execute(context.Background(), "mcp_ig_a", nil)
	if reg.HasRegistered("mcp_ig_a") {
		t.Fatal("the next round is the second strike")
	}
}

func TestRegistry_RetiredConnectorComesBackNextTurn(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&accountTool{name: "mcp_gmail_a", server: "gmail", result: failedWith(toolshared.MCPAccountMissing)})
	reg.Execute(context.Background(), "mcp_gmail_a", nil)
	if !reg.IsMCPServerRetired("gmail") {
		t.Fatal("readiness must still see the connector as registered")
	}
	before := reg.Version()

	reg.RestoreRetiredMCP()

	if !reg.HasRegistered("mcp_gmail_a") || reg.IsMCPServerRetired("gmail") {
		t.Fatal("a reconnect does not reach the pod, so the tools must come back on their own")
	}
	if reg.Version() == before {
		t.Fatal("the prompt cache must notice the tools changed")
	}
}

// A reload that registers the connector again wins over the retired entry.
func TestRegistry_RestoreKeepsAToolRegisteredAgain(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&accountTool{name: "mcp_gmail_a", server: "gmail", result: failedWith(toolshared.MCPAccountMissing)})
	reg.Execute(context.Background(), "mcp_gmail_a", nil)
	fresh := &accountTool{name: "mcp_gmail_a", server: "gmail", result: NewToolResult("ok")}
	reg.Register(fresh)

	reg.RestoreRetiredMCP()

	if got, _ := reg.GetRegistered("mcp_gmail_a"); got != Tool(fresh) {
		t.Fatal("the retired entry replaced the newer one")
	}
}

type namedNativeTool struct{ name string }

func (n namedNativeTool) Name() string               { return n.name }
func (n namedNativeTool) Description() string        { return "" }
func (n namedNativeTool) Parameters() map[string]any { return nil }
func (n namedNativeTool) Execute(context.Context, map[string]any) *ToolResult {
	return NewToolResult("ok")
}

func TestDropsUnknownArgs_OnlyDeclaredReadOnlyToolsDrop(t *testing.T) {
	for _, name := range []string{"exec", "memory", "write_file", "edit_file", "message", "cron", "spawn"} {
		if dropsUnknownArgs(namedNativeTool{name}) {
			t.Errorf("%s must refuse: the stray argument can carry intent", name)
		}
	}
	reader := &accountTool{name: "reader", safety: toolshared.RepeatReadOnly}
	if !dropsUnknownArgs(reader) {
		t.Error("a tool that declares itself read-only should drop a stray argument")
	}
}

func TestRegistry_UnknownArgs(t *testing.T) {
	args := func() map[string]any { return map[string]any{"known": "k", "stray": "s"} }

	reader := &accountTool{
		name:   "mcp_x_read",
		server: "x",
		safety: toolshared.RepeatReadOnly,
		result: NewToolResult("ok"),
	}
	writer := &accountTool{
		name:   "mcp_x_send",
		server: "x",
		safety: toolshared.RepeatUnsafe,
		result: NewToolResult("ok"),
	}
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
