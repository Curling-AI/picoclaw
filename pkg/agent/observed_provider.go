package agent

import (
	"context"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// observedProvider wraps an LLMProvider so background call paths that bypass
// the turn pipeline (evolution cold path today) still report usage through
// the AfterLLM hooks — where the usage meter lives. Without this, those calls
// are billed by the provider but invisible to local metering (observed in
// prod: ~2 unmetered evolution calls per turn, the bulk of the request gap
// between the gateway report and usage_events).
//
// The AgentLoop is attached late: NewAgentLoop builds the evolution bridge
// before the loop struct exists, so Chat reads the pointer at call time.
type observedProvider struct {
	providers.LLMProvider
	source string

	mu sync.RWMutex
	al *AgentLoop
}

func newObservedProvider(inner providers.LLMProvider, source string) *observedProvider {
	return &observedProvider{LLMProvider: inner, source: source}
}

func (o *observedProvider) attach(al *AgentLoop) {
	o.mu.Lock()
	o.al = al
	o.mu.Unlock()
}

func (o *observedProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	resp, err := o.LLMProvider.Chat(ctx, messages, tools, model, options)
	if err == nil {
		o.mu.RLock()
		al := o.al
		o.mu.RUnlock()
		notifyBackgroundLLM(ctx, al, o.source, model, resp)
	}
	return resp, err
}

// notifyBackgroundLLM reports a background (non-turn) LLM call to the
// AfterLLM hooks, best-effort. The source doubles as the usage-attribution
// channel ("evolution", "vision", "summarize") — the meter maps it through
// the "background:" session-key prefix.
func notifyBackgroundLLM(
	ctx context.Context,
	al *AgentLoop,
	source, model string,
	resp *providers.LLMResponse,
) {
	if al == nil || al.hooks == nil || resp == nil || resp.Usage == nil {
		return
	}
	_, _ = al.hooks.AfterLLM(ctx, &LLMHookResponse{
		Meta: HookMeta{
			Source:     source,
			SessionKey: "background:" + source,
			TracePath:  "background.llm",
		},
		Model:    model,
		Response: resp,
	})
}
