package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

func parkedResults(al *AgentLoop, sessionKey string) int {
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	return len(al.mirror.parked[sessionKey])
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

// A second turn on a conversation that is already in one interleaves with it.
// A web result waits for the live turn to end and enters the history then.
func TestRun_WebResultForABusyConversationIsWrittenWhenItsTurnEnds(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-7", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	startRunLoop(t, al)

	if err := msgBus.PublishInbound(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "the result to wait for the live turn", func() bool {
		return parkedResults(al, conversationSession) == 1
	})
	if got := len(sessions.GetHistory(conversationSession)); got != 2 {
		t.Fatalf("history changed to %d messages during the live turn", got)
	}
	if n := al.pendingSteeringCountForScope(conversationSession); n != 0 {
		t.Fatalf("the result entered the live turn's steering queue (%d)", n)
	}

	al.clearActiveTurn(live)

	waitFor(t, "the result in the history after the turn", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 3
	})
	if got := sessions.GetHistory(conversationSession)[2]; !strings.HasPrefix(
		got.Content,
		"[System: async:spawn] Spawn failed",
	) {
		t.Fatalf("appended %q, want the result", got.Content)
	}
	if provider.count() != 0 {
		t.Fatalf("a turn ran for the result (%d model calls)", provider.count())
	}
}

// In the live turn's steering queue the result could be
// lost (a direct turn never drains it, /stop clears it, the queue caps at 10)
// and it skipped the rest of the tool batch as if the user had written. It
// waits outside that queue and gets a turn of its own when the live one ends.
func TestRun_ChatResultForABusyConversationRunsAfterTheTurn(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, _ := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-7", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	startRunLoop(t, al)

	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "the result to wait for the live turn", func() bool {
		return parkedResults(al, conversationSession) == 1
	})
	// What /stop does to the live turn's queue must not reach the result.
	al.clearSteeringMessagesForScope(conversationSession)
	if provider.count() != 0 || al.pendingSteeringCountForScope(conversationSession) != 0 {
		t.Fatalf("result ran (%d calls) or entered the steering queue during the live turn", provider.count())
	}

	al.clearActiveTurn(live)

	waitFor(t, "a turn for the result after the live one", func() bool { return provider.count() == 1 })
	select {
	case out := <-msgBus.OutboundChan():
		if out.Channel != "telegram" || out.ChatID != "123" {
			t.Fatalf("reply went to %s:%s, want telegram:123", out.Channel, out.ChatID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the chat never got a reply about the result")
	}
}

// More results than the old steering queue held (10) while the turn runs: none
// is lost, and they reach the conversation in order.
func TestRun_ManyResultsForABusyConversationAllArrive(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-7", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	startRunLoop(t, al)

	const results = 15
	for i := range results {
		msg := spawnResultMessage(conversationSession)
		msg.Content = fmt.Sprintf("Task %02d completed", i)
		if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
			t.Fatalf("PublishInbound: %v", err)
		}
	}
	waitFor(t, "every result to wait", func() bool { return parkedResults(al, conversationSession) == results })
	al.clearActiveTurn(live)

	waitFor(t, "every result in the history", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 2+results
	})
	for i, m := range sessions.GetHistory(conversationSession)[2:] {
		if want := fmt.Sprintf("Task %02d completed", i); !strings.Contains(m.Content, want) {
			t.Fatalf("history[%d] = %q, want %q (order kept)", i+2, m.Content, want)
		}
	}
}

// The resolver answers "" for chats it cannot map
// (Discord, WhatsApp groups), which read as a web run and silenced them.
func TestRun_ResultForAChatTheResolverCannotMapStillGetsAReply(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, _ := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	startRunLoop(t, al)

	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "discord:999", "discord:999"
	if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "a turn for the result", func() bool { return provider.count() == 1 })
}

