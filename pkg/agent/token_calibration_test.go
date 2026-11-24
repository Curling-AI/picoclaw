package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

func TestCalibratedWindow(t *testing.T) {
	const window = 1_000_000
	msgs := []providers.Message{{Role: "user", Content: strings.Repeat("a", 2_500)}}
	estimated := estimateCallTokens(msgs, nil, 0)
	cases := []struct {
		name     string
		measured int
		want     int
	}{
		{"heavier tokenizer shrinks the window", estimated * 5 / 4, window * 4 / 5},
		{"lighter tokenizer keeps the window", estimated / 2, window},
		{"ratio is capped", estimated * 10, int(window / maxPromptTokenRatio)},
		{"no usage keeps the window", 0, window},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &AgentInstance{promptTokenRatios: &sync.Map{}}
			a.recordPromptTokens("m", msgs, nil, c.measured)
			if got := a.calibratedWindow("m", window); got != c.want {
				t.Fatalf("calibratedWindow = %d, want %d", got, c.want)
			}
			if got := a.calibratedWindow("other", window); got != window {
				t.Fatalf("another model's window = %d, want the nominal %d", got, window)
			}
		})
	}
	var bare *AgentInstance
	bare.recordPromptTokens("m", msgs, nil, 1)
	if got := bare.calibratedWindow("m", window); got != window {
		t.Fatalf("nil agent window = %d, want %d", got, window)
	}
}

// Vercel AI Gateway, prod 23/11: every Anthropic upstream refused a call past
// the 1M window with 400, and the gateway answered with the last one's 404.
var errVercelLastUpstreamNotFound = errors.New("API request failed:\n  Status: 404\n  Body:   " +
	`{"error":{"message":"Not Found","type":"AI_APICallError"}}`)

// heavyTokenizerProvider counts every call 25% heavier than our estimate,
// reports that in usage, and refuses what does not fit its window the way the
// gateway did.
type heavyTokenizerProvider struct {
	mu         sync.Mutex
	window     int
	base       int // estimated tokens of the first call
	rejected   int
	toolRounds int // asks for big_output this many times a turn, then answers
}

func (p *heavyTokenizerProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	estimated := estimateCallTokens(messages, toolDefs, 0)
	if p.base == 0 {
		p.base = estimated
	}
	measured := estimated * 5 / 4
	if p.window > 0 && measured > p.window {
		p.rejected++
		return nil, errVercelLastUpstreamNotFound
	}
	usage := &protocoltypes.UsageInfo{PromptTokens: measured}
	if results := len(toolMessages(messages)); results < p.toolRounds {
		return &providers.LLMResponse{Usage: usage, ToolCalls: []providers.ToolCall{{
			ID:        fmt.Sprintf("call-%d", results+1),
			Name:      "big_output",
			Arguments: map[string]any{},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done", Usage: usage}, nil
}

func (p *heavyTokenizerProvider) GetDefaultModel() string { return "heavy-model" }

// A session whose estimate stays under the window while the upstream counts it
// past: the turn-start check measured against the calibrated window compacts
// before the call, so no turn is refused.
func TestHeavierTokenizerCompactsBeforeTheUpstreamRefuses(t *testing.T) {
	cfg := windowedConfig(t)
	provider := &heavyTokenizerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	const session = "sk_v1_heavy_tokenizer"
	ctx := context.Background()
	if _, err := al.ProcessDirectWithChannel(ctx, "ping", session, "test", session); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// Between 80% of the window and the window minus the output reserve the
	// estimate says a call fits and the upstream refuses it: 16K estimated
	// tokens a turn cannot step over that band.
	window := provider.base*5/4 + 100_000
	provider.mu.Lock()
	provider.window = window
	provider.mu.Unlock()
	agent.ContextWindow = window

	turnMessage := strings.Repeat("lorem ipsum dolor sit amet, ", 40_000/28)
	for turn := 1; turn <= 10; turn++ {
		resp, err := al.ProcessDirectWithChannel(ctx, fmt.Sprintf("%d %s", turn, turnMessage), session, "test", session)
		if err != nil || resp != "done" {
			t.Fatalf("turn %d: response=%q err=%v", turn, resp, err)
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.rejected > 0 {
		t.Fatalf("upstream refused %d calls the calibrated window should have compacted first", provider.rejected)
	}
}

// Tool results push the turn past the window between two calls, where no
// turn-start check runs: the gateway's 404 for that call is taken for the size
// refusal it is, and the context retry elides results until the call fits.
func TestRefusalPastTheMeasuredWindowRetriesAsContextError(t *testing.T) {
	cfg := windowedConfig(t)
	cfg.Tools.MaxResultChars = 10_000_000
	provider := &heavyTokenizerProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(&bigOutputTool{size: 200_000})
	agent := al.registry.GetDefaultAgent()
	const session = "sk_v1_heavy_tokenizer_turn"
	ctx := context.Background()
	if _, err := al.ProcessDirectWithChannel(ctx, "ping", session, "test", session); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// Each result is 80K estimated, 100K measured: the turn's third call carries
	// two of them and is past the window.
	window := provider.base*5/4 + 150_000
	provider.mu.Lock()
	provider.window, provider.toolRounds = window, 2
	provider.mu.Unlock()
	agent.ContextWindow = window

	resp, err := al.ProcessDirectWithChannel(ctx, "take two snapshots", session, "test", session)
	if err != nil || resp != "done" {
		t.Fatalf("response=%q err=%v", resp, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.rejected == 0 {
		t.Fatal("no call went past the window: the test no longer exercises the refusal")
	}
}
