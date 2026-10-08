package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const conversationSession = "sk_v1_web_conversation"

// countingReplyProvider answers every call and counts them.
type countingReplyProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *countingReplyProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return &providers.LLMResponse{Content: "background result noted"}, nil
}

func (p *countingReplyProvider) GetDefaultModel() string { return "mock-model" }

func (p *countingReplyProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newSystemMessageTestLoop(
	t *testing.T,
	provider providers.LLMProvider,
) (*AgentLoop, *bus.MessageBus, session.SessionStore) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	t.Cleanup(al.Close)
	sessions := al.registry.GetDefaultAgent().Sessions
	sessions.AddMessage(conversationSession, "user", "rewrite the project")
	sessions.AddMessage(conversationSession, "assistant", "Started a background task.")
	return al, msgBus, sessions
}

func spawnResultMessage(sessionKey string) bus.InboundMessage {
	return bus.NormalizeInboundMessage(bus.InboundMessage{
		Context:    bus.InboundContext{Channel: "system", ChatID: "grpc:run-1", SenderID: "async:spawn"},
		Content:    "Spawn failed: subagent exceeded its 20 min limit",
		SessionKey: sessionKey,
	})
}

// lateAsyncTool keeps its callback until the test fires it, as a spawn that
// finishes after the turn that launched it has ended.
type lateAsyncTool struct {
	mu   sync.Mutex
	ctx  context.Context
	done tools.AsyncCallback
}

func (t *lateAsyncTool) Name() string               { return "late_async" }
func (t *lateAsyncTool) Description() string        { return "async tool that completes later" }
func (t *lateAsyncTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t *lateAsyncTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.AsyncResult("started")
}

func (t *lateAsyncTool) ExecuteAsync(ctx context.Context, _ map[string]any, cb tools.AsyncCallback) *tools.ToolResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ctx, t.done = ctx, cb
	return tools.AsyncResult("started")
}

func (t *lateAsyncTool) finish(result string) {
	t.mu.Lock()
	ctx, cb := t.ctx, t.done
	t.mu.Unlock()
	cb(ctx, &tools.ToolResult{ForLLM: result})
}

// The reported case: the result of a spawn launched in a web conversation
// opened a turn in the assistant's main session, which went on editing the same
// project in parallel with the conversation and never showed up in it.
func TestAsyncToolResult_CarriesTheLaunchingSession(t *testing.T) {
	provider := &toolCallProvider{
		toolCalls: []providers.ToolCall{{
			ID: "call_async_1", Type: "function", Name: "late_async",
			Function:  &providers.FunctionCall{Name: "late_async", Arguments: "{}"},
			Arguments: map[string]any{},
		}},
		finalResp: "async launched",
	}
	al, msgBus, _ := newSystemMessageTestLoop(t, provider)
	tool := &lateAsyncTool{}
	al.RegisterTool(tool)

	if _, err := al.runAgentLoop(context.Background(), al.registry.GetDefaultAgent(), processOptions{
		SessionKey:      conversationSession,
		Channel:         "grpc",
		ChatID:          "run-1",
		UserMessage:     "run async tool",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	tool.finish("background result")

	select {
	case msg := <-msgBus.InboundChan():
		if msg.Channel != "system" || msg.SessionKey != conversationSession {
			t.Fatalf("published %s message for session %q, want system for %q",
				msg.Channel, msg.SessionKey, conversationSession)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("async result was not published")
	}
}

func TestProcessSystemMessage_RunsInTheLaunchingSession(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	mainKey := session.BuildMainSessionKey(al.registry.GetDefaultAgent().ID)

	if _, err := al.processSystemMessage(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	history := sessions.GetHistory(conversationSession)
	if len(history) != 4 {
		t.Fatalf("conversation history has %d messages, want the result and the reply appended", len(history))
	}
	if !strings.HasPrefix(history[2].Content, "[System: async:spawn] Spawn failed") {
		t.Errorf("result entered the conversation as %q", history[2].Content)
	}
	if got := len(sessions.GetHistory(mainKey)); got != 0 {
		t.Errorf("main session got %d messages, want none", got)
	}
}

// Messages from before the session key was carried keep landing in main.
func TestProcessSystemMessage_WithoutSessionFallsBackToMain(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	mainKey := session.BuildMainSessionKey(al.registry.GetDefaultAgent().ID)

	if _, err := al.processSystemMessage(context.Background(), spawnResultMessage("")); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	if got := len(sessions.GetHistory(mainKey)); got == 0 {
		t.Fatal("main session got nothing")
	}
	if got := len(sessions.GetHistory(conversationSession)); got != 2 {
		t.Fatalf("conversation history changed to %d messages", got)
	}
}

// While the conversation has a live turn, the result joins that turn as a
// queued message instead of opening a second turn on the same history.
func TestRun_SystemMessageForABusySessionJoinsTheLiveTurn(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.registerActiveTurn(&turnState{turnID: "turn-7", sessionKey: conversationSession})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = al.Run(ctx) }()

	if err := msgBus.PublishInbound(ctx, spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for al.pendingSteeringCountForScope(conversationSession) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the result was not queued for the live turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued := al.dequeueSteeringMessagesForScope(conversationSession)
	if len(queued) != 1 || !strings.HasPrefix(queued[0].Content, "[System: async:spawn] Spawn failed") {
		t.Fatalf("queued %+v, want the marked result", queued)
	}
	if provider.count() != 0 {
		t.Fatalf("a second turn ran (%d model calls)", provider.count())
	}
	if got := len(sessions.GetHistory(conversationSession)); got != 2 {
		t.Fatalf("conversation history changed to %d messages", got)
	}
}

// A spawn launched by a sub-turn belongs to the conversation the sub-turn
// works for, not to the sub-turn's throwaway session.
func TestTurnState_OriginSessionKeyIsTheRootTurns(t *testing.T) {
	root := &turnState{sessionKey: conversationSession}
	child := &turnState{sessionKey: "subturn-1", parentTurnState: root}
	grandchild := &turnState{sessionKey: "subturn-2", parentTurnState: child}

	if got := grandchild.originSessionKey(); got != conversationSession {
		t.Fatalf("originSessionKey = %q, want %q", got, conversationSession)
	}
}
