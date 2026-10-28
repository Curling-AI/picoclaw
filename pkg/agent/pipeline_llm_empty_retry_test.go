package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/session"
)

// emptyThenContentProvider returns an empty (no content, no tool calls)
// response on the first call and a real answer afterwards — the shape of the
// reasoning-only glitch observed in prod.
type emptyThenContentProvider struct {
	calls atomic.Int32
}

func (p *emptyThenContentProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{Content: ""}, nil
	}
	return &providers.LLMResponse{Content: "recovered answer"}, nil
}

func (p *emptyThenContentProvider) GetDefaultModel() string { return "mock-model" }

// alwaysEmptyProvider never produces content — the retry cap must end the
// turn with the default fallback instead of looping.
type alwaysEmptyProvider struct {
	calls atomic.Int32
}

func (p *alwaysEmptyProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	return &providers.LLMResponse{Content: ""}, nil
}

func (p *alwaysEmptyProvider) GetDefaultModel() string { return "mock-model" }

func TestEmptyLLMResponseIsRetriedOnce(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &emptyThenContentProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "hello", "empty-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "recovered answer" {
		t.Errorf("response = %q, want the retried answer", resp)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (original + one retry)", got)
	}
}

func TestEmptyLLMResponseRetryIsCapped(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysEmptyProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "hello", "empty-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp == "recovered answer" {
		t.Fatalf("unexpected content from always-empty provider")
	}
	if got := provider.calls.Load(); got != 1+maxEmptyResponseRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxEmptyResponseRetries)
	}
}

// reasoningOnlyProvider always returns thinking but no visible content, in
// the reasoning_content (DeepSeek-style) or reasoning (AI-Gateway-style)
// field depending on useReasoningField.
type reasoningOnlyProvider struct {
	calls             atomic.Int32
	useReasoningField bool
	thought           string
}

func (p *reasoningOnlyProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	resp := &providers.LLMResponse{Content: ""}
	if p.useReasoningField {
		resp.Reasoning = p.thought
	} else {
		resp.ReasoningContent = p.thought
	}
	return resp, nil
}

func (p *reasoningOnlyProvider) GetDefaultModel() string { return "mock-model" }

// directSessionHistory resolves the session key the way processMessage does
// and returns the persisted history for a ProcessDirect call.
func directSessionHistory(t *testing.T, al *AgentLoop) []providers.Message {
	t.Helper()
	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("no default agent found")
	}
	return defaultAgent.Sessions.GetHistory(directSessionKey(al))
}

// directSessionKey is the session key a ProcessDirect call resolves to.
func directSessionKey(al *AgentLoop) string {
	route := al.registry.ResolveRoute(bus.InboundContext{
		Channel:  "cli",
		ChatType: "direct",
		SenderID: "cron",
	})
	alloc := al.allocateRouteSession(route, testInboundMessage(bus.InboundMessage{
		Channel:  "cli",
		SenderID: "cron",
		ChatID:   "direct",
	}))
	return alloc.SessionKey
}

// A synthesized empty-response fallback must never enter session history:
// the model reads its own prior replies, and a persisted fallback teaches it
// to keep answering with nothing (observed in prod as sessions degrading to
// ~50% empty turns until wiped).
func TestEmptyFallbackNotPersistedToHistory(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &alwaysEmptyProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "hello", "empty-no-persist-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp == "" {
		t.Fatal("expected the fallback text as the user-visible response")
	}

	history := directSessionHistory(t, al)
	if len(history) != 1 {
		t.Fatalf("history len = %d, want 1 (user message only)", len(history))
	}
	assertRoles(t, history, "user")
	for _, msg := range history {
		if msg.Content == resp {
			t.Fatalf("fallback text %q was persisted to history", resp)
		}
	}
}

func TestReasoningSalvagedAfterRetriesExhausted(t *testing.T) {
	for _, tc := range []struct {
		name              string
		useReasoningField bool
	}{
		{name: "reasoning_content field", useReasoningField: false},
		{name: "reasoning field", useReasoningField: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = t.TempDir()
			provider := &reasoningOnlyProvider{
				useReasoningField: tc.useReasoningField,
				thought:           "the salvaged thinking",
			}
			al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

			resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-salvage-session")
			if err != nil {
				t.Fatalf("ProcessDirect: %v", err)
			}
			if resp != "the salvaged thinking" {
				t.Errorf("response = %q, want the salvaged reasoning", resp)
			}
			// The glitch is retried before reasoning is used as the reply.
			if got := provider.calls.Load(); got != 1+maxEmptyResponseRetries {
				t.Errorf("provider calls = %d, want %d (original + retries before salvage)",
					got, 1+maxEmptyResponseRetries)
			}
			// Salvaged reasoning is a real reply and stays in history.
			history := directSessionHistory(t, al)
			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2 (user + salvaged assistant)", len(history))
			}
			assertRoles(t, history, "user", "assistant")
			if history[1].Content != "the salvaged thinking" {
				t.Errorf("persisted assistant content = %q, want the salvaged reasoning", history[1].Content)
			}
		})
	}
}

