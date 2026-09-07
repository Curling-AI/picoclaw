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

type loopThenAnswerProvider struct {
	calls    atomic.Int32
	sawNudge atomic.Bool
}

func (p *loopThenAnswerProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{Content: prodLoop}, nil
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "repeating the same phrase") {
			p.sawNudge.Store(true)
		}
	}
	return &providers.LLMResponse{Content: "encontrei três registros sobre o tema"}, nil
}

func (p *loopThenAnswerProvider) GetDefaultModel() string { return "mock-model" }

type alwaysLoopingProvider struct {
	calls atomic.Int32
}

func (p *alwaysLoopingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{Content: prodLoop}, nil
}

func (p *alwaysLoopingProvider) GetDefaultModel() string { return "mock-model" }

func TestDegenerateResponseIsRetriedWithANudge(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &loopThenAnswerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "procura tudo sobre isso", "degenerate-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if strings.Contains(resp, "princípios de interação com IA / \"princípios") {
		t.Errorf("the repeated block was delivered as the answer: %q", resp[:min(len(resp), 120)])
	}
	if got := provider.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (original + one retry)", got)
	}
	if !provider.sawNudge.Load() {
		t.Error("the retry went out without the corrective nudge")
	}
}

func TestDegenerateRetryIsCappedAndNothingLoopedIsPersisted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysLoopingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	if _, err := al.ProcessDirect(
		context.Background(),
		"procura tudo sobre isso",
		"degenerate-cap-session",
	); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := provider.calls.Load(); got != 1+maxDegenerateResponseRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxDegenerateResponseRetries)
	}

	history := directSessionHistory(t, al)
	if len(history) == 0 {
		t.Fatal("session history is empty — the loop below would be vacuous")
	}
	for _, m := range history {
		if strings.Contains(m.Content, "repeating the same phrase") {
			t.Errorf("the corrective nudge was persisted: %q", m.Content)
		}
		if strings.Contains(m.Content, "princípios de interação com IA / \"princípios") {
			t.Errorf("the repeated block was persisted: %q", m.Content[:min(len(m.Content), 120)])
		}
	}
}
