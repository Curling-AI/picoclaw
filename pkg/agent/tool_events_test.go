package agent

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// Tool events for channels that render tool cards per chat. (seucaranguejo
// fork — MST-957)

// toolEventsDelegate is a channel manager whose channels render tool cards
// for the chats in toolEvents, keyed by "channel:chatID".
type toolEventsDelegate struct {
	toolEvents map[string]bool
}

func (toolEventsDelegate) GetStreamer(context.Context, string, string, string) (bus.Streamer, bool) {
	return nil, false
}

func (d toolEventsDelegate) ToolEventsFor(channel, chatID string) bool {
	return d.toolEvents[channel+":"+chatID]
}

// toolEventsProvider asks, with reasoning and narration, for one call of each
// tool in toolNames (ids call_1, call_2, …) with args, then answers "done". It
// keeps the tools and the messages of each request.
type toolEventsProvider struct {
	toolNames []string
	args      map[string]any
	requests  [][]providers.ToolDefinition
	messages  [][]providers.Message
}

func (p *toolEventsProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.requests = append(p.requests, defs)
	p.messages = append(p.messages, messages)
	if len(p.requests) > 1 {
		return &providers.LLMResponse{Content: "done"}, nil
	}
	calls := make([]providers.ToolCall, 0, len(p.toolNames))
	for i, name := range p.toolNames {
		calls = append(calls, providers.ToolCall{
			ID:        fmt.Sprintf("call_%d", i+1),
			Type:      "function",
			Name:      name,
			Arguments: maps.Clone(p.args),
		})
	}
	return &providers.LLMResponse{
		Content:          "checking",
		ReasoningContent: "thinking it over",
		ToolCalls:        calls,
	}, nil
}

func (p *toolEventsProvider) GetDefaultModel() string { return "tool-events-model" }

// failingEventTool always fails.
type failingEventTool struct{}