const reasoningCapNudgeMarker = "whole output limit"

// reasoningAtCap is the shape of the prod failure (2026-10-28): the call
// stopped at the output limit with thinking and no visible text, the thinking
// a loop over the same list.
func reasoningAtCap() providers.LLMResponse {
	return providers.LLMResponse{
		ReasoningContent: "Candidates:\n- Ana?\n- Maria?\n- Ana?\n- Maria?\n- Ana?",
		FinishReason:     "length",
		Usage:            &providers.UsageInfo{CompletionTokens: config.DefaultConfig().Agents.Defaults.MaxTokens},
	}
}

func TestReasoningAtOutputCapIsRetriedWithNudgeAndNeverSalvaged(t *testing.T) {
	al, provider := newScriptedLoop(t, reasoningAtCap(), reasoningAtCap())

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != defaultResponse {
		t.Errorf("response = %q, want the fallback, not the cut reasoning", resp)
	}
	if got := provider.calls(); got != 1+maxEmptyResponseRetries {
		t.Errorf("provider calls = %d, want %d", got, 1+maxEmptyResponseRetries)
	}
	if provider.callCarried(1, reasoningCapNudgeMarker) {
		t.Error("the first call must not carry the output-cap nudge")
	}
	if !provider.callCarried(2, reasoningCapNudgeMarker) {
		t.Error("the retry must carry the output-cap nudge")
	}
	history := directSessionHistory(t, al)
	if len(history) != 1 {
		t.Fatalf("history len = %d, want 1 (user message only)", len(history))
	}
	assertRoles(t, history, "user")
}

// finish_reason is not always reported; completion tokens at or past
// max_tokens mean the same thing (prod reports up to 7 past the limit).
func TestReasoningAtOutputCapIsDetectedByTokensWhenFinishReasonIsMissing(t *testing.T) {
	atCap := providers.LLMResponse{
		ReasoningContent:    "cut mid-thought",
		FinishReasonMissing: true,
		Usage: &providers.UsageInfo{
			CompletionTokens: config.DefaultConfig().Agents.Defaults.MaxTokens + 3,
		},
	}
	al, provider := newScriptedLoop(t, atCap, atCap)

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-cap-tokens-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != defaultResponse {
		t.Errorf("response = %q, want the fallback", resp)
	}
	if !provider.callCarried(2, reasoningCapNudgeMarker) {
		t.Error("the retry must carry the output-cap nudge")
	}
}

func TestReasoningAtOutputCapRecoversOnTheNudgedRetry(t *testing.T) {
	al, provider := newScriptedLoop(t, reasoningAtCap(), providers.LLMResponse{Content: "the answer"})

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-cap-recover-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "the answer" {
		t.Errorf("response = %q, want the retried answer", resp)
	}
	if !provider.callCarried(2, reasoningCapNudgeMarker) {
		t.Error("the retry must carry the output-cap nudge")
	}
	for _, msg := range directSessionHistory(t, al) {
		if strings.Contains(msg.Content, reasoningCapNudgeMarker) {
			t.Fatalf("the nudge entered session history: %q", msg.Content)
		}
	}
}

// The nudge talks about "your last reply"; once the model makes a real call it
// is stale and must not ride along on the rest of the turn.
func TestReasoningCapNudgeIsDroppedAfterARealToolCall(t *testing.T) {
	al, provider := newScriptedLoop(t,
		reasoningAtCap(),
		readFileCall("call_1"),
		providers.LLMResponse{Content: "done"},
	)

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-cap-tool-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "done" {
		t.Errorf("response = %q, want %q", resp, "done")
	}
	if !provider.callCarried(2, reasoningCapNudgeMarker) {
		t.Error("the retry must carry the output-cap nudge")
	}
	if provider.callCarried(3, reasoningCapNudgeMarker) {
		t.Error("the call after the tool ran must not carry the output-cap nudge")
	}
}

// blockingScriptedProvider holds call blockAt until release is closed, so a
// test can steer while that call is in flight.
type blockingScriptedProvider struct {
	scriptedProvider
	blockAt int
	started chan struct{}
	release chan struct{}
}

