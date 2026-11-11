package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type turnProviderProbe struct{}

func (*turnProviderProbe) GetDefaultModel() string { return "fixture" }
func (*turnProviderProbe) Chat(context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any) (*providers.LLMResponse, error) {
	return nil, nil
}

func TestTurnProviderDoesNotReplaceTheProviderOfDelegatedOrLaterTurns(t *testing.T) {
	provider := &turnProviderProbe{}
	ctx := WithTurnProvider(t.Context(), provider)
	if providerForTurn(ctx, "owner") != provider || providerForTurn(ctx, "child") != nil || providerForTurn(ctx, "owner") != provider {
		t.Fatal("the external provider escaped its owning turn")
	}
	if providerForTurn(t.Context(), "owner") != nil {
		t.Fatal("ordinary turns received an external provider")
	}
}
