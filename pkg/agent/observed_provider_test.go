package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type staticUsageProvider struct{ usage *providers.UsageInfo }

func (s *staticUsageProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: "ok", Usage: s.usage}, nil
}

func (s *staticUsageProvider) GetDefaultModel() string { return "mock-model" }

type captureLLMHook struct {
	mu   sync.Mutex
	seen []*LLMHookResponse
}

func (c *captureLLMHook) BeforeLLM(
	_ context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	return req, HookDecision{Action: HookActionContinue}, nil
}

func (c *captureLLMHook) AfterLLM(
	_ context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	c.mu.Lock()
	c.seen = append(c.seen, resp)
	c.mu.Unlock()
	return resp, HookDecision{Action: HookActionContinue}, nil
}

func TestObservedProvider_ReportsUsageThroughAfterLLM(t *testing.T) {
	inner := &staticUsageProvider{usage: &providers.UsageInfo{
		PromptTokens: 100, CompletionTokens: 40, ReasoningTokens: 10, CostUSD: 0.02,
	}}
	obs := newObservedProvider(inner, "evolution")

	// Before attach: must not panic, just pass through.
	if _, err := obs.Chat(context.Background(), nil, nil, "m", nil); err != nil {
		t.Fatalf("chat before attach: %v", err)
	}

	provider := &staticUsageProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	hook := &captureLLMHook{}
	if err := al.hooks.Mount(HookRegistration{Name: "capture", Hook: hook}); err != nil {
		t.Fatalf("mount hook: %v", err)
	}
	obs.attach(al)

	if _, err := obs.Chat(context.Background(), nil, nil, "deepseek/x", nil); err != nil {
		t.Fatalf("chat after attach: %v", err)
	}

	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.seen) != 1 {
		t.Fatalf("AfterLLM calls = %d, want 1", len(hook.seen))
	}
	got := hook.seen[0]
	if got.Meta.Source != "evolution" || got.Meta.SessionKey != "background:evolution" {
		t.Errorf("meta = %+v, want source/sessionkey evolution", got.Meta)
	}
	if got.Model != "deepseek/x" {
		t.Errorf("model = %q, want deepseek/x", got.Model)
	}
	if got.Response == nil || got.Response.Usage == nil || got.Response.Usage.CostUSD != 0.02 {
		t.Errorf("usage not propagated: %+v", got.Response)
	}
}

func TestNotifyBackgroundLLM_NilSafe(t *testing.T) {
	// nil loop, nil usage, nil response: all no-ops.
	notifyBackgroundLLM(context.Background(), nil, "vision", "m", &providers.LLMResponse{})
	provider := &staticUsageProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	notifyBackgroundLLM(context.Background(), al, "vision", "m", nil)
	notifyBackgroundLLM(context.Background(), al, "vision", "m", &providers.LLMResponse{})
}
