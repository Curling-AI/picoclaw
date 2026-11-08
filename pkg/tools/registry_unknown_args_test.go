package tools

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

type hintedTool struct {
	name     string
	safety   toolshared.RepeatSafety
	result   *ToolResult
	received map[string]any
}

func (h *hintedTool) Name() string        { return h.name }
func (h *hintedTool) Description() string { return "" }
func (h *hintedTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"known": map[string]any{"type": "string"}}}
}

func (h *hintedTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	h.received = args
	return h.result
}
func (h *hintedTool) RepeatSafety() toolshared.RepeatSafety { return h.safety }

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
	reader := &hintedTool{name: "reader", safety: toolshared.RepeatReadOnly}
	if !dropsUnknownArgs(reader) {
		t.Error("a tool that declares itself read-only should drop a stray argument")
	}
}

func TestRegistry_UnknownArgs(t *testing.T) {
	args := func() map[string]any { return map[string]any{"known": "k", "stray": "s"} }

	reader := &hintedTool{
		name:   "mcp_x_read",
		safety: toolshared.RepeatReadOnly,
		result: NewToolResult("ok"),
	}
	writer := &hintedTool{
		name:   "mcp_x_send",
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
