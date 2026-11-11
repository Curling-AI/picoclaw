package agent

import (
	"context"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type turnProviderKey struct{}

type turnProvider struct {
	mu       sync.Mutex
	owner    string
	provider providers.LLMProvider
}

// WithTurnProvider selects a caller-supplied provider for this turn only.
func WithTurnProvider(ctx context.Context, provider providers.LLMProvider) context.Context {
	return context.WithValue(ctx, turnProviderKey{}, &turnProvider{provider: provider})
}

func providerForTurn(ctx context.Context, turnID string) providers.LLMProvider {
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
	return binding.provider
}