// With a stop pending for the conversation the worker
// returned early and dropped the result, and built the continuation from the
// system message's channel. The result is written for the next turn instead.
func TestRun_ResultForAStoppedConversationIsWrittenNotRun(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	al.markPendingStop(conversationSession)
	startRunLoop(t, al)

	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "the result in the history", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 3
	})
	if provider.count() != 0 {
		t.Fatalf("a turn ran after the stop (%d model calls)", provider.count())
	}
	select {
	case out := <-msgBus.OutboundChan():
		t.Fatalf("something was sent after the stop: %+v", out)
	case <-time.After(200 * time.Millisecond):
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
	// The result's turn answers once, in its chat.
	var replies []bus.OutboundMessage
	deadline := time.After(300 * time.Millisecond)
	for collecting := true; collecting; {
		select {
		case out := <-msgBus.OutboundChan():
			replies = append(replies, out)
		case <-deadline:
			collecting = false
		}
	}
	if len(replies) != 1 || replies[0].ChatID != "123" {
		t.Fatalf("replies = %+v, want exactly one, in the chat", replies)
	}
}

// A conversation cleared or deleted while the work ran is not recreated with
// only the result in it, and its result opens no turn in main, where it would
// act on the old task unseen.
func TestProcessSystemMessage_ResultOfAGoneConversationIsDropped(t *testing.T) {
	provider := &countingReplyProvider{}
	al, _, sessions := newSystemMessageTestLoop(t, provider)
	sessions.SetHistory(conversationSession, nil)
	mainKey := session.BuildMainSessionKey(al.registry.GetDefaultAgent().ID)

	if _, err := al.processSystemMessage(context.Background(), spawnResultMessage(conversationSession)); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	if got := len(sessions.GetHistory(conversationSession)); got != 0 {
		t.Fatalf("deleted conversation came back with %d messages", got)
	}
	if got := len(sessions.GetHistory(mainKey)); got != 0 {
		t.Fatalf("main session got %d messages", got)
	}
	if provider.count() != 0 {
		t.Fatalf("a turn ran for the result (%d model calls)", provider.count())
	}
}

