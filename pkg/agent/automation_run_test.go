package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// automationRunKey is the session key ExecuteJobWithResult gives a run
// (pkg/tools/cron.go): "agent:cron-<job>-<run>".
const automationRunKey = "agent:cron-job-1-6f1c2f3e-4b5a-4c6d-8e9f-0a1b2c3d4e5f"

func automationRequest(sessionKey string) turnresult.Request {
	return turnresult.Request{
		Progress:   nil,
		Media:      nil,
		Content:    "[Scheduled run of cron job \"Novas vagas\"]\nme avise só quando houver vaga nova",
		SessionKey: sessionKey,
		Channel:    "telegram",
		ChatID:     "chat-1",
	}
}

func TestIsCronRunMainTurn(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{automationRunKey, true},
		{"agent:cronmodel-job-1-run", true},
		{"subturn-1", false},
		{"sk_v1_abc", false},
		{"", false},
	} {
		ts := &turnState{sessionKey: tc.key}
		if got := ts.isCronRunMainTurn(); got != tc.want {
			t.Errorf("isCronRunMainTurn(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
	var none *turnState
	if none.isCronRunMainTurn() {
		t.Error("a nil turn is not a cron run")
	}
	// A turn the run spawned: isCronRun counts it, by the job's cache scope it
	// inherits, but it is not the run itself.
	child := &turnState{sessionKey: "subturn-1", promptCacheScopeOverride: "agent:cron-job-1"}
	if !child.isCronRun() || child.isCronRunMainTurn() {
		t.Errorf("a run's subturn: isCronRun = %v, isCronRunMainTurn = %v; want true, false",
			child.isCronRun(), child.isCronRunMainTurn())
	}
}

// Adapted from picoclaw#128 (silent_turn_test.go). Here an empty end is still
// a failure: only what is delivered changes. (seucaranguejo fork — MST-795)
func TestEmptyAnswerOfAnAutomationRunDeliversNothing(t *testing.T) {
	al, provider := newScriptedLoop(t, providers.LLMResponse{Content: ""}, providers.LLMResponse{Content: ""})
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest(automationRunKey))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Status != turnresult.Failed || result.Content != "" {
		t.Fatalf("result = %+v, want failed with no content", result)
	}
	if got := provider.calls(); got != 1+maxEmptyResponseRetries {
		t.Fatalf("provider calls = %d, want %d (the empty-answer retry still runs)", got, 1+maxEmptyResponseRetries)
	}
}

func TestEmptyAnswerOutsideAnAutomationRunKeepsTheFallback(t *testing.T) {
	al, _ := newScriptedLoop(t, providers.LLMResponse{Content: ""}, providers.LLMResponse{Content: ""})
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest("subturn-1"))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Status != turnresult.Failed || result.Content != defaultResponse {
		t.Fatalf("result = %+v, want failed with the default response", result)
	}
}

func TestAutomationRunAtTheIterationCapKeepsTheToolLimitReply(t *testing.T) {
	al, _ := newScriptedLoopWithCap(t, 1, readFileCall("call-1"), readFileCall("call-2"))
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest(automationRunKey))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Content != toolLimitResponse {
		t.Fatalf("result = %+v, want the tool-limit reply", result)
	}
}

func TestReasoningIsNotTheAnswerOfAnAutomationRun(t *testing.T) {
	const thought = "没有新内容，不需要通知用户。"
	reply := providers.LLMResponse{Content: "", ReasoningContent: thought}
	for _, tc := range []struct {
		name   string
		key    string
		status turnresult.Status
		answer string
	}{
		{"automation run", automationRunKey, turnresult.Failed, ""},
		{"other turn", "chat-session", turnresult.Completed, thought},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, _ := newScriptedLoop(t, reply, reply)
			result, err := al.ProcessDirectWithResult(context.Background(), automationRequest(tc.key))
			if err != nil {
				t.Fatalf("ProcessDirectWithResult: %v", err)
			}
			if result.Status != tc.status || result.Content != tc.answer {
				t.Fatalf("result = %+v, want %s with %q", result, tc.status, tc.answer)
			}
		})
	}
}

