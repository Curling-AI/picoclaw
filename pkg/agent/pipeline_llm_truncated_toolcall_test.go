package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// truncatedToolCallTail is the shape prod delivers when the gateway's tool
// parser matches the opening tags, gives up, and forwards the rest as text.
// The function name is gone with the prefix — nothing here is recoverable.
const truncatedToolCallTail = "<<<<<<< SEARCH\n  }, [contribuicoes, avaliacoes, params])\n=======\n" +
	"  }, [contribuicoes, params])\n>>>>>>> REPLACE</parameter>\n" +
	"<parameter=path>src/pages/Gestao.tsx</parameter>\n</function>\n</tool_call>"

type truncatedThenAnswerProvider struct {
	calls    atomic.Int32
	sawNudge atomic.Bool
}

func (p *truncatedThenAnswerProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{Content: truncatedToolCallTail}, nil
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "tool call written out as text") {
			p.sawNudge.Store(true)
		}
	}
	return &providers.LLMResponse{Content: "patch applied"}, nil
}

func (p *truncatedThenAnswerProvider) GetDefaultModel() string { return "mock-model" }

type alwaysTruncatedProvider struct {
	calls atomic.Int32
}

func (p *alwaysTruncatedProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{Content: truncatedToolCallTail}, nil
}

func (p *alwaysTruncatedProvider) GetDefaultModel() string { return "mock-model" }

func TestTruncatedToolCallIsRetriedWithANudge(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &truncatedThenAnswerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "remove as avaliações", "truncated-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "patch applied" {
		t.Errorf("response = %q, want the retried answer", resp)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (original + one retry)", got)
	}
	if !provider.sawNudge.Load() {
		t.Error("the retry went out without the corrective nudge")
	}
}

func TestTruncatedToolCallTextIsNeverDelivered(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysTruncatedProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "remove as avaliações", "truncated-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if strings.Contains(resp, "</tool_call>") || strings.Contains(resp, "<parameter=") {
		t.Errorf("raw tool-call markup reached the user: %q", resp)
	}
	if got := provider.calls.Load(); got != 1+maxTruncatedToolCallRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxTruncatedToolCallRetries)
	}

	// Neither the markup nor the correction may be persisted: the first
	// teaches the model that prose calls are normal here, the second reads as
	// a standing instruction in every later turn.
	history := directSessionHistory(t, al)
	// Guard the guard: an empty history would make the loop below pass without
	// checking anything.
	if len(history) == 0 {
		t.Fatal("session history is empty — the assertions below would be vacuous")
	}
	for _, m := range history {
		if strings.Contains(m.Content, "</tool_call>") {
			t.Errorf("tool-call markup persisted to history: %q", m.Content)
		}
		if strings.Contains(m.Content, "tool call written out as text") {
			t.Errorf("the corrective nudge was persisted: %q", m.Content)
		}
	}
}

// glmCutValue is the GLM shape that reached a user as the final answer
// (2026-10-09): the reply IS the rest of a call, opening on a bare value.
const glmCutValue = "<arg_value>pocketbase/migrations/0001"

// truncatedAroundAToolCallProvider breaks twice in one turn with a real call
// in between: the second glitch must get its own retry instead of ending the
// turn with the fallback message.
type truncatedAroundAToolCallProvider struct {
	calls       atomic.Int32
	nudgeOnCall [5]bool
}

func (p *truncatedAroundAToolCallProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	n := p.calls.Add(1)
	if int(n) < len(p.nudgeOnCall) {
		for _, m := range messages {
			if strings.Contains(m.Content, "tool call written out as text") {
				p.nudgeOnCall[n] = true
			}
		}
	}
	switch n {
	case 1, 3:
		return &providers.LLMResponse{Content: glmCutValue}, nil
	case 2:
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "call_read",
			Type:      "function",
			Name:      "read_file",
			Arguments: map[string]any{"path": "README.md"},
		}}}, nil
	default:
		return &providers.LLMResponse{Content: "migration criada"}, nil
	}
}

func (p *truncatedAroundAToolCallProvider) GetDefaultModel() string { return "mock-model" }

func TestTruncatedToolCallRetryStartsOverAfterARealCall(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &truncatedAroundAToolCallProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "crie a migration", "truncated-reset-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "migration criada" {
		t.Errorf("response = %q, want the answer after the second retry", resp)
	}
	if got := provider.calls.Load(); got != 4 {
		t.Errorf("provider calls = %d, want 4 (glitch, retry with a call, glitch, retry)", got)
	}
	if !provider.nudgeOnCall[2] {
		t.Error("the first retry went out without the nudge")
	}
	if provider.nudgeOnCall[3] {
		t.Error("the nudge kept riding along after the call it asked for went through")
	}
	if !provider.nudgeOnCall[4] {
		t.Error("the second glitch was not retried with the nudge")
	}
}