// A result released when its turn ended can still be on the
// inbound queue when the user clears the conversation. It must not reach
// main as a turn of its own.
func TestRun_ResultReleasedBeforeAClearOpensNoTurn(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	mainKey := session.BuildMainSessionKey(al.registry.GetDefaultAgent().ID)
	live := &turnState{turnID: "turn-9", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	if !al.routeBackgroundResult(msg) {
		t.Fatal("the result was not parked")
	}
	al.clearActiveTurn(live)
	waitFor(t, "the release", func() bool { return len(msgBus.InboundChan()) == 1 })

	if _, err := al.ProcessDirect(context.Background(), "/clear", conversationSession); err != nil {
		t.Fatalf("/clear: %v", err)
	}
	startRunLoop(t, al)
	waitFor(t, "the loop to take the result", func() bool { return len(msgBus.InboundChan()) == 0 })
	time.Sleep(300 * time.Millisecond)

	if provider.count() != 0 {
		t.Fatalf("the old result opened a turn (%d model calls)", provider.count())
	}
	if got := len(sessions.GetHistory(mainKey)); got != 0 {
		t.Fatalf("main session got %d messages", got)
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

// A result queued behind a turn survived /clear and the flush
// wrote it into the cleared conversation, so the next turn read the old task.
func TestClear_DropsResultsWaitingForTheTurn(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-9", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	al.mirror.mu.Lock()
	al.queueMirroredLocked(conversationSession, providers.Message{
		Role:    "user",
		Content: "[System: async:spawn] Task 'importação' completed.",
	})
	al.mirror.mu.Unlock()
	// And a result really parked behind the live turn.
	parked := spawnResultMessage(conversationSession)
	parked.Context.ChatID, parked.ChatID = "telegram:123", "telegram:123"
	parked.Content = "Task 'exportação' completed."
	if !al.routeBackgroundResult(parked) {
		t.Fatal("the result was not parked")
	}

	if _, err := al.ProcessDirect(context.Background(), "/clear", conversationSession); err != nil {
		t.Fatalf("/clear: %v", err)
	}
	if n := pendingNotes(al, conversationSession) + parkedResults(al, conversationSession); n != 0 {
		t.Fatalf("%d results still waiting after /clear", n)
	}
	al.clearActiveTurn(live)

	time.Sleep(100 * time.Millisecond)
	if n := len(msgBus.InboundChan()); n != 0 {
		t.Fatalf("%d old results went back to the loop", n)
	}
	for _, m := range sessions.GetHistory(conversationSession) {
		if strings.Contains(m.Content, "completed") {
			t.Fatalf("an old result reached the cleared conversation: %q", m.Content)
		}
	}
}

// The Run loop parks a result for a session it saw claimed, but
// the claimant can end (releasing an empty list) before the result is parked.
// A session found idle hands the result straight back.
func TestParkBackgroundResult_AfterTheClaimantEndedIsReleased(t *testing.T) {
	al, msgBus, _ := newSystemMessageTestLoop(t, &countingReplyProvider{})
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-9", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	al.clearActiveTurn(live)

	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	al.parkBackgroundResult(conversationSession, msg)

	select {
	case got := <-msgBus.InboundChan():
		if got.SessionKey != conversationSession {
			t.Fatalf("released %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the result stayed parked with no turn alive (%d parked)", parkedResults(al, conversationSession))
	}
}

// A release that finds the inbound queue full (the loop busy on
// a long inline message) or closed must not lose the result.
func TestReleaseParkedResults_FullBusWritesTheResultIntoTheConversation(t *testing.T) {
	al, msgBus, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	al.SetDeliverySessionResolver(webResolver)
	fillInboundQueue(t, msgBus)
	live := &turnState{turnID: "turn-9", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	if !al.routeBackgroundResult(spawnResultMessage(conversationSession)) {
		t.Fatal("the result was not parked")
	}

	al.clearActiveTurn(live)

	waitFor(t, "the result written into the conversation", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 3
	})
	if got := sessions.GetHistory(conversationSession)[2]; !strings.HasPrefix(
		got.Content,
		"[System: async:spawn] Spawn failed",
	) {
		t.Fatalf("wrote %q", got.Content)
	}
}

// After a /stop the user wants the agent quiet. Results that
// arrived during the stopped turn, or arrive before it ends, are written for the
// next turn, never opened as turns of their own: all of them, more than the
// mirror queue holds, and through the /stop path itself.
func TestStop_ParkedResultsBecomeNotes(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	live := &turnState{turnID: "turn-9", sessionKey: conversationSession}
	al.registerActiveTurn(live)
	startRunLoop(t, al)
	publish := func(i int) {
		msg := spawnResultMessage(conversationSession)
		msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
		msg.Content = fmt.Sprintf("Task %d completed", i)
		if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
			t.Fatalf("PublishInbound: %v", err)
		}
	}
	results := maxPendingMirrors + 5
	for i := range results {
		publish(i)
	}
	waitFor(t, "the results to wait", func() bool { return parkedResults(al, conversationSession) == results })

	if result, err := al.stopActiveTurnForSession(conversationSession); err != nil || !result.Stopped {
		t.Fatalf("stop = %+v, %v", result, err)
	}
	publish(results) // arrives while the stopped turn winds down
	waitFor(t, "the loop to take the late result", func() bool { return len(msgBus.InboundChan()) == 0 })
	time.Sleep(100 * time.Millisecond) // parked right after it is taken
	al.clearActiveTurn(live)

	waitFor(t, "the results in the history", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 2+results+1
	})
	time.Sleep(200 * time.Millisecond)
	if provider.count() != 0 {
		t.Fatalf("results opened %d turns after the stop", provider.count())
	}
	history := sessions.GetHistory(conversationSession)
	if first := history[2].Content; !strings.Contains(first, "Task 0 completed") {
		t.Fatalf("first note = %q, want the oldest result", first)
	}
}

// stallingProvider answers the first call only when the turn is stopped, and
// tells the test the turn is running.
type stallingProvider struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *stallingProvider) Chat(
	ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *stallingProvider) GetDefaultModel() string { return "mock-model" }

// A /stop on the turn a result opened rolled the history
// back with the result in it, so the subagent's work was in no conversation,
// and the stop reply quoted the internal envelope as the task name.
func TestStop_TurnOpenedByAResultKeepsTheResultAsANote(t *testing.T) {
	provider := &stallingProvider{started: make(chan struct{}, 1)}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	startRunLoop(t, al)
	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	if err := msgBus.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	select {
	case <-provider.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the result never opened a turn")
	}

	result, err := al.stopActiveTurnForSession(conversationSession)
	if err != nil || !result.Stopped {
		t.Fatalf("stop = %+v, %v", result, err)
	}
	if result.TaskName != "" {
		t.Fatalf("stop reply names the task %q", result.TaskName)
	}

	waitFor(t, "the result back in the history", func() bool {
		return len(sessions.GetHistory(conversationSession)) == 3 && al.getActiveTurnState(conversationSession) == nil
	})
	if got := sessions.GetHistory(conversationSession)[2]; got.Role != "user" ||
		!strings.HasPrefix(got.Content, "[System: async:spawn] Spawn failed") {
		t.Fatalf("history ends with %s %q", got.Role, got.Content)
	}
	if n := provider.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want the one that was stopped", n)
	}
}

