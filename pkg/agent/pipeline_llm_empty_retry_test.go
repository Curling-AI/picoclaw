package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// emptyThenContentProvider returns an empty (no content, no tool calls)
// response on the first call and a real answer afterwards — the shape of the
// reasoning-only glitch observed in prod.
type emptyThenContentProvider struct {
	calls atomic.Int32
}

func (p *emptyThenContentProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{Content: ""}, nil
	}
	return &providers.LLMResponse{Content: "recovered answer"}, nil
}

func (p *emptyThenContentProvider) GetDefaultModel() string { return "mock-model" }

// alwaysEmptyProvider never produces content — the retry cap must end the
// turn with the default fallback instead of looping.
type alwaysEmptyProvider struct {
	calls atomic.Int32
}

func (p *alwaysEmptyProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{Content: ""}, nil
}

func (p *alwaysEmptyProvider) GetDefaultModel() string { return "mock-model" }

func TestEmptyLLMResponseIsRetriedOnce(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &emptyThenContentProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "hello", "empty-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "recovered answer" {
		t.Errorf("response = %q, want the retried answer", resp)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (original + one retry)", got)
	}
}

func TestEmptyLLMResponseRetryIsCapped(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysEmptyProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "hello", "empty-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp == "recovered answer" {
		t.Fatalf("unexpected content from always-empty provider")
	}
	if got := provider.calls.Load(); got != 1+maxEmptyResponseRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxEmptyResponseRetries)
	}
}
