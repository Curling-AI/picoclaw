package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// End-to-end checks, through processMessage, of how tool results are kept
// within the context window. They reproduce the prod failures: before the
// fix each one ended in "refusing to drop active turn messages" or in the
// tool running again until max_tool_iterations.

// The upstream's answer when the request is bigger than the model's window
// (Vercel AI Gateway, observed in prod).
var errInputExceedsContextWindow = errors.New("API request failed:\n  Status: 400\n  Body:   " +
	`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`)

// bigOutput is prose, not one long token (the registry already swaps
// base64-like payloads for a note), numbered so a test can tell results apart.
func bigOutput(size, n int) string {
	head := fmt.Sprintf("HEAD-OF-OUTPUT-%03d ", n)
	const tail, filler = " TAIL-OF-OUTPUT", "lorem ipsum dolor sit amet, "
	body := strings.Repeat(filler, size/len(filler)+1)[:size-len(head)-len(tail)]
	return head + body + tail
}

// bigOutputTool answers its n-th call with bigOutput(size, n), after onCall
// when set.
type bigOutputTool struct {
	size   int
	calls  atomic.Int32
	onCall func(ctx context.Context, n int)
}

func (t *bigOutputTool) Name() string        { return "big_output" }
func (t *bigOutputTool) Description() string { return "Returns a large output" }
func (t *bigOutputTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *bigOutputTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	n := int(t.calls.Add(1))
	if t.onCall != nil {
		t.onCall(ctx, n)
	}
	return tools.NewToolResult(bigOutput(t.size, n))
}

// windowedToolProvider asks for big_output toolRounds times, then answers.
// Like the gateway, it rejects any call estimated above window.
type windowedToolProvider struct {
	mu         sync.Mutex
	window     int
	toolRounds int
	callID     func(round int) string // the id of each tool call; "call-N" by default
	calls      []windowedCall
}

type windowedCall struct {
	tokens   int
	rejected bool
	turn     bool // carries tool definitions: a call of the turn, not a summary
	content  string
}

func (p *windowedToolProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	_ string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := windowedCall{tokens: EstimateToolDefsTokens(toolDefs), turn: len(toolDefs) > 0}
	if maxTokens, ok := opts["max_tokens"].(int); ok {
		call.tokens += maxTokens
	}
	toolResults := 0
	for _, m := range messages {
		call.tokens += EstimateMessageTokens(m)
		call.content += m.Content + "\n"
		if m.Role == "tool" {
			toolResults++
		}
	}
	call.rejected = p.window > 0 && call.tokens > p.window
	p.calls = append(p.calls, call)
	if call.rejected {
		return nil, errInputExceedsContextWindow
	}
	if toolResults < p.toolRounds {
		id := fmt.Sprintf("call-%d", toolResults+1)
		if p.callID != nil {
			id = p.callID(toolResults + 1)
		}
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        id,
			Name:      "big_output",
			Arguments: map[string]any{},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *windowedToolProvider) GetDefaultModel() string { return "windowed-model" }

func (p *windowedToolProvider) turnCalls() []windowedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []windowedCall
	for _, c := range p.calls {
		if c.turn {
			out = append(out, c)
		}
	}
	return out
}

type windowedTurn struct {
	al       *AgentLoop
	provider *windowedToolProvider
	agent    *AgentInstance
	tool     *bigOutputTool
	base     int // tokens of a turn's first call
}

// newWindowedTurn builds an agent whose provider and context window agree on
// base + headroom tokens. base is measured on a throwaway turn, so the tests
// do not depend on the size of the system prompt or of the tool set.
func newWindowedTurn(t *testing.T, cfg *config.Config, tool *bigOutputTool, headroom int) *windowedTurn {
	t.Helper()
	provider := &windowedToolProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(tool)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("no default agent")
	}
	if _, err := al.ProcessDirectWithChannel(
		context.Background(),
		"ping",
		"sk_v1_calibration",
		"test",
		"calibration",
	); err != nil {
		t.Fatalf("calibration turn: %v", err)
	}
	base := provider.turnCalls()[0].tokens
	provider.mu.Lock()
	provider.calls = nil
	provider.window = base + headroom
	provider.mu.Unlock()
	agent.ContextWindow = base + headroom
	return &windowedTurn{al: al, provider: provider, agent: agent, tool: tool, base: base}
}

