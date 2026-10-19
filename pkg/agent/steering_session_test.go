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