func (failingEventTool) Name() string        { return "failing_event_tool" }
func (failingEventTool) Description() string { return "Always fails" }
func (failingEventTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (failingEventTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.ErrorResult("boom")
}

// emptyEventTool succeeds without output.
type emptyEventTool struct{}

func (emptyEventTool) Name() string        { return "empty_event_tool" }
func (emptyEventTool) Description() string { return "Succeeds without output" }
func (emptyEventTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (emptyEventTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.SilentResult("")
}

// failingRespondHook answers the named tool itself, as a failure.
type failingRespondHook struct{ tool string }

func (h failingRespondHook) BeforeTool(
	_ context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	if call.Tool != h.tool {
		return call, HookDecision{Action: HookActionContinue}, nil
	}
	next := call.Clone()
	next.HookResult = &tools.ToolResult{ForLLM: "hook refused", IsError: true}
	return next, HookDecision{Action: HookActionRespond}, nil
}

func (failingRespondHook) AfterTool(
	_ context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}

func newToolEventsTestLoop(
	t *testing.T,
	delegate bus.StreamDelegate,
	provider *toolEventsProvider,
) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	if delegate != nil {
		msgBus.SetStreamDelegate(delegate)
	}
	al := NewAgentLoop(cfg, msgBus, provider)
	agent := al.GetRegistry().GetDefaultAgent()
	agent.Tools.Register(&toolLimitTestTool{})
	agent.Tools.Register(failingEventTool{})
	agent.Tools.Register(emptyEventTool{})
	return al, msgBus
}

// runWebhookTurn runs a turn the way the pod's /message does.
func runWebhookTurn(t *testing.T, al *AgentLoop, channel, chatID string) {
	t.Helper()
	if _, err := al.ProcessDirectWithMedia(
		context.Background(), "go", "agent:main:"+channel+":direct:"+chatID, channel, chatID, nil,
	); err != nil {
		t.Fatalf("ProcessDirectWithMedia() error = %v", err)
	}
}

func drainOutboundMessages(msgBus *bus.MessageBus) []bus.OutboundMessage {
	var out []bus.OutboundMessage
	for {
		select {
		case msg := <-msgBus.OutboundChan():
			out = append(out, msg)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func outboundKinds(msgs []bus.OutboundMessage) []string {
	kinds := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		kinds = append(kinds, msg.Context.Raw[metadataKeyMessageKind])
	}
	return kinds
}

func TestToolEventsChannelGetsToolCallsAndResultsOnly(t *testing.T) {
	provider := &toolEventsProvider{toolNames: []string{"tool_limit_test_tool"}, args: map[string]any{"value": "x"}}
	delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
	al, msgBus := newToolEventsTestLoop(t, delegate, provider)

	runWebhookTurn(t, al, "slack", "C1")

	out := drainOutboundMessages(msgBus)
	if got := strings.Join(outboundKinds(out), ","); got != "tool_calls,tool_result" {
		t.Fatalf("outbound kinds = %q, want tool_calls,tool_result (no reasoning, no narration)", got)
	}
	calls := out[0]
	if calls.Context.Channel != "slack" || calls.ChatID != "C1" {
		t.Fatalf("tool_calls went to %s/%s, want slack/C1", calls.Context.Channel, calls.ChatID)
	}
	if raw := calls.Context.Raw[metadataKeyToolCalls]; !strings.Contains(raw, `"id":"call_1"`) ||
		!strings.Contains(raw, "tool_limit_test_tool") {
		t.Fatalf("tool_calls payload = %s, want call_1 of tool_limit_test_tool", raw)
	}
	result := out[1]
	if result.Context.Raw[metadataKeyToolCallID] != "call_1" {
		t.Fatalf("tool_result tool_call_id = %q, want call_1", result.Context.Raw[metadataKeyToolCallID])
	}
	if result.Content != "" {
		t.Fatalf("tool_result content = %q, want none: the card shows no output", result.Content)
	}
	if _, marked := result.Context.Raw[metadataKeyToolIsError]; marked {
		t.Fatalf("tool_result of a successful call carries %s", metadataKeyToolIsError)
	}
}

func TestToolEventsStayOffWhereTheChannelSaysNo(t *testing.T) {
	for name, delegate := range map[string]bus.StreamDelegate{
		"no delegate":      nil,
		"another chat":     toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}},
		"delegate says no": toolEventsDelegate{},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &toolEventsProvider{
				toolNames: []string{"tool_limit_test_tool"},
				args:      map[string]any{"value": "x"},
			}
			al, msgBus := newToolEventsTestLoop(t, delegate, provider)

			runWebhookTurn(t, al, "slack", "C2")

			if out := drainOutboundMessages(msgBus); len(out) != 0 {
				t.Fatalf("outbound = %v, want none", outboundKinds(out))
			}
		})
	}
}

func TestToolEventsResultMarksAFailedCall(t *testing.T) {
	for _, channel := range []string{"slack", "pico"} {
		t.Run(channel, func(t *testing.T) {
			provider := &toolEventsProvider{
				toolNames: []string{"failing_event_tool"},
				args:      map[string]any{"value": "x"},
			}
			delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
			al, msgBus := newToolEventsTestLoop(t, delegate, provider)

			runWebhookTurn(t, al, channel, "C1")

			for _, msg := range drainOutboundMessages(msgBus) {
				if msg.Context.Raw[metadataKeyMessageKind] != messageKindToolResult {
					continue
				}
				if msg.Context.Raw[metadataKeyToolIsError] != "true" {
					t.Fatalf("tool_result raw = %v, want %s=true", msg.Context.Raw, metadataKeyToolIsError)
				}
				return
			}
			t.Fatal("no tool_result published")
		})
	}
}

// A hook that answers the call itself publishes the same tool_result.
func TestToolEventsResultOfAHookAnsweredCall(t *testing.T) {
	provider := &toolEventsProvider{toolNames: []string{"tool_limit_test_tool"}, args: map[string]any{"value": "x"}}
	delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
	al, msgBus := newToolEventsTestLoop(t, delegate, provider)
	if err := al.MountHook(NamedHook("failing-respond", failingRespondHook{tool: "tool_limit_test_tool"})); err != nil {
		t.Fatalf("MountHook() error = %v", err)
	}

	runWebhookTurn(t, al, "slack", "C1")

	out := drainOutboundMessages(msgBus)
	if got := strings.Join(outboundKinds(out), ","); got != "tool_calls,tool_result" {
		t.Fatalf("outbound kinds = %q, want tool_calls,tool_result", got)
	}
	if out[1].Context.Raw[metadataKeyToolCallID] != "call_1" || out[1].Context.Raw[metadataKeyToolIsError] != "true" {
		t.Fatalf("tool_result raw = %v, want call_1 marked as failed", out[1].Context.Raw)
	}
}

