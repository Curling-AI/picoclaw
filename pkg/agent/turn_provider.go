package agent

import (
	"context"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type turnProviderKey struct{}

type turnProvider struct {
	mu           sync.Mutex
	owner        string
	provider     providers.LLMProvider
	selectedTool string
}

// WithTurnProvider selects a caller-supplied provider for this turn only.
func WithTurnProvider(ctx context.Context, provider providers.LLMProvider) context.Context {
	return WithTurnProviderAndSelectedTool(ctx, provider, "")
}

// WithTurnProviderAndSelectedTool binds a caller-validated registered tool to
// the provider's turn, independently of the shared discovery TTL.
func WithTurnProviderAndSelectedTool(
	ctx context.Context,
	provider providers.LLMProvider,
	name string,
) context.Context {
	return context.WithValue(ctx, turnProviderKey{}, &turnProvider{provider: provider, selectedTool: name})
}

func turnProviderBinding(ctx context.Context, turnID string) *turnProvider {
	binding, ok := ctx.Value(turnProviderKey{}).(*turnProvider)
	if !ok {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.owner == "" {
		binding.owner = turnID
	}
	if binding.owner != turnID {
		return nil
	}
	return binding
}

func providerForTurn(ctx context.Context, turnID string) providers.LLMProvider {
	if binding := turnProviderBinding(ctx, turnID); binding != nil {
		return binding.provider
	}
	return nil
}

func selectedToolForTurn(ctx context.Context, turnID string) string {
	if binding := turnProviderBinding(ctx, turnID); binding != nil {
		return binding.selectedTool
	}
	return ""
}