// A turn that ends between the stop's cancel and its rollback already restored
// its own snapshot. The rollback must not then cut what was written into the
// conversation after the turn (here, a note).
func TestHardAbort_SparesWhatWasWrittenAfterTheTurnEnded(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	live := &turnState{
		turnID:               "turn-9",
		sessionKey:           conversationSession,
		session:              sessions,
		initialHistoryLength: 2,
	}
	live.turnCancel = func() {
		al.clearActiveTurn(live)
		_, _ = al.deliverToConversation(agent, conversationSession, providers.Message{
			Role:    "user",
			Content: "[System: async:spawn] Task 'importação' completed.",
		})
	}
	al.registerActiveTurn(live)

	if err := al.HardAbort(conversationSession); err != nil {
		t.Fatalf("HardAbort: %v", err)
	}

	if got := len(sessions.GetHistory(conversationSession)); got != 3 {
		t.Fatalf("history has %d messages, want the note kept", got)
	}
}

// fillInboundQueue fills the loop's inbound queue (no loop is running) and
// shortens the wait for room, so the next publish of a background result fails.
func fillInboundQueue(t *testing.T, msgBus *bus.MessageBus) {
	t.Helper()
	previous := backgroundPublishTimeout
	backgroundPublishTimeout = 100 * time.Millisecond
	t.Cleanup(func() { backgroundPublishTimeout = previous })
	for i := 0; ; i++ {
		filler := spawnResultMessage("sk_v1_other")
		filler.Content = fmt.Sprintf("filler %d", i)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := msgBus.PublishInbound(ctx, filler)
		cancel()
		if err != nil {
			return // the queue is full
		}
	}
}

// asyncResultAsBuilt is an async result the way the tool callback builds it:
// only Context set, not yet normalized by the bus.
func asyncResultAsBuilt(sessionKey, content string) bus.InboundMessage {
	return bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:  "system",
			ChatID:   "telegram:123",
			ChatType: "direct",
			SenderID: "async:spawn",
		},
		Content:    content,
		SessionKey: sessionKey,
	}
}

// The result of a spawn the user stopped must not open a turn, whenever it
// lands (the stopped turn may be gone by then). It is written for the next
// turn instead, and nothing goes to the loop.
func TestDeliverAsyncResult_StoppedWorkIsANoteNotATurn(t *testing.T) {
	al, msgBus, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	msg := asyncResultAsBuilt(conversationSession, "Spawn failed: "+ErrSubTurnParentCanceled.Error())

	al.deliverAsyncResult(msg, fmt.Errorf("spawn: %w", ErrSubTurnParentCanceled))

	if n := len(msgBus.InboundChan()); n != 0 {
		t.Fatalf("%d messages went to the loop", n)
	}
	history := sessions.GetHistory(conversationSession)
	if len(history) != 3 || !strings.Contains(history[2].Content, "its turn was stopped") {
		t.Fatalf("history = %+v, want the stopped result as a note", history)
	}
}

// A result the loop does not take in time is written into the conversation,
// not only logged.
func TestDeliverAsyncResult_FullBusWritesTheResultIntoTheConversation(t *testing.T) {
	al, msgBus, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	fillInboundQueue(t, msgBus)

	al.deliverAsyncResult(asyncResultAsBuilt(conversationSession, "Task 'x' completed."), nil)

	if got := len(sessions.GetHistory(conversationSession)); got != 3 {
		t.Fatalf("history has %d messages, want the result written", got)
	}
}

// A stop armed after the worker's check but before the turn registers is
// taken inside runTurn: the turn aborts at once and its rollback removes the
// result that opened it. The result is written back as a note.
func TestProcessSystemMessage_PendingStopKeepsTheResultAsANote(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	al.SetDeliverySessionResolver(webResolver)
	msg := spawnResultMessage(conversationSession)
	msg.Context.ChatID, msg.ChatID = "telegram:123", "telegram:123"
	msg.Content = "Task 'importação' completed."
	al.markPendingStop(conversationSession)

	if _, err := al.processSystemMessage(context.Background(), msg); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	history := sessions.GetHistory(conversationSession)
	if len(history) != 3 || !strings.Contains(history[2].Content, "Task 'importação' completed") {
		t.Fatalf("history = %+v, want the result as a note", history)
	}
}