// The card waits for its result even when the tool says nothing; the web
// channels keep skipping an empty one.
func TestToolEventsResultOfAnEmptyCall(t *testing.T) {
	for channel, want := range map[string]string{"slack": "tool_calls,tool_result", "pico": "thought,tool_calls"} {
		t.Run(channel, func(t *testing.T) {
			provider := &toolEventsProvider{toolNames: []string{"empty_event_tool"}, args: map[string]any{"value": "x"}}
			delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
			al, msgBus := newToolEventsTestLoop(t, delegate, provider)

			runWebhookTurn(t, al, channel, "C1")

			if got := strings.Join(outboundKinds(drainOutboundMessages(msgBus)), ","); got != want {
				t.Fatalf("outbound kinds = %q, want %q", got, want)
			}
		})
	}
}

func TestToolEventsWantedOnlyForTheConversationsOwnTurn(t *testing.T) {
	msgBus := bus.NewMessageBus()
	msgBus.SetStreamDelegate(toolEventsDelegate{toolEvents: map[string]bool{
		"slack:C1": true, "pico:C1": true, "grpc:C1": true,
	}})
	al := &AgentLoop{bus: msgBus}
	turn := func(edit func(*turnState)) *turnState {
		ts := &turnState{
			channel:    "slack",
			chatID:     "C1",
			sessionKey: "agent:main:slack:direct:c1",
			opts:       processOptions{AllowInterimPicoPublish: true},
		}
		if edit != nil {
			edit(ts)
		}
		return ts
	}

	cases := map[string]struct {
		ts   *turnState
		want bool
	}{
		"conversation turn":     {turn(nil), true},
		"another chat":          {turn(func(ts *turnState) { ts.chatID = "C2" }), false},
		"sub-turn":              {turn(func(ts *turnState) { ts.parentTurnID = "turn-1" }), false},
		"no interim output":     {turn(func(ts *turnState) { ts.opts.AllowInterimPicoPublish = false }), false},
		"automation run":        {turn(func(ts *turnState) { ts.sessionKey = "agent:cron-job-1-run-1" }), false},
		"structured web (pico)": {turn(func(ts *turnState) { ts.channel = "pico" }), false},
		"structured web (grpc)": {turn(func(ts *turnState) { ts.channel = "grpc" }), false},
		"no chat":               {turn(func(ts *turnState) { ts.chatID = "" }), false},
	}
	for name, tc := range cases {
		if got := al.toolEventsWanted(tc.ts); got != tc.want {
			t.Errorf("%s: toolEventsWanted = %v, want %v", name, got, tc.want)
		}
	}
	if (&AgentLoop{}).toolEventsWanted(turn(nil)) {
		t.Error("loop without a bus: toolEventsWanted = true, want false")
	}
}

// recordingToolEventsChannel renders tool cards for chat C1 and records what
// reaches its Send.
type recordingToolEventsChannel struct {
	mu   sync.Mutex
	sent []bus.OutboundMessage
}

func (*recordingToolEventsChannel) Name() string                        { return "slack" }
func (*recordingToolEventsChannel) Start(context.Context) error         { return nil }
func (*recordingToolEventsChannel) Stop(context.Context) error          { return nil }
func (*recordingToolEventsChannel) IsRunning() bool                     { return true }
func (*recordingToolEventsChannel) IsAllowed(string) bool               { return true }
func (*recordingToolEventsChannel) IsAllowedSender(bus.SenderInfo) bool { return true }
func (*recordingToolEventsChannel) ReasoningChannelID() string          { return "" }
func (*recordingToolEventsChannel) ToolEventsFor(chatID string) bool    { return chatID == "C1" }

func (c *recordingToolEventsChannel) Send(_ context.Context, msg bus.OutboundMessage) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil, nil
}