func (w *windowedTurn) run(t *testing.T, session, message string) {
	t.Helper()
	resp, err := w.al.ProcessDirectWithChannel(context.Background(), message, session, "test", session)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if resp != "done" {
		t.Fatalf("response = %q, want done", resp)
	}
}

func windowedConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 48,
			},
		},
	}
}

func toolMessages(history []providers.Message) []providers.Message {
	var out []providers.Message
	for _, m := range history {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// Prod, 14-15/10: one browser snapshot of 3.2M characters made the active turn
// alone larger than the model's window. A result past the limit now enters the
// turn trimmed, start and end, with the full output saved in the workspace.
func TestOversizedToolResult_IsTrimmedBeforeItEntersTheTurn(t *testing.T) {
	cfg := windowedConfig(t)
	turn := newWindowedTurn(t, cfg, &bigOutputTool{size: 1_000_000}, 60_000)
	turn.provider.toolRounds = 1

	turn.run(t, "sk_v1_oversized", "take a snapshot")

	for i, c := range turn.provider.turnCalls() {
		if c.rejected {
			t.Fatalf("call %d (%d tokens) was over the window: the result reached the model untrimmed", i, c.tokens)
		}
	}
	results := toolMessages(turn.agent.Sessions.GetHistory("sk_v1_oversized"))
	if len(results) != 1 {
		t.Fatalf("tool results in history = %d, want 1", len(results))
	}
	got := results[0].Content
	if n := len([]rune(got)); n > config.DefaultToolMaxResultChars {
		t.Fatalf("stored result has %d characters, want at most %d", n, config.DefaultToolMaxResultChars)
	}
	for _, want := range []string{"HEAD-OF-OUTPUT-001", "TAIL-OF-OUTPUT", "1000000 characters"} {
		if !strings.Contains(got, want) {
			t.Fatalf("trimmed result lacks %q:\n%.400s", want, got)
		}
	}
	saved, err := filepath.Glob(
		filepath.Join(cfg.Agents.Defaults.Workspace, "artifacts", toolOutputDir, "big_output_*.txt"),
	)
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved outputs = %v (err %v), want exactly one", saved, err)
	}
	if savedToolOutputPath(got) != saved[0] {
		t.Fatalf("trimmed result does not point at the saved file %s", saved[0])
	}
	if full, _ := os.ReadFile(saved[0]); string(full) != bigOutput(1_000_000, 1) {
		t.Fatalf("saved output has %d bytes, want the full 1000000", len(full))
	}
}

// Prod, 13/10: 258 web_search results of up to 16K characters each, all in one
// turn. No single result is large, but together they outgrew the window. The
// retry now stands notes in for the earliest results of the turn, in what it
// sends only, and the turn goes on.
func TestAccumulatedToolResults_EarliestAreElidedAndTheTurnGoesOn(t *testing.T) {
	const resultSize, rounds = 2_600, 32
	for _, ids := range []struct {
		name   string
		callID func(int) string
	}{
		{"distinct call ids", nil},
		// Text-lifted calls number their ids per response ("xml_call_0" every
		// time): an elision must still hit one result, never its namesakes.
		{"call ids reused across responses", func(int) string { return "xml_call_0" }},
	} {
		t.Run(ids.name, func(t *testing.T) {
			perRound := EstimateMessageTokens(
				providers.Message{Role: "tool", ToolCallID: "call-10", Content: bigOutput(resultSize, 1)},
			) + 40
			turn := newWindowedTurn(t, windowedConfig(t), &bigOutputTool{size: resultSize}, 25*perRound)
			turn.provider.toolRounds = rounds
			turn.provider.callID = ids.callID

			turn.run(t, "sk_v1_accumulated", "search a lot")

			if n := turn.tool.calls.Load(); n != rounds {
				t.Fatalf("tool ran %d times, want %d: a result was lost and the model asked again", n, rounds)
			}
			calls := turn.provider.turnCalls()
			elisions := 0
			for i := 0; i+2 < len(calls); i++ {
				if !calls[i].rejected || calls[i+1].rejected {
					continue
				}
				// calls[i+1] is the retry that elided. The elision rides along
				// on the turn's next call (carry), and its headroom leaves that
				// call room for the new result.
				elisions++
				next := calls[i+2]
				if next.rejected || !strings.Contains(next.content, elidedToolResultMarker) {
					t.Fatalf("call %d, the first after an elision: rejected=%v, carries notes=%v",
						i+2, next.rejected, strings.Contains(next.content, elidedToolResultMarker))
				}
			}
			if elisions == 0 {
				t.Fatal("no call was sent with elided results")
			}
			last := calls[len(calls)-1]
			if !strings.Contains(last.content, bigOutput(resultSize, rounds)) {
				t.Fatal("the final call lost the newest result")
			}
			if strings.Contains(last.content, "HEAD-OF-OUTPUT-001") {
				t.Fatal("the final call still carries the oldest result: elision must start there")
			}

			// The elision is a view of the call: the session keeps every result.
			results := toolMessages(turn.agent.Sessions.GetHistory("sk_v1_accumulated"))
			if len(results) != rounds {
				t.Fatalf("tool results in history = %d, want %d", len(results), rounds)
			}
			for i, m := range results {
				if m.Content != bigOutput(resultSize, i+1) {
					t.Fatalf("history result %d was altered (%d characters)", i+1, len(m.Content))
				}
			}
		})
	}
}

// Round 1's script also posted a report to the chat: the history has it after
// round 1's result, and every call has it inside that result. Counting it as a
// message of the turn moved the turn's start one message early, so the retry's
// elision of round 1 hit nothing, and with nothing else to elide the turn
// failed with "context window still exceeded". (seucaranguejo fork)
func TestContextRetry_ElidesTheRoundBeforeASelfSend(t *testing.T) {
	const resultSize, rounds = 2_600, 32
	perRound := EstimateMessageTokens(
		providers.Message{Role: "tool", ToolCallID: "call-10", Content: bigOutput(resultSize, 1)},
	) + 40
	tool := &bigOutputTool{size: resultSize}
	turn := newWindowedTurn(t, windowedConfig(t), tool, 25*perRound)
	turn.provider.toolRounds = rounds
	var chat string
	turn.al.SetDeliverySessionResolver(func(string, string) string { return chat })
	tool.onCall = func(ctx context.Context, n int) {
		if n != 1 {
			return
		}
		origin, turnID := tools.ExecOrigin(ctx)
		chat = origin
		turn.al.MirrorDelivery(tools.ExternalDelivery{
			Origin: origin, TurnID: turnID, Channel: "test", ChatID: "report", Text: "Relatório da rodada 1.",
		})
	}

	turn.run(t, "sk_v1_selfsend", "search a lot")

	calls := turn.provider.turnCalls()
	last := calls[len(calls)-1]
	if strings.Contains(last.content, "HEAD-OF-OUTPUT-001") {
		t.Fatal("the final call still carries round 1's result: its elision missed")
	}
	if !strings.Contains(last.content, "Relatório da rodada 1.") {
		t.Fatal("the final call lost the report round 1's script sent")
	}
	history := turn.agent.Sessions.GetHistory("sk_v1_selfsend")
	if len(toolMessages(history)) != rounds {
		t.Fatalf("tool results in history = %d, want %d", len(toolMessages(history)), rounds)
	}
}

// The retry guard measured the active turn on the history read at the start
// of the turn, which lacks this turn's tool results. Seeing a turn of just the
// user message, it let the destructive compaction run when that could not
// help: in prod it dropped earlier messages of the conversation each time
// ("Emergency compression dropped 12 oldest messages"), and in a conversation
// with nothing before the turn it dropped the turn's own tool call and result,
// so the model ran the tool again until max_tool_iterations.
func TestContextRetry_DoesNotCompactWhenOnlyTheActiveTurnOverflows(t *testing.T) {
	for _, earlierTurn := range []bool{true, false} {
		name := map[bool]string{true: "earlier history", false: "active turn only"}[earlierTurn]
		t.Run(name, func(t *testing.T) {
			cfg := windowedConfig(t)
			// Under the per-result limit and still past the window, like a
			// capped result on the smaller window of the image model.
			cfg.Tools.MaxResultChars = 10_000_000
			turn := newWindowedTurn(t, cfg, &bigOutputTool{size: 200_000}, 20_000)
			const session = "sk_v1_guard"
			if earlierTurn {
				turn.run(t, session, "earlier question")
			}
			turn.provider.toolRounds = 1

			turn.run(t, session, "take a snapshot")

			if n := turn.tool.calls.Load(); n != 1 {
				t.Fatalf("tool ran %d times, want 1: the turn lost its own result and asked again", n)
			}
			history := turn.agent.Sessions.GetHistory(session)
			if results := toolMessages(history); len(results) != 1 || results[0].Content != bigOutput(200_000, 1) {
				t.Fatalf("the turn's tool result is missing or altered in history (%d results)", len(results))
			}
			if !earlierTurn {
				return
			}
			if len(history) == 0 || history[0].Content != "earlier question" {
				t.Fatalf("earlier history was dropped; history starts with %+v", history[:min(1, len(history))])
			}
			// With the turn back under the window, the call keeps the
			// conversation that led to it.
			calls := turn.provider.turnCalls()
			if last := calls[len(calls)-1]; !strings.Contains(last.content, "earlier question") {
				t.Fatal("the call after the elision lost the earlier conversation")
			}
		})
	}
}

// When the upstream counts tokens heavier than our estimate (or a fallback
// model has a smaller window), it refuses calls the estimate says fit, and the
// retry used to send them again unchanged until it gave up. The window is now
// lowered to what the upstream refused, for the rest of the turn.
func TestContextRetry_AnUpstreamThatCountsHeavierStillGetsAFit(t *testing.T) {
	const resultSize, rounds = 15_000, 6
	resultTokens := EstimateMessageTokens(providers.Message{Role: "tool", Content: bigOutput(resultSize, 1)})
	turn := newWindowedTurn(t, windowedConfig(t), &bigOutputTool{size: resultSize}, resultTokens*7/2)
	turn.agent.ContextWindow = turn.base + 20*resultTokens // our side believes there is room
	turn.provider.toolRounds = rounds

	turn.run(t, "sk_v1_heavier", "search a lot")

	if n := turn.tool.calls.Load(); n != rounds {
		t.Fatalf("tool ran %d times, want %d", n, rounds)
	}
}

// bigRespondHook answers big_output itself, before the tool runs.
type bigRespondHook struct{ size int }

func (h *bigRespondHook) BeforeTool(
	_ context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	if call.Tool != "big_output" {
		return call, HookDecision{Action: HookActionContinue}, nil
	}
	next := call.Clone()
	next.HookResult = &tools.ToolResult{ForLLM: bigOutput(h.size, 1)}
	return next, HookDecision{Action: HookActionRespond}, nil
}

func (h *bigRespondHook) AfterTool(
	_ context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}

func TestOversizedHookResponse_IsTrimmedBeforeItEntersTheTurn(t *testing.T) {
	turn := newWindowedTurn(t, windowedConfig(t), &bigOutputTool{size: 10}, 60_000)
	if err := turn.al.MountHook(NamedHook("big-respond", &bigRespondHook{size: 1_000_000})); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	turn.provider.toolRounds = 1

	turn.run(t, "sk_v1_hooked", "take a snapshot")

	results := toolMessages(turn.agent.Sessions.GetHistory("sk_v1_hooked"))
	if len(results) != 1 || !strings.HasPrefix(results[0].Content, truncatedToolResultPrefix) {
		t.Fatalf("hook response entered the turn untrimmed (%d results)", len(results))
	}
	if n := len([]rune(results[0].Content)); n > config.DefaultToolMaxResultChars {
		t.Fatalf("stored hook response has %d characters, want at most %d", n, config.DefaultToolMaxResultChars)
	}
}

// An async result comes back as a new message of the conversation: it opens
// the next turn, where it cannot be compacted either.
func TestOversizedAsyncToolResult_IsTrimmedBeforeItIsQueued(t *testing.T) {
	cfg := windowedConfig(t)
	provider := &toolCallProvider{
		toolCalls: []providers.ToolCall{{
			ID:        "call_async_1",
			Type:      "function",
			Name:      "async_followup",
			Function:  &providers.FunctionCall{Name: "async_followup", Arguments: "{}"},
			Arguments: map[string]any{},
		}},
		finalResp: "async launched",
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	done := make(chan struct{})
	al.RegisterTool(
		&asyncFollowUpTool{name: "async_followup", followUpText: bigOutput(1_000_000, 1), completionSig: done},
	)
	runtimeCh, closeEvents := subscribeRuntimeEventsForTest(t, al, 32, runtimeevents.KindAgentFollowUpQueued)
	defer closeEvents()

	if _, err := al.runAgentLoop(context.Background(), al.registry.GetDefaultAgent(), processOptions{
		SessionKey:      "session-async",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run async tool",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for the async tool")
	}
	evt := waitForRuntimeEvent(t, runtimeCh, 2*time.Second, func(evt runtimeevents.Event) bool {
		return evt.Kind == runtimeevents.KindAgentFollowUpQueued
	})
	payload, ok := evt.Payload.(FollowUpQueuedPayload)
	if !ok {
		t.Fatalf("payload = %T, want FollowUpQueuedPayload", evt.Payload)
	}
	// ContentLen counts bytes and the limit counts characters: the omission
	// marker's two "…" take three bytes each.
	if payload.ContentLen > config.DefaultToolMaxResultChars+4 {
		t.Fatalf(
			"queued async result has %d bytes, want the %d-character limit",
			payload.ContentLen,
			config.DefaultToolMaxResultChars,
		)
	}
}

// transientThenRefusalProvider fails a call once with a rate limit, then once
// with a context refusal, then answers.
type transientThenRefusalProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *transientThenRefusalProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch p.calls {
	case 1:
		return nil, errors.New(
			"API request failed:\n  Status: 429\n  Body:   {\"error\":{\"message\":\"rate limited\"}}",
		)
	case 2:
		return nil, errInputExceedsContextWindow
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *transientThenRefusalProvider) GetDefaultModel() string { return "flaky-model" }

// A rate limit bumps the retry counter too. A single context refusal after
// it is still the first: it must be retried as is, not shrink the window (a
// window of 90% of a small call leaves no room for the output reserve).
func TestContextRetry_ARateLimitDoesNotCountAsARefusal(t *testing.T) {
	cfg := windowedConfig(t)
	cfg.Agents.Defaults.LLMRetryBackoffSecs = 1
	provider := &transientThenRefusalProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirectWithChannel(context.Background(), "hi", "sk_v1_flaky", "test", "flaky")
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if resp != "done" || provider.calls != 3 {
		t.Fatalf("response %q after %d calls, want done after 3", resp, provider.calls)
	}
}
