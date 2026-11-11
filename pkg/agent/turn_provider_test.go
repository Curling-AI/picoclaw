package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

type turnProviderProbe struct{}

func (*turnProviderProbe) GetDefaultModel() string { return "fixture" }
func (*turnProviderProbe) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	return nil, nil
}

func TestTurnProviderExecutesToolsWithoutWritingFinalReply(t *testing.T) {
	const sessionKey = "external-tool-turn"
	const callID = "external-tool-call"
	configured := &scriptedProvider{}
	al, agent, cleanup := newHookTestLoop(t, configured)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountHook(NamedHook("tool-rewrite", &toolRewriteHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}
	external := &scriptedProvider{script: []providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{{
			ID:        callID,
			Name:      "echo_text",
			Arguments: map[string]any{"text": "original"},
		}}},
		{},
	}}

	content, err := al.runAgentLoop(WithTurnProvider(t.Context(), external), agent, processOptions{
		SessionKey:      sessionKey,
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run tool",
		DefaultResponse: defaultResponse,
	})
	if err != nil || content != "" {
		t.Fatalf("runAgentLoop = (%q, %v), want an empty result without fallback", content, err)
	}
	if external.calls() != 2 || configured.calls() != 0 {
		t.Fatalf("provider calls = (%d external, %d configured), want (2, 0)", external.calls(), configured.calls())
	}

	history := agent.Sessions.GetHistory(sessionKey)
	if len(history) != 3 {
		t.Fatalf("history = %+v, want only user message, tool call and result", history)
	}
	call, result := history[1], history[2]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != callID {
		t.Fatalf("assistant history = %+v, want tool call %q", call, callID)
	}
	if result.Role != "tool" || result.ToolCallID != callID || result.Content != "after:modified" {
		t.Fatalf("tool history = %+v, want hooked result for %q", result, callID)
	}
}

func TestTurnProviderDoesNotReplaceTheProviderOfDelegatedOrLaterTurns(t *testing.T) {
	provider := &turnProviderProbe{}
	ctx := WithTurnProvider(t.Context(), provider)
	if providerForTurn(ctx, "owner") != provider ||
		providerForTurn(ctx, "child") != nil ||
		providerForTurn(ctx, "owner") != provider {
		t.Fatal("the external provider escaped its owning turn")
	}
	if providerForTurn(t.Context(), "owner") != nil {
		t.Fatal("ordinary turns received an external provider")
	}
}

func TestTurnProviderIterationLimitEndsWithoutRuntimeReply(t *testing.T) {
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	t.Cleanup(cleanup)
	t.Cleanup(al.Close)
	al.RegisterTool(&toolLimitTestTool{})
	ctx := WithTurnProvider(t.Context(), &toolLimitOnlyProvider{})
	result, err := al.runAgentLoopResult(ctx, al.registry.GetDefaultAgent(), processOptions{
		SessionKey:        "external-tool-limit",
		Channel:           "cli",
		ChatID:            "direct",
		UserMessage:       "run tool",
		SendResponse:      true,
		MaxToolIterations: 1,
	})
	if err != nil || result.Status != turnresult.Failed || result.Content != "" {
		t.Fatalf("result = (%+v, %v), want failure without runtime reply", result, err)
	}
	select {
	case outbound := <-msgBus.OutboundChan():
		t.Fatalf("unexpected runtime outbound: %+v", outbound)
	default:
	}
}

func TestTurnProviderRepeatGuardEndsWithoutRuntimeReply(t *testing.T) {
	al, _, msgBus, _, cleanup := newTestAgentLoop(t)
	t.Cleanup(cleanup)
	t.Cleanup(al.Close)
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
	al.RegisterTool(create)
	external := &stepProvider{steps: oneCallPerStep(times(createCall, 10)...)}
	ctx := WithTurnProvider(t.Context(), external)
	result, err := al.runAgentLoopResult(ctx, al.registry.GetDefaultAgent(), processOptions{
		SessionKey:   "external-repeat-guard",
		Channel:      "cli",
		ChatID:       "direct",
		UserMessage:  "run tool",
		SendResponse: true,
	})
	if err != nil || result.Status != turnresult.Failed || result.Content != "" {
		t.Fatalf("result = (%+v, %v), want failure without runtime reply", result, err)
	}
	if got := create.calls.Load(); got != maxUnchangedRuns {
		t.Fatalf("tool calls = %d, want repeat guard to stop at %d", got, maxUnchangedRuns)
	}
	if external.next != maxUnchangedRuns+2 {
		t.Fatalf("provider calls = %d, want %d", external.next, maxUnchangedRuns+2)
	}
	select {
	case outbound := <-msgBus.OutboundChan():
		t.Fatalf("unexpected runtime outbound: %+v", outbound)
	default:
	}
}
