package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// The gateway keys idempotency on the trace id, so a fallback call must not
// reuse the one the stream already spent — nor edit the caller's options.
func TestOptsWithFreshRequestID_DoesNotReuseOrMutate(t *testing.T) {
	base := map[string]any{llmRequestIDOption: "pc-original", "temperature": 0.7}

	next, id := optsWithFreshRequestID(base)

	if id == "" || id == "pc-original" {
		t.Fatalf("fresh id = %q, want a new one", id)
	}
	if next[llmRequestIDOption] != id {
		t.Fatalf("options carry %v, want %q", next[llmRequestIDOption], id)
	}
	if base[llmRequestIDOption] != "pc-original" {
		t.Fatalf("caller options mutated: %v", base[llmRequestIDOption])
	}
	if next["temperature"] != 0.7 {
		t.Fatalf("temperature lost in the clone: %v", next["temperature"])
	}
}

func TestNewLLMRequestID_IsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		id := newLLMRequestID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestResponseCompletionTokens_UnknownIsNegative(t *testing.T) {
	if got := responseCompletionTokens(nil); got != -1 {
		t.Fatalf("nil response = %d, want -1", got)
	}
	if got := responseCompletionTokens(&providers.LLMResponse{}); got != -1 {
		t.Fatalf("no usage = %d, want -1", got)
	}
	resp := &providers.LLMResponse{Usage: &providers.UsageInfo{CompletionTokens: 14}}
	if got := responseCompletionTokens(resp); got != 14 {
		t.Fatalf("completion tokens = %d, want 14", got)
	}
}

// requestIDRecorder fails its first call with a server error and keeps the
// request id every call carried.
type requestIDRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (p *requestIDRecorder) Chat(
	_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, requestIDFromOptions(opts))
	if len(p.ids) == 1 {
		return nil, errors.New("API request failed:\n  Status: 500\n  Body:   internal server error")
	}
	return &providers.LLMResponse{Content: "recovered", FinishReason: "stop"}, nil
}

func (p *requestIDRecorder) GetDefaultModel() string { return "test-model" }

// A retry is a new upstream call: the gateway reads a repeated request id as
// the call before it.
func TestPipeline_CallLLM_RetryGetsAFreshRequestID(t *testing.T) {
	provider := &requestIDRecorder{}
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace:           t.TempDir(),
		ModelName:           "test-model",
		MaxTokens:           4096,
		MaxToolIterations:   10,
		MaxLLMRetries:       1,
		LLMRetryBackoffSecs: 1,
	}}}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()
	agent := al.registry.GetDefaultAgent()
	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	retries, stop := subscribeRuntimeEventsForTest(t, al, 4, runtimeevents.KindAgentLLMRetry)
	defer stop()
	if _, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1); err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	ids := provider.ids
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("request ids = %q, want a fresh one for the retry", ids)
	}
	if exec.llmRequestID != ids[1] {
		t.Fatalf("recorded request id = %q, want the one the answering call carried (%q)", exec.llmRequestID, ids[1])
	}
	// The retry event is the only record of an attempt that never answers.
	retry, ok := findRuntimeEvent(collectRuntimeEventStream(retries), runtimeevents.KindAgentLLMRetry)
	if payload, _ := retry.Payload.(LLMRetryPayload); !ok || payload.RequestID != ids[1] {
		t.Fatalf("retry event = %+v, want it to name the retry's request id %q", retry.Payload, ids[1])
	}
}
