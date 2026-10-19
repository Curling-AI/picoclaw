package agent

import (
	"context"
	"os"
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

// sessionRecordingProvider holds its first call open until released and keeps
// what every call was sent, so a test can tell which turn read a message.
type sessionRecordingProvider struct {
	mu        sync.Mutex
	sent      [][]providers.Message
	started   chan struct{}
	release   chan struct{}
	firstResp string
	laterResp string
}

func newSessionRecordingProvider(firstResp, laterResp string) *sessionRecordingProvider {
	return &sessionRecordingProvider{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		firstResp: firstResp,
		laterResp: laterResp,
	}
}

func (p *sessionRecordingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.sent = append(p.sent, append([]providers.Message(nil), messages...))
	call := len(p.sent)
	p.mu.Unlock()

	if call > 1 {
		return &providers.LLMResponse{Content: p.laterResp}, nil
	}
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &providers.LLMResponse{Content: p.firstResp}, nil
}

func (p *sessionRecordingProvider) GetDefaultModel() string { return "session-recording-mock" }

func (p *sessionRecordingProvider) calls() [][]providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]providers.Message(nil), p.sent...)
}

func sentContains(messages []providers.Message, text string) bool {
	return occurrences(messages, text) > 0
}

func occurrences(messages []providers.Message, text string) int {
	n := 0
	for _, m := range messages {
		if strings.Contains(m.Content, text) {
			n++
		}
	}
	return n
}

// The web derives opaque keys like these; the loop keeps them as they are.
var (
	conversationA = session.BuildOpaqueSessionKey("web:direct:u-1:conversation-a")
	conversationB = session.BuildOpaqueSessionKey("web:direct:u-1:conversation-b")
)

func newSessionSteeringLoop(t *testing.T, provider providers.LLMProvider) *AgentLoop {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	return NewAgentLoop(cfg, bus.NewMessageBus(), provider)
}

type directResult struct {
	resp string
	err  error
}

func runDirect(al *AgentLoop, content, sessionKey, chatID string) <-chan directResult {
	done := make(chan directResult, 1)
	go func() {
		resp, err := al.ProcessDirectWithChannel(context.Background(), content, sessionKey, "test", chatID)
		done <- directResult{resp: resp, err: err}
	}()
	return done
}

func waitDirect(t *testing.T, done <-chan directResult) directResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("turn failed: %v", result.err)
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never ended")
	}
	return directResult{}
}

// A message sent while its conversation is answering joins the turn that is
// answering, as a channel message does, instead of opening a second turn that
// redoes the same work.
func TestAgentLoop_SteerSession_ReadByTheRunningTurn(t *testing.T) {
	provider := newSessionRecordingProvider("stale answer", "answer that read the follow-up")
	al := newSessionSteeringLoop(t, provider)

	done := runDirect(al, "build the deck", conversationA, "run-a")
	<-provider.started
	if err := al.SteerSession(conversationA, "carry on", nil); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	close(provider.release)

	if result := waitDirect(t, done); result.resp != "answer that read the follow-up" {
		t.Errorf("turn answered %q, want the answer of the call that read the follow-up", result.resp)
	}
	calls := provider.calls()
	if len(calls) != 2 || !sentContains(calls[1], "carry on") {
		t.Fatalf("got %d calls; the second must carry the follow-up", len(calls))
	}
	if n := al.PendingSteering(conversationA); n != 0 {
		t.Errorf("%d messages still queued after the turn read them", n)
	}
	// Read during a reply without tool calls, the follow-up used to enter the
	// context and the history twice, on every channel.
	if n := occurrences(calls[1], "carry on"); n != 1 {
		t.Errorf("the model got the follow-up %d times, want once", n)
	}
	if n := occurrences(al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(conversationA), "carry on"); n != 1 {
		t.Errorf("the history holds the follow-up %d times, want once", n)
	}
}