// failingAppendStore loses every append and says so, like a full disk.
type failingAppendStore struct{ session.SessionStore }

func (failingAppendStore) AppendMessage(string, providers.Message) error {
	return errors.New("no space left on device")
}

// A failed write was logged as "Recorded background result
// in its conversation". The caller now gets the error.
func TestDeliverToConversation_ReportsAFailedWrite(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	agent.Sessions = failingAppendStore{sessions}

	note := backgroundNote(spawnResultMessage(conversationSession))
	deferred, err := al.deliverToConversation(agent, conversationSession, note)

	if deferred || err == nil {
		t.Fatalf("deferred=%v err=%v, want the failed write reported", deferred, err)
	}
}

// A task launched before /clear delivered into the new
// conversation once the user had typed anything, and spent a model call.
func TestRun_ResultOfWorkLaunchedBeforeAClearIsDropped(t *testing.T) {
	provider := &countingReplyProvider{}
	al, msgBus, sessions := newSystemMessageTestLoop(t, provider)
	al.SetDeliverySessionResolver(webResolver)
	before := spawnResultMessage(conversationSession)
	before.Context.ChatID, before.ChatID = "telegram:123", "telegram:123"
	before.LaunchedAt = time.Now()

	if _, err := al.ProcessDirect(context.Background(), "/clear", conversationSession); err != nil {
		t.Fatalf("/clear: %v", err)
	}
	sessions.AddMessage(conversationSession, "user", "vamos começar outra coisa")
	after := before
	after.LaunchedAt = time.Now()
	after.Content = "Task 'nova' completed."
	startRunLoop(t, al)

	if err := msgBus.PublishInbound(context.Background(), before); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if provider.count() != 0 || len(sessions.GetHistory(conversationSession)) != 1 {
		t.Fatalf("the old result reached the new conversation (%d model calls, %d messages)",
			provider.count(), len(sessions.GetHistory(conversationSession)))
	}

	if err := msgBus.PublishInbound(context.Background(), after); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	waitFor(t, "work launched after the clear to arrive", func() bool { return provider.count() == 1 })
}

// losingOneStore loses the append of one message and keeps the others.
type losingOneStore struct {
	session.SessionStore
	lose string
}

func (s losingOneStore) AppendMessage(key string, msg providers.Message) error {
	if msg.Content == s.lose {
		return errors.New("no space left on device")
	}
	s.AddFullMessage(key, msg)
	return nil
}

// A failed write reports the loss without dropping the rest of the batch,
// which was already taken off the queue.
func TestWriteMirrored_AFailedAppendKeepsTheRestOfTheBatch(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	agent.Sessions = losingOneStore{SessionStore: sessions, lose: "b"}

	err := al.writeMirrored(agent, conversationSession, []providers.Message{
		{Role: "user", Content: "a"}, {Role: "user", Content: "b"}, {Role: "user", Content: "c"},
	})

	if err == nil {
		t.Fatal("the lost message was not reported")
	}
	history := sessions.GetHistory(conversationSession)
	if len(history) != 4 || history[2].Content != "a" || history[3].Content != "c" {
		t.Fatalf("history = %+v, want a and c written", history)
	}
}

// A /clear between resolving a result's conversation and writing the note
// into it drops the result: the check is made again where the note is written.
func TestDeliverBackgroundNote_ClearedAfterTheCheckIsDropped(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	msg := spawnResultMessage(conversationSession)
	msg.LaunchedAt = time.Now()
	if _, _, ok := al.backgroundResultTarget(msg); !ok {
		t.Fatal("the conversation was not resolved")
	}
	al.dropMirroredDeliveries(conversationSession) // what /clear stamps

	if _, err := al.deliverBackgroundNote(agent, conversationSession, msg); !errors.Is(err, errClearedSinceLaunch) {
		t.Fatalf("err = %v, want the result dropped", err)
	}
	if got := len(sessions.GetHistory(conversationSession)); got != 2 {
		t.Fatalf("history has %d messages, want the old result left out", got)
	}
}
