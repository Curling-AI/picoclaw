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

func startRunLoop(t *testing.T, al *AgentLoop) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = al.Run(ctx) }()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pendingNotes(al *AgentLoop, sessionKey string) int {
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	return len(al.mirror.pending[sessionKey])
}

// webResolver maps chats the way the Ethos gateway does: a web run id is no
// chat with a conversation of its own.
func webResolver(channel, _ string) string {
	if channel == "telegram" {
		return conversationSession
	}
	return ""
}

// A second turn on a conversation that is already in one interleaves with it;
// the result waits for the live turn to end and enters the history then.
func TestRun_ResultForABusyConversationIsWrittenWhenItsTurnEnds(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	live := &turnState{turnID: "turn-7", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	startRunLoop(t, al)

	if err := msgBus.PublishInbound(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "the result to wait for the live turn", func() bool {
		return pendingNotes(al, conversationSession) == 1
	})
	if got := len(sessions.GetHistory(conversationSession)); got != 2 {
		t.Fatalf("history changed to %d messages during the live turn", got)
	}

	al.clearActiveTurn(live)

	history := sessions.GetHistory(conversationSession)
	if len(history) != 3 || !strings.HasPrefix(history[2].Content, "[System: async:spawn] Spawn failed") {
		t.Fatalf("history after the turn = %+v, want the result appended", history)
	}
	if provider.count() != 0 {
		t.Fatalf("a turn ran for the result (%d model calls)", provider.count())
	}
}

// The reported case: a web run's stream ends with the run, so a turn for the
// result would act unseen and race the user's next message. The result is
// written for the next turn instead.
func TestRun_WebResultForAnIdleConversationIsWrittenWithoutATurn(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	startRunLoop(t, al)

	if err := msgBus.PublishInbound(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	// A turn would hold the session while it appends the result and its reply.
	waitFor(t, "the result in an idle conversation", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 3 && al.getActiveTurnState(conversationSession) == nil
	})
	if got := sessions.GetHistory(conversationSession)[2]; got.Role != "user" ||
		!strings.HasPrefix(got.Content, "[System: async:spawn] Spawn failed") {
		t.Fatalf("recorded %s %q", got.Role, got.Content)
	}
	if provider.count() != 0 {
		t.Fatalf("a turn ran for the result (%d model calls)", provider.count())
	}
}

// A chat that receives replies (Telegram) gets a turn of its own conversation,
// and the session is released afterwards.
func TestRun_ChatResultForAnIdleConversationRunsATurnThere(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	startRunLoop(t, al)

	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "the turn to end and release the session", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 4 && al.getActiveTurnState(conversationSession) == nil
	})
	if provider.count() != 1 {
		t.Fatalf("model calls = %d, want one turn", provider.count())
	}
}

// A conversation deleted while the work ran is not recreated with only the
// result in it.
func TestProcessSystemMessage_DeletedConversationFallsBackToMain(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	sessions.SetHistory(conversationSession, nil)
	mainKey := session.BuildMainSessionKey(al.registry.GetDefaultAgent().ID)

	if _, err := al.processSystemMessage(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	if got := len(sessions.GetHistory(conversationSession)); got != 0 {
		t.Fatalf("deleted conversation came back with %d messages", got)
	}
	if got := len(sessions.GetHistory(mainKey)); got == 0 {
		t.Fatal("main session got nothing")
	}
}

func TestBackgroundResultTarget_SkipsInternalOrigins(t *testing.T) {
	al, _, _ := newSystemMessageTestLoop(t, &countingReplyProvider{})
	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "cli:direct", "cli:direct"

	if _, _, ok := al.backgroundResultTarget(msg); ok {
		t.Fatal("an internal origin was routed to the conversation")
	}
}

// A result run as a turn of its conversation drains what was queued for the
// conversation meanwhile, like any other turn.
func TestBuildContinuationTarget_ResultTurnDrainsItsConversation(t *testing.T) {
	al, _, _ := newSystemMessageTestLoop(t, &countingReplyProvider{})
	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"

	target, err := al.buildContinuationTarget(msg)
	if err != nil {
		t.Fatalf("buildContinuationTarget: %v", err)
	}
	want := continuationTarget{SessionKey: conversationSession, Channel: "telegram", ChatID: "123"}
	if target == nil || *target != want {
		t.Fatalf("target = %+v, want the conversation and its chat", target)
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
