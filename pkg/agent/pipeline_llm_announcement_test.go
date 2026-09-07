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

// announcedToolCall is verbatim from greenhouse (2026-09-07). The turn ended
// right here: no markup for the truncation guard to find, no call on the wire,
// and the user had to type "Siga" to get anything else.
const announcedToolCall = "Vou buscar o artigo do DCRainmaker sobre o Fenix 9 e resumir, depois procurar " +
	"relação com o seu relógio 970.\n\nPrimeiro, deixa eu abrir o artigo."

type announceThenAnswerProvider struct {
	calls    atomic.Int32
	sawNudge atomic.Bool
}

func (p *announceThenAnswerProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{Content: announcedToolCall}, nil
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "announced an action") {
			p.sawNudge.Store(true)
		}
	}
	return &providers.LLMResponse{Content: "o Fenix 9 traz mapas offline e ECG"}, nil
}

func (p *announceThenAnswerProvider) GetDefaultModel() string { return "mock-model" }

type alwaysAnnouncingProvider struct {
	calls atomic.Int32
}

func (p *alwaysAnnouncingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{Content: announcedToolCall}, nil
}

func (p *alwaysAnnouncingProvider) GetDefaultModel() string { return "mock-model" }

func TestAnnouncedToolCallIsRetriedWithANudge(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &announceThenAnswerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "resuma esse artigo pra mim", "announce-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp == announcedToolCall {
		t.Error("the promise was delivered as the answer instead of being retried")
	}
	if got := provider.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (original + one retry)", got)
	}
	if !provider.sawNudge.Load() {
		t.Error("the retry went out without the corrective nudge")
	}
}

func TestAnnouncementRetryIsCappedAndTheNudgeIsNeverPersisted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysAnnouncingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	if _, err := al.ProcessDirect(context.Background(), "resuma esse artigo pra mim", "announce-cap-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := provider.calls.Load(); got != 1+maxUndeliveredAnnouncementRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxUndeliveredAnnouncementRetries)
	}

	// The correction is about THIS emission. Persisted, it reads as a standing
	// instruction in every later turn of the session.
	history := directSessionHistory(t, al)
	if len(history) == 0 {
		t.Fatal("session history is empty — the loop below would be vacuous")
	}
	for _, m := range history {
		if strings.Contains(m.Content, "announced an action") {
			t.Errorf("the corrective nudge was persisted: %q", m.Content)
		}
	}
}