// The events go to the registered channel straight from the turn, before it
// returns, and not through the bus.
func TestToolEventsReachTheRegisteredChannelOnTheTurn(t *testing.T) {
	provider := &toolEventsProvider{toolNames: []string{"failing_event_tool"}, args: map[string]any{"value": "x"}}
	al, msgBus := newToolEventsTestLoop(t, nil, provider)
	manager, err := channels.NewManager(al.cfg, msgBus, nil)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	channel := &recordingToolEventsChannel{}
	manager.RegisterChannel("slack", channel)
	al.SetChannelManager(manager)

	runWebhookTurn(t, al, "slack", "C1")

	channel.mu.Lock()
	sent := append([]bus.OutboundMessage(nil), channel.sent...)
	channel.mu.Unlock()
	if got := strings.Join(outboundKinds(sent), ","); got != "tool_calls,tool_result" {
		t.Fatalf("channel got %q, want tool_calls,tool_result", got)
	}
	if sent[1].Context.Raw[metadataKeyToolIsError] != "true" || sent[1].ChatID != "C1" {
		t.Fatalf("tool_result = %+v, want chat C1 marked as failed", sent[1])
	}
	if out := drainOutboundMessages(msgBus); len(out) != 0 {
		t.Fatalf("bus got %v, want nothing", outboundKinds(out))
	}
}

func TestPublishToolResultEvent(t *testing.T) {
	long := strings.Repeat("r", maxToolResultEventLen)
	cases := []struct {
		name       string
		channel    string
		toolEvents bool
		interim    bool
		content    string
		isError    bool
		want       string // content of the tool_result, or "-" for none
		wantError  bool
	}{
		{name: "grpc gets the trimmed text", channel: "grpc", interim: true, content: "  out  ", want: "out"},
		{name: "pico gets the text", channel: "pico", interim: true, content: "out", want: "out"},
		{name: "text at the limit stays whole", channel: "grpc", interim: true, content: long, want: long},
		{name: "longer text is cut", channel: "grpc", interim: true, content: long + "rr", want: long + "…"},
		{name: "empty text skips the web", channel: "grpc", interim: true, content: "  ", want: "-"},
		{name: "no interim output", channel: "grpc", content: "out", want: "-"},
		{
			name:       "card channel gets no text",
			channel:    "slack",
			toolEvents: true,
			interim:    true,
			content:    "out",
			want:       "",
		},
		{name: "card channel gets an empty result", channel: "slack", toolEvents: true, interim: true, want: ""},
		{name: "other channels get nothing", channel: "slack", interim: true, content: "out", want: "-"},
		{
			name:      "a failed call is marked",
			channel:   "grpc",
			interim:   true,
			content:   "boom",
			isError:   true,
			want:      "boom",
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgBus := bus.NewMessageBus()
			al := &AgentLoop{bus: msgBus}
			ts := &turnState{
				agent:      &AgentInstance{ID: "main"},
				channel:    tc.channel,
				chatID:     "C1",
				toolEvents: tc.toolEvents,
				opts:       processOptions{AllowInterimPicoPublish: tc.interim},
			}

			al.publishToolResultEvent(context.Background(), ts, "call_1", tc.content, tc.isError)

			out := drainOutboundMessages(msgBus)
			if tc.want == "-" {
				if len(out) != 0 {
					t.Fatalf("published %d messages, want none", len(out))
				}
				return
			}
			if len(out) != 1 || out[0].Context.Raw[metadataKeyMessageKind] != messageKindToolResult {
				t.Fatalf("published %v, want one tool_result", outboundKinds(out))
			}
			if out[0].Content != tc.want || out[0].Context.Raw[metadataKeyToolCallID] != "call_1" {
				t.Fatalf("tool_result = %q for %q, want %q for call_1",
					out[0].Content, out[0].Context.Raw[metadataKeyToolCallID], tc.want)
			}
			if got := out[0].Context.Raw[metadataKeyToolIsError] == "true"; got != tc.wantError {
				t.Fatalf("%s = %v, want %v", metadataKeyToolIsError, got, tc.wantError)
			}
		})
	}
}
