package agent

import (
	"context"
	"errors"
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

type failingUsageProvider struct{ staticUsageProvider }

func (p *failingUsageProvider) Chat(context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: "partial", Usage: &providers.UsageInfo{PromptTokens: 10, CompletionTokens: 3}}, errors.New("provider unavailable")
}

type captureLLMHook struct {
	mu       sync.Mutex
	seen     []*LLMHookResponse
	contexts []context.Context
}

func (c *captureLLMHook) BeforeLLM(
	_ context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	return req, HookDecision{Action: HookActionContinue}, nil
}

func (c *captureLLMHook) AfterLLM(
	ctx context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	c.mu.Lock()
	c.seen = append(c.seen, resp)
	c.contexts = append(c.contexts, ctx)
	c.mu.Unlock()
	return resp, HookDecision{Action: HookActionContinue}, nil
}

func TestBackgroundUsageAbsenceRemainsObservable(t *testing.T) {
	for _, path := range []string{"evolution provider", "vision delegate", "nil response", "evolution error", "vision error"} {
		t.Run(path, func(t *testing.T) {
			provider := &staticUsageProvider{}
			al, instance, cleanup := newTurnCoordTestLoop(t, provider)
			defer cleanup()
			hook := &captureLLMHook{}
			if err := al.hooks.Mount(HookRegistration{Name: "capture-missing-usage", Hook: hook}); err != nil {
				t.Fatal(err)
			}
			key := struct{}{}
			ctx := context.WithValue(t.Context(), key, "execution-context")
			source := "vision"
			switch path {
			case "evolution provider":
				source = "evolution"
				observed := newObservedProvider(provider, source)
				observed.attach(al)
				if _, err := observed.Chat(ctx, nil, nil, "model", nil); err != nil {
					t.Fatal(err)
				}
			case "vision delegate":
				pipeline := &Pipeline{al: al}
				turn := &turnState{agent: instance, sessionKey: "agent:cron-job-11111111-1111-4111-8111-111111111111"}
				if _, err := pipeline.callVisionDelegate(ctx, turn, provider, "model", "", []string{"data:image/png;base64,abc"}); err != nil {
					t.Fatal(err)
				}
			case "nil response":
				notifyBackgroundLLM(ctx, al, source, "model", nil)
			case "evolution error":
				source = "evolution"
				observed := newObservedProvider(&failingUsageProvider{}, source)
				observed.attach(al)
				if _, err := observed.Chat(ctx, nil, nil, "model", nil); err == nil {
					t.Fatal("provider error was hidden")
				}
			case "vision error":
				pipeline := &Pipeline{al: al}
				turn := &turnState{agent: instance, sessionKey: "agent:cron-job-11111111-1111-4111-8111-111111111111"}
				if _, err := pipeline.callVisionDelegate(ctx, turn, &failingUsageProvider{}, "model", "", []string{"data:image/png;base64,abc"}); err == nil {
					t.Fatal("vision error was hidden")
				}
			}
			hook.mu.Lock()
			defer hook.mu.Unlock()
			if len(hook.seen) != 1 {
				t.Fatalf("missing usage was swallowed: AfterLLM calls=%d, want 1", len(hook.seen))
			}
			got := hook.seen[0]
			if got.Response != nil && got.Response.Usage != nil {
				t.Fatal("missing usage was replaced by invented zero usage")
			}
			if got.Meta.Source != source || got.Meta.SessionKey != "background:"+source || hook.contexts[0].Value(key) != "execution-context" {
				t.Fatal("background source or execution context was lost")
			}
		})
	}
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

func TestObservedProvider_RecoveryDoesNotHideAnUnmeasuredFailedCall(t *testing.T) {
	known := &staticUsageProvider{usage: &providers.UsageInfo{PromptTokens: 10, CompletionTokens: 3}}
	al, _, cleanup := newTurnCoordTestLoop(t, known)
	defer cleanup()
	hook := &captureLLMHook{}
	if err := al.hooks.Mount(HookRegistration{Name: "capture-recovery", Hook: hook}); err != nil {
		t.Fatal(err)
	}
	observed := newObservedProvider(known, "evolution")
	observed.attach(al)
	if _, err := observed.Chat(t.Context(), nil, nil, "model", nil); err != nil {
		t.Fatal(err)
	}
	observed.LLMProvider = &failingUsageProvider{}
	if _, err := observed.Chat(t.Context(), nil, nil, "model", nil); err == nil {
		t.Fatal("failed call unexpectedly succeeded")
	}
	observed.LLMProvider = known
	if _, err := observed.Chat(t.Context(), nil, nil, "model", nil); err != nil {
		t.Fatal("recovery", err)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.seen) != 3 || hook.seen[1].Response != nil {
		t.Fatal("failure between measured calls did not invalidate completeness")
	}
}

func TestNotifyBackgroundLLM_NilSafe(t *testing.T) {
	// A nil loop is a no-op; absent responses and usage are safe hook inputs.
	notifyBackgroundLLM(context.Background(), nil, "vision", "m", &providers.LLMResponse{})
	provider := &staticUsageProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	notifyBackgroundLLM(context.Background(), al, "vision", "m", nil)
	notifyBackgroundLLM(context.Background(), al, "vision", "m", &providers.LLMResponse{})
}
