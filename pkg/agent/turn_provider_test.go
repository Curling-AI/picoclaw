package agent

import (
	"context"
	"slices"
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
	ctx := WithTurnProviderAndSelectedTool(t.Context(), provider, heldToolName)
	if providerForTurn(ctx, "owner") != provider ||
		providerForTurn(ctx, "child") != nil ||
		providerForTurn(ctx, "owner") != provider {
		t.Fatal("the external provider escaped its owning turn")
	}
	if providerForTurn(t.Context(), "owner") != nil {
		t.Fatal("ordinary turns received an external provider")
	}
	if selectedToolForTurn(ctx, "owner") != heldToolName ||
		selectedToolForTurn(ctx, "child") != "" ||
		selectedToolForTurn(t.Context(), "owner") != "" {
		t.Fatal("the selected tool escaped its owning turn")
	}
}

type selectedToolProbe struct {
	name       string
	force      bool
	afterOffer func()
	offered    [][]string
}

func (*selectedToolProbe) GetDefaultModel() string { return "fixture" }

func (p *selectedToolProbe) Chat(
	_ context.Context,
	_ []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Function.Name)
	}
	p.offered = append(p.offered, names)
	if len(p.offered) != 1 || p.name == "" || (!p.force && !slices.Contains(names, p.name)) {
		return &providers.LLMResponse{Content: "done"}, nil
	}
	if p.afterOffer != nil {
		p.afterOffer()
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID: "selected-call", Name: p.name, Arguments: map[string]any{},
	}}}, nil
}

func TestTurnProviderSelectedToolSurvivesSharedTTL(t *testing.T) {
	ordinary := &selectedToolProbe{}
	al, agent, cleanup := newHookTestLoop(t, ordinary)
	t.Cleanup(cleanup)
	selected := &countingTool{name: heldToolName}
	agent.Tools.RegisterHidden(selected)
	agent.Tools.RegisterHidden(&countingTool{name: foreignToolName})
	agent.Tools.PromoteTools([]string{selected.Name()}, 1)
	agent.Tools.TickTTL() // A different conversation expires the old shared promotion before offer.

	run := func(ctx context.Context, key string) {
		t.Helper()
		if _, err := al.runAgentLoop(ctx, agent, processOptions{
			SessionKey:      key,
			Channel:         "cli",
			ChatID:          "direct",
			UserMessage:     "run tool",
			DefaultResponse: defaultResponse,
		}); err != nil {
			t.Fatal(err)
		}
	}
	run(t.Context(), "ordinary-before")
	external := &selectedToolProbe{name: selected.Name(), afterOffer: agent.Tools.TickTTL}
	ctx := WithTurnProviderAndSelectedTool(t.Context(), external, selected.Name())
	run(ctx, "selected")
	run(t.Context(), "ordinary-after")
	if got := selected.calls.Load(); got != 1 {
		t.Fatalf("selected tool ran %d times after expiry, want 1; external offers %v", got, external.offered)
	}
	if !slices.Contains(external.offered[0], selected.Name()) ||
		slices.Contains(external.offered[0], foreignToolName) {
		t.Fatalf("unexpected external offers: %v", external.offered)
	}
	if _, visible := agent.Tools.Get(selected.Name()); visible {
		t.Fatal("selected execution revived the shared discovery entry")
	}
	for _, names := range ordinary.offered {
		if slices.Contains(names, selected.Name()) {
			t.Fatalf("ordinary turn inherited the selected tool: %v", names)
		}
	}
}

type closedSelectedTool struct{ countingTool }

func (*closedSelectedTool) OfferedIn(context.Context) bool { return false }

func TestTurnProviderSelectedToolPreservesRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
	}{
		{name: "different tool", call: foreignToolName},
		{name: "scoped tool", call: heldToolName},
		{name: "execution policy", call: heldToolName},
		{name: "approval hook", call: heldToolName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, agent, cleanup := newHookTestLoop(t, &selectedToolProbe{})
			t.Cleanup(cleanup)
			selected := &countingTool{name: heldToolName}
			var registered tools.Tool = selected
			if tc.name == "scoped tool" {
				scoped := &closedSelectedTool{countingTool: countingTool{name: selected.Name()}}
				selected = &scoped.countingTool
				registered = scoped
			}
			other := &countingTool{name: foreignToolName}
			agent.Tools.RegisterHidden(registered)
			agent.Tools.RegisterHidden(other)
			external := &selectedToolProbe{name: tc.call, force: true}
			ctx := WithTurnProviderAndSelectedTool(t.Context(), external, selected.Name())
			if tc.name == "execution policy" {
				ctx = tools.WithExecutionPolicy(ctx, tools.ExecutionPolicy{
					AllowsTool: func(tools.Tool) bool { return false },
				})
			}
			if tc.name == "approval hook" {
				if err := al.MountHook(NamedHook("deny-approval", &denyApprovalHook{})); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := al.ProcessDirect(ctx, "run the requested tool", "selected"); err != nil {
				t.Fatal(err)
			}
			if selected.calls.Load() != 0 || other.calls.Load() != 0 {
				t.Fatalf("selection bypassed %s", tc.name)
			}
			if (tc.name == "scoped tool" || tc.name == "execution policy") &&
				slices.Contains(external.offered[0], selected.Name()) {
				t.Fatalf("selection advertised a forbidden tool: %v", external.offered[0])
			}
		})
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
