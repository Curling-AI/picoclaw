package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// noticeTurn is a question answered with the empty-response notice, then a
// conversation long enough to compact.
func noticeTurn() []providers.Message {
	notice := providers.Message{Role: "assistant", Content: defaultResponse, Notice: true}
	return []providers.Message{
		msg("user", "Q1"), notice,
		msg("user", "Q2"), msg("assistant", "A2"),
		msg("user", "Q3"), msg("assistant", "A3"),
		msg("user", "Q4"), msg("assistant", "A4"),
	}
}

// promptRecordingProvider keeps what every call sent, the summary prompt
// included.
type promptRecordingProvider struct {
	mu   sync.Mutex
	sent []string
}

func (p *promptRecordingProvider) Chat(
	_ context.Context, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range messages {
		p.sent = append(p.sent, m.Content)
	}
	return &providers.LLMResponse{Content: "resumo"}, nil
}

func (p *promptRecordingProvider) GetDefaultModel() string { return "mock-model" }

func (p *promptRecordingProvider) carried(text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sent {
		if strings.Contains(s, text) {
			return true
		}
	}
	return false
}

// The notice is for the person: summarized, it came back to the model inside
// the summary of the conversation, as an answer it gave.
func TestSummaryLeavesTheNoticeOut(t *testing.T) {
	al, sessions := newMirrorLoop(t)
	agent := al.registry.GetDefaultAgent()
	provider := &promptRecordingProvider{}
	agent.Provider = provider
	agent.KeepLastMessages = 4
	sessions.SetHistory(mirrorChatSession, noticeTurn())

	(&legacyContextManager{al: al}).summarizeSession(context.Background(), agent, mirrorChatSession)

	if !provider.carried("Q1") {
		t.Fatal("the summary did not read the turn before the notice — the check below would be vacuous")
	}
	if provider.carried(defaultResponse) {
		t.Fatal("the summary prompt carried the notice as an assistant reply")
	}
}

// Seahorse rebuilds its store from the session history when it starts: the
// notice came back to the model as a reply, which the history sent on the
// legacy path already skips.
func TestSeahorseStartupLeavesTheNoticeOut(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ContextManager = "seahorse"
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockProvider{})
	manager, ok := al.contextManager.(*seahorseContextManager)
	if !ok {
		t.Fatal("real Seahorse context manager unavailable")
	}
	defer manager.engine.Close()
	const key = "sk_v1_seahorse_notice"
	al.registry.GetDefaultAgent().Sessions.SetHistory(key, noticeTurn())

	manager.bootstrapSession(t.Context(), key)
	assembled, err := manager.Assemble(t.Context(), &AssembleRequest{SessionKey: key, Budget: 100000, MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}

	contents := make([]string, 0, len(assembled.History))
	for _, m := range assembled.History {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "\n")
	if !strings.Contains(joined, "Q1") {
		t.Fatalf("assembled history %q lost the question — the check below would be vacuous", joined)
	}
	if strings.Contains(joined, defaultResponse) {
		t.Fatal("Seahorse handed the notice back to the model as a reply")
	}
}
