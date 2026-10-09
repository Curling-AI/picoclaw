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