// A streamed answer reaches the chat before the run's gate sees it, so an
// automation run's turn does not stream: with streaming on for the model and
// the channel, its NO_REPLY stays off the channel, while the same turn in a
// chat session still streams. (seucaranguejo fork — MST-795)
func TestAutomationRunDoesNotStreamItsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		streams   bool
		finalized []string
	}{
		{"automation run", automationRunKey, false, nil},
		{"other turn", "chat-session", true, []string{"NO_REPLY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newConfiguredStreamingTestConfig(t, true, true, nil)
			streamer := &recordingStreamer{}
			msgBus := bus.NewMessageBus()
			msgBus.SetStreamDelegate(configuredStreamingDelegate{streamer: streamer})
			provider := &configuredStreamingProvider{
				streamPlan: []configuredStreamingCall{{
					chunks:   []string{"NO_REPLY"},
					response: &providers.LLMResponse{Content: "NO_REPLY"},
				}},
				chatResponse: &providers.LLMResponse{Content: "NO_REPLY"},
			}
			al := NewAgentLoop(cfg, msgBus, provider)
			t.Cleanup(al.Close)
			request := automationRequest(tc.key)
			request.Channel, request.ChatID = "pico", "session-1"

			result, err := al.ProcessDirectWithResult(context.Background(), request)
			if err != nil {
				t.Fatalf("ProcessDirectWithResult: %v", err)
			}
			if result.Content != "NO_REPLY" {
				t.Fatalf("result = %+v, want the answer NO_REPLY", result)
			}

			if streams := provider.streamCalls > 0; streams != tc.streams {
				t.Fatalf("stream calls = %d, want streaming = %v", provider.streamCalls, tc.streams)
			}
			if !slices.Equal(streamer.finalized, tc.finalized) {
				t.Fatalf("finalized stream %q, want %q", streamer.finalized, tc.finalized)
			}
			var published []string
			for drained := false; !drained; {
				select {
				case out := <-msgBus.OutboundChan():
					published = append(published, out.Content)
				default:
					drained = true
				}
			}
			if !tc.streams && (len(streamer.updates) > 0 || len(published) > 0) {
				t.Fatalf("stream chunks %q and bus messages %q, want none", streamer.updates, published)
			}
		})
	}
}

// newEndedRunLoop is an agent whose automation run, delivered to Telegram chat
// 123 (conversation mirrorChatSession), has ended: the run's session holds its
// note and its answer.
func newEndedRunLoop(
	t *testing.T,
	script ...providers.LLMResponse,
) (*AgentLoop, *bus.MessageBus, session.SessionStore) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, &scriptedProvider{script: script})
	t.Cleanup(al.Close)
	al.SetDeliverySessionResolver(func(channel, chatID string) string {
		if channel == "telegram" && chatID == "123" {
			return mirrorChatSession
		}
		return ""
	})
	sessions := al.registry.GetDefaultAgent().Sessions
	sessions.AddMessage(mirrorChatSession, "user", "oi")
	sessions.AddMessage(mirrorChatSession, "assistant", "Oi! Em que posso ajudar?")
	sessions.AddMessage(automationRunKey, "user", automationRequest(automationRunKey).Content)
	sessions.AddMessage(automationRunKey, "assistant", "NO_REPLY")
	return al, msgBus, sessions
}

// A spawn an automation run launched can finish after the run ended. Its
// result opens a turn in the run's session, whose history holds the note that
// asks for NO_REPLY, and that turn's answer goes to the chat: it delivers what
// the run's own answer would. (seucaranguejo fork — MST-795)
func TestLateResultOfAnAutomationRunDeliversByItsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    string
		published string
	}{
		{"nothing to deliver", "NO_REPLY", ""},
		{"news", "Nova vaga: casa em Lisboa", "Nova vaga: casa em Lisboa"},
		{"token mixed with news", "NO_REPLY\nNova vaga: casa em Lisboa", "Nova vaga: casa em Lisboa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, msgBus, sessions := newEndedRunLoop(t, providers.LLMResponse{Content: tc.answer})
			before := len(sessions.GetHistory(mirrorChatSession))

			if _, err := al.processSystemMessage(context.Background(), bus.NormalizeInboundMessage(bus.InboundMessage{
				Context:    bus.InboundContext{Channel: "system", ChatID: "telegram:123", SenderID: "async:spawn"},
				Content:    "Task 'vagas' completed.\n\nResult:\nlistagem conferida",
				SessionKey: automationRunKey,
			})); err != nil {
				t.Fatalf("processSystemMessage: %v", err)
			}

			var published []string
			for drained := false; !drained; {
				select {
				case out := <-msgBus.OutboundChan():
					published = append(published, out.Content)
				default:
					drained = true
				}
			}
			var mirrored []string
			for _, m := range sessions.GetHistory(mirrorChatSession)[before:] {
				mirrored = append(mirrored, m.Content)
			}
			var want []string
			if tc.published != "" {
				want = []string{tc.published}
			}
			if !slices.Equal(published, want) || !slices.Equal(mirrored, want) {
				t.Fatalf("published %q and mirrored %q, want %q in both", published, mirrored, want)
			}
		})
	}
}

// The gate is the automation run's: a late result in a chat conversation
// delivers its answer as it is. (seucaranguejo fork — MST-795)
func TestLateResultOutsideAnAutomationRunIsDeliveredAsItIs(t *testing.T) {
	al, msgBus, _ := newEndedRunLoop(t, providers.LLMResponse{Content: "NO_REPLY"})

	if _, err := al.processSystemMessage(context.Background(), bus.NormalizeInboundMessage(bus.InboundMessage{
		Context:    bus.InboundContext{Channel: "system", ChatID: "telegram:123", SenderID: "async:spawn"},
		Content:    "Task 'vagas' completed.\n\nResult:\nlistagem conferida",
		SessionKey: mirrorChatSession,
	})); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	select {
	case out := <-msgBus.OutboundChan():
		if out.Content != "NO_REPLY" {
			t.Fatalf("published %q, want the answer as it is", out.Content)
		}
	default:
		t.Fatal("the chat conversation's answer was not published")
	}
}
