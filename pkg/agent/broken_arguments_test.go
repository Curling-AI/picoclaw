package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// cutWriteProvider writes a file in one call whose JSON ran past the output
// cap, the way a subagent's 32,768-token reply arrived in prod, then reads
// what the agent answered.
type cutWriteProvider struct {
	maxTokens  int
	calls      atomic.Int32
	toolResult string
}

func (p *cutWriteProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{
			FinishReason: "tool_calls",
			Usage:        &providers.UsageInfo{CompletionTokens: p.maxTokens},
			ToolCalls: []providers.ToolCall{{
				ID:        "call_write",
				Type:      "function",
				Name:      "write_file",
				Arguments: map[string]any{"raw": `{"path":"Index.tsx","content":"export default function Ind`},
			}},
		}, nil
	}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call_write" {
			p.toolResult = m.Content
		}
	}
	return &providers.LLMResponse{Content: "vou escrever em partes"}, nil
}

func (p *cutWriteProvider) GetDefaultModel() string { return "mock-model" }

func TestCutOffToolArgumentsAreExplainedNotRun(t *testing.T) {
	cfg := config.DefaultConfig()
	workspace := t.TempDir()
	cfg.Agents.Defaults.Workspace = workspace
	provider := &cutWriteProvider{maxTokens: cfg.Agents.Defaults.MaxTokens}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	if _, err := al.ProcessDirect(context.Background(), "crie a página", "cut-args-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if !strings.Contains(provider.toolResult, "cut off at your output limit") {
		t.Errorf("tool result = %q, want the cut-off explanation", provider.toolResult)
	}
	if _, err := os.Stat(filepath.Join(workspace, "Index.tsx")); !os.IsNotExist(err) {
		t.Errorf("the cut call ran anyway (stat err = %v)", err)
	}
}

func TestBrokenArgumentsReason(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(&rawArgTool{})
	broken := map[string]any{"raw": `{"path":"a.md","content":"abc`}
	capped := &providers.LLMResponse{Usage: &providers.UsageInfo{CompletionTokens: 32768}}
	answered := &providers.LLMResponse{Usage: &providers.UsageInfo{CompletionTokens: 120}, FinishReason: "tool_calls"}

	if got := brokenArgumentsReason(
		registry,
		"write_file",
		broken,
		capped,
		32768,
	); !strings.Contains(
		got,
		"output limit",
	) {
		t.Errorf("at the cap: %q", got)
	}
	if got := brokenArgumentsReason(
		registry,
		"write_file",
		broken,
		answered,
		32768,
	); !strings.Contains(
		got,
		"not valid JSON",
	) {
		t.Errorf("under the cap: %q", got)
	}
	if got := brokenArgumentsReason(registry, rawArgToolName, broken, capped, 32768); got != "" {
		t.Errorf("a tool that takes a raw argument was refused: %q", got)
	}
	if got := brokenArgumentsReason(registry, "write_file", map[string]any{"path": "a.md"}, capped, 32768); got != "" {
		t.Errorf("decoded arguments were refused: %q", got)
	}
}

const rawArgToolName = "post_raw"

type rawArgTool struct{}

func (rawArgTool) Name() string        { return rawArgToolName }
func (rawArgTool) Description() string { return "posts a raw body" }
func (rawArgTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"raw": map[string]any{"type": "string"}}}
}

func (rawArgTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.NewToolResult("ok")
}