func (p *blockingScriptedProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls()+1 == p.blockAt {
		close(p.started)
		<-p.release
	}
	return p.scriptedProvider.Chat(ctx, messages, tools, model, options)
}

// The user steers while the nudged retry is in flight: the turn goes on with the
// new message, and the next call must not repeat that nothing reached them,
// whether the retry answered or hit the limit again.
func TestReasoningCapNudgeIsDroppedWhenTheUserSteersAfterTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry providers.LLMResponse
	}{
		{name: "retry answers", retry: providers.LLMResponse{Content: "an answer the user was reading"}},
		{name: "retry hits the limit again", retry: reasoningAtCap()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = t.TempDir()
			provider := &blockingScriptedProvider{
				scriptedProvider: scriptedProvider{script: []providers.LLMResponse{
					reasoningAtCap(),
					tc.retry,
					{Content: "answer to the follow-up"},
				}},
				blockAt: 2,
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
			sessionKey := session.BuildMainSessionKey(routing.DefaultAgentID)

			type result struct {
				resp string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := al.ProcessDirectWithChannel(context.Background(), "hello", sessionKey, "test", "chat1")
				done <- result{resp, err}
			}()

			select {
			case <-provider.started:
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for the nudged retry")
			}
			if err := al.Steer(providers.Message{Role: "user", Content: "follow-up"}); err != nil {
				t.Fatalf("Steer: %v", err)
			}
			close(provider.release)

			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("ProcessDirectWithChannel: %v", r.err)
				}
				if r.resp != "answer to the follow-up" {
					t.Errorf("response = %q, want the answer to the follow-up", r.resp)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for the turn")
			}
			if got := provider.calls(); got != 3 {
				t.Fatalf("provider calls = %d, want 3", got)
			}
			if !provider.callCarried(2, reasoningCapNudgeMarker) {
				t.Error("the retry must carry the output-cap nudge")
			}
			if !provider.callCarried(3, "follow-up") {
				t.Error("the call after steering must carry the follow-up")
			}
			if provider.callCarried(3, reasoningCapNudgeMarker) {
				t.Error("the call after steering must not carry the output-cap nudge")
			}
		})
	}
}

// Same when the nudged retry comes back with text that another guard retries.
func TestReasoningCapNudgeIsDroppedWhenAnotherGuardRetriesTheAnswer(t *testing.T) {
	al, provider := newScriptedLoop(t,
		reasoningAtCap(),
		providers.LLMResponse{Content: truncatedToolCallTail},
		providers.LLMResponse{Content: "patch applied"},
	)

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-cap-guard-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "patch applied" {
		t.Errorf("response = %q, want %q", resp, "patch applied")
	}
	if !provider.callCarried(3, "tool call written out as text") {
		t.Error("the third call must carry the truncated-tool-call nudge")
	}
	if provider.callCarried(3, reasoningCapNudgeMarker) {
		t.Error("the third call must not carry the output-cap nudge")
	}
}

// Without the output limit the old behavior stands: no nudge, and the thinking
// is salvaged as the reply.
func TestEmptyReplyBelowOutputCapIsSalvagedWithoutNudge(t *testing.T) {
	stopped := providers.LLMResponse{
		ReasoningContent: "the salvaged thinking",
		FinishReason:     "stop",
		Usage:            &providers.UsageInfo{CompletionTokens: 120},
	}
	al, provider := newScriptedLoop(t, stopped, stopped)

	resp, err := al.ProcessDirect(context.Background(), "hello", "reasoning-stop-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "the salvaged thinking" {
		t.Errorf("response = %q, want the salvaged reasoning", resp)
	}
	if got := provider.calls(); got != 1+maxEmptyResponseRetries {
		t.Fatalf(
			"provider calls = %d, want %d (callCarried below is vacuous otherwise)",
			got,
			1+maxEmptyResponseRetries,
		)
	}
	if provider.callCarried(2, reasoningCapNudgeMarker) {
		t.Error("a retry below the output limit must not carry the output-cap nudge")
	}
}

// A visible reply that hit the output limit never reaches the empty-response
// path: what the user can read is delivered as before, whatever its size.
func TestVisibleReplyAtOutputCapIsDeliveredUntouched(t *testing.T) {
	al, provider := newScriptedLoop(t, providers.LLMResponse{
		Content:          "a long answer that ran out",
		ReasoningContent: "some thinking",
		FinishReason:     "length",
		Usage:            &providers.UsageInfo{CompletionTokens: config.DefaultConfig().Agents.Defaults.MaxTokens},
	})

	resp, err := al.ProcessDirect(context.Background(), "hello", "visible-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "a long answer that ran out" {
		t.Errorf("response = %q, want the visible reply", resp)
	}
	if got := provider.calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}
