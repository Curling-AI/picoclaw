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

// closersAfterAnswerProvider answers with a complete reply followed by the
// DSML closers a provider leaves after parsing a call.
type closersAfterAnswerProvider struct {
	calls atomic.Int32
}

const answerBeforeClosers = "Which day suits you for the visit, Monday or Tuesday?"

func (p *closersAfterAnswerProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{
		Content: answerBeforeClosers + "\n</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜function_calls>",
	}, nil
}

func (p *closersAfterAnswerProvider) GetDefaultModel() string { return "mock-model" }

func TestReplyEndingInDanglingCallClosersIsDeliveredWithoutThem(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &closersAfterAnswerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "book the visit", "dangling-closers-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != answerBeforeClosers {
		t.Errorf("response = %q, want the answer without the closers", resp)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1: the answer is complete, nothing to retry", got)
	}
	history := directSessionHistory(t, al)
	if len(history) == 0 {
		t.Fatal("session history is empty — the assertion below would be vacuous")
	}
	for _, m := range history {
		if strings.Contains(m.Content, "DSML") {
			t.Errorf("tool-call closers persisted to history: %q", m.Content)
		}
	}
}