// Steer picks any active turn. On a pod where another conversation is the one
// running, the message must wait for its own conversation instead of entering
// someone else's turn.
func TestAgentLoop_SteerSession_StaysWithItsOwnSession(t *testing.T) {
	provider := newSessionRecordingProvider("b answered", "a answered")
	al := newSessionSteeringLoop(t, provider)

	done := runDirect(al, "question of b", conversationB, "run-b")
	<-provider.started
	if err := al.SteerSession(conversationA, "message of a", nil); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	if got := al.PendingSteering(conversationA); got != 1 {
		t.Fatalf("conversation a holds %d messages, want 1", got)
	}
	if got := al.PendingSteering(conversationB); got != 0 {
		t.Fatalf("conversation b holds %d messages, want 0", got)
	}
	close(provider.release)

	if result := waitDirect(t, done); result.resp != "b answered" {
		t.Errorf("turn of b answered %q", result.resp)
	}
	if calls := provider.calls(); len(calls) != 1 || sentContains(calls[0], "message of a") {
		t.Fatalf("the turn of b read the message of a (%d calls)", len(calls))
	}

	resp, err := al.Continue(context.Background(), conversationA, "test", "run-a")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if resp != "a answered" {
		t.Errorf("Continue answered %q", resp)
	}
	if calls := provider.calls(); len(calls) != 2 || !sentContains(calls[1], "message of a") {
		t.Fatal("Continue did not run the message of a")
	}
}

// queuedReplyProvider answers each call with the next scripted response (the last
// one repeats) and keeps what each call was sent.
type queuedReplyProvider struct {
	mu        sync.Mutex
	sent      [][]providers.Message
	responses []providers.LLMResponse
}

func (p *queuedReplyProvider) Chat(
	_ context.Context, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, append([]providers.Message(nil), messages...))
	resp := p.responses[min(len(p.sent), len(p.responses))-1]
	return &resp, nil
}

func (*queuedReplyProvider) GetDefaultModel() string { return "queued-reply-mock" }

func toolThenAnswer() *queuedReplyProvider {
	return &queuedReplyProvider{responses: []providers.LLMResponse{
		{Content: "Checking.", ToolCalls: []providers.ToolCall{{
			ID: "call_custom", Type: "function", Name: "mock_custom", Arguments: map[string]any{},
		}}},
		{Content: "continued answer"},
	}}
}

// A caller draining a direct turn's queue must tell "nothing was read" from a
// turn that ran: a handled reply ends without text, and taking that for "nothing
// to do" left the next queued message unanswered.
func TestAgentLoop_ContinueDirect_ReportsWhetherItRan(t *testing.T) {
	provider := &queuedReplyProvider{responses: []providers.LLMResponse{{Content: "continued answer"}}}
	al := newSessionSteeringLoop(t, provider)
	req := DirectContinuation{SessionKey: conversationA, Channel: "grpc", ChatID: "run-a"}

	if content, ran, err := al.ContinueDirect(context.Background(), req); err != nil || ran || content != "" {
		t.Fatalf("empty queue: got %q ran=%v err=%v, want nothing run", content, ran, err)
	}
	if err := al.SteerSession(conversationA, "one more thing", nil); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	content, ran, err := al.ContinueDirect(context.Background(), req)
	if err != nil || !ran || content != "continued answer" {
		t.Fatalf("queued message: got %q ran=%v err=%v", content, ran, err)
	}
}

// endTurnTool ends its turn with a reply the user already sees, as the
// connector card does: the turn closes with no text of its own.
type endTurnTool struct{}

func (*endTurnTool) Name() string        { return "show_card" }
func (*endTurnTool) Description() string { return "Shows a card that needs the person's answer" }
func (*endTurnTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (*endTurnTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	result := tools.UserResult("card shown").WithResponseHandled()
	result.EndTurn = true
	return result
}

// A continuation that ends on a handled reply ran even though it has no text:
// ran says so, where Continue's empty answer reads like "nothing was queued".
func TestAgentLoop_ContinueDirect_HandledReplyStillRan(t *testing.T) {
	provider := &queuedReplyProvider{responses: []providers.LLMResponse{
		{
			ToolCalls: []providers.ToolCall{
				{ID: "call_card", Type: "function", Name: "show_card", Arguments: map[string]any{}},
			},
		},
	}}
	al := newSessionSteeringLoop(t, provider)
	al.RegisterTool(&endTurnTool{})
	if err := al.SteerSession(conversationA, "connect the calendar", nil); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}

	content, ran, err := al.ContinueDirect(
		context.Background(),
		DirectContinuation{SessionKey: conversationA, Channel: "grpc", ChatID: "run-a"},
	)
	if err != nil || !ran || content != "" {
		t.Fatalf("handled reply: got %q ran=%v err=%v, want an empty answer from a turn that ran", content, ran, err)
	}
}

// The continuation of a web reply shows on the screen as it happens, like the
// reply it continues: Continue's turn published nothing until its final text,
// the silence that made the person resend in the first place.
func TestAgentLoop_ContinueDirect_StreamsToTheChat(t *testing.T) {
	interims := func(t *testing.T, continueTurn func(*AgentLoop) error) int {
		t.Helper()
		tmpDir := t.TempDir()
		cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: tmpDir, ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 10,
		}}}
		msgBus := bus.NewMessageBus()
		al := NewAgentLoop(cfg, msgBus, toolThenAnswer())
		al.RegisterTool(&mockCustomTool{})
		if err := al.SteerSession(conversationA, "check the deck", nil); err != nil {
			t.Fatalf("SteerSession: %v", err)
		}
		if err := continueTurn(al); err != nil {
			t.Fatalf("continuation: %v", err)
		}
		published := 0
		for {
			select {
			case out := <-msgBus.OutboundChan():
				if out.ChatID == "run-a" {
					published++
				}
			default:
				return published
			}
		}
	}

	direct := interims(t, func(al *AgentLoop) error {
		req := DirectContinuation{SessionKey: conversationA, Channel: "grpc", ChatID: "run-a"}
		_, _, err := al.ContinueDirect(context.Background(), req)
		return err
	})
	if direct == 0 {
		t.Error("ContinueDirect published nothing to the chat before its final text")
	}
	silent := interims(t, func(al *AgentLoop) error {
		_, err := al.Continue(context.Background(), conversationA, "grpc", "run-a")
		return err
	})
	if silent != 0 {
		t.Errorf("Continue published %d messages; this test assumes it streams nothing", silent)
	}
}

// blockingTool holds its call until released and ignores the turn's context,
// as a tool in the middle of a long command does.
type blockingTool struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingTool) Name() string        { return "slow_command" }
func (*blockingTool) Description() string { return "A command that takes a while" }
func (*blockingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (b *blockingTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	close(b.started)
	<-b.release
	return tools.SilentResult("done")
}

// After the person stops a reply, the turn keeps unwinding until its tool
// returns. What they send meanwhile is for the next turn: the stopped one used
// to read it after the tool, write it in the history, and never answer it.
func TestAgentLoop_StoppedTurnLeavesTheQueueToTheNextTurn(t *testing.T) {
	provider := &queuedReplyProvider{responses: []providers.LLMResponse{
		{
			ToolCalls: []providers.ToolCall{
				{ID: "call_slow", Type: "function", Name: "slow_command", Arguments: map[string]any{}},
			},
		},
		{Content: "should not be reached"},
	}}
	al := newSessionSteeringLoop(t, provider)
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	al.RegisterTool(tool)

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := al.ProcessDirectWithChannel(ctx, "run the build", conversationA, "grpc", "run-a")
		done <- err
	}()
	<-tool.started
	stop()
	if err := al.SteerSession(conversationA, "now do something else", nil); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	close(tool.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped turn never ended")
	}

	if n := al.PendingSteering(conversationA); n != 1 {
		t.Errorf("%d messages queued after the stopped turn ended, want the one sent after the stop", n)
	}
	if n := occurrences(
		al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(conversationA),
		"now do something else",
	); n != 0 {
		t.Errorf("the stopped turn wrote the next turn's message in the history %d times", n)
	}
}

// A key the loop routes elsewhere names a queue no turn reads, and an empty
// one the shared fallback queue: either way the message would be lost.
func TestAgentLoop_SteerSession_RefusesAKeyNoTurnRunsUnder(t *testing.T) {
	al := newSessionSteeringLoop(t, &mockProvider{})

	for _, key := range []string{"", "  ", "web:conversation-a"} {
		if err := al.SteerSession(key, "lost", nil); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
	if n := al.steering.len(); n != 0 {
		t.Errorf("%d messages queued", n)
	}
}

func TestAgentLoop_ClearSteering_DropsOnlyThatSession(t *testing.T) {
	al := newSessionSteeringLoop(t, &mockProvider{})
	for _, key := range []string{conversationA, conversationA, conversationB} {
		if err := al.SteerSession(key, "queued", []string{"/workspace/uploads/photo.png"}); err != nil {
			t.Fatalf("SteerSession: %v", err)
		}
	}

	if dropped := al.ClearSteering(conversationA); dropped != 2 {
		t.Errorf("dropped %d, want 2", dropped)
	}
	if n := al.PendingSteering(conversationA); n != 0 {
		t.Errorf("conversation a still holds %d", n)
	}
	msgs := al.dequeueSteeringMessagesForScope(conversationB)
	if len(msgs) != 1 || len(msgs[0].Media) != 1 {
		t.Fatalf("conversation b lost its message or its attachment: %+v", msgs)
	}
	if al.ClearSteering("") != 0 || al.PendingSteering("") != 0 {
		t.Error("an empty key reached the shared fallback queue")
	}
}
