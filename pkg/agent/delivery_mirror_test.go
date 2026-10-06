package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	mirrorChatSession = "sk_v1_telegram_chat"
	mirrorCronSession = "agent:cron-job1-6f1c"
)

// newMirrorLoop builds a loop with the resolver the control plane would
// install: Telegram chat 123 is processed in mirrorChatSession.
func newMirrorLoop(t *testing.T) (*AgentLoop, session.SessionStore) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockProvider{})
	al.SetDeliverySessionResolver(func(channel, chatID string) string {
		if channel == "telegram" && chatID == "123" {
			return mirrorChatSession
		}
		return ""
	})
	return al, al.registry.GetDefaultAgent().Sessions
}

// newMirrorTestLoop is newMirrorLoop with chat 123 already in a conversation.
func newMirrorTestLoop(t *testing.T) (*AgentLoop, session.SessionStore) {
	t.Helper()
	al, sessions := newMirrorLoop(t)
	ensureSessionMetadata(sessions, mirrorChatSession,
		&session.SessionScope{Version: session.ScopeVersionV1, AgentID: "main", Channel: "telegram"}, nil)
	sessions.AddMessage(mirrorChatSession, "user", "oi")
	sessions.AddMessage(mirrorChatSession, "assistant", "Oi! Em que posso ajudar?")
	return al, sessions
}

func lastMessage(t *testing.T, sessions session.SessionStore, key string) providers.Message {
	t.Helper()
	history := sessions.GetHistory(key)
	if len(history) == 0 {
		t.Fatalf("session %s has no history", key)
	}
	return history[len(history)-1]
}

// The reported case: a scheduled run delivers a suggestion on Telegram, and the
// chat's own conversation must hold it for the user's "pode fazer" to make sense.
func TestMirrorDelivery_CronReplyEntersTheChatConversation(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession,
		"Daniel abriu o PR #2045. Quer que eu resuma as 3 decisões?")

	got := lastMessage(t, sessions, mirrorChatSession)
	if got.Role != "assistant" || got.Content != "Daniel abriu o PR #2045. Quer que eu resuma as 3 decisões?" {
		t.Fatalf("last chat message = %s %q, want the delivered suggestion", got.Role, got.Content)
	}
}

// Most automations reach Telegram through the message tool from a web-bound
// cron run, not through the run's final reply.
func TestMirrorDelivery_MessageToolSendFromAnotherSession(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	tool, ok := al.registry.GetDefaultAgent().Tools.Get("message")
	if !ok {
		t.Fatal("message tool not registered")
	}

	ctx := tools.WithToolSessionContext(context.Background(), "main", mirrorCronSession, nil)
	result := tool.Execute(ctx, map[string]any{
		"channel": "telegram", "chat_id": "123", "content": "Resumo do dia: 2 PRs novos.",
	})
	if result == nil || result.IsError {
		t.Fatalf("message tool failed: %+v", result)
	}

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "Resumo do dia: 2 PRs novos." {
		t.Fatalf("last chat message = %q, want the message tool content", got.Content)
	}
}

// A reply in the chat's own turn is already persisted by that turn.
func TestMirrorDelivery_SameSessionIsNotDuplicated(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	before := len(sessions.GetHistory(mirrorChatSession))

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorChatSession, "resposta")

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("history grew from %d to %d on a same-session reply", before, after)
	}
}

func TestMirrorDelivery_SkipsChatsWithoutConversation(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	al.SetDeliverySessionResolver(func(channel, chatID string) string {
		switch chatID {
		case "123":
			return mirrorChatSession
		case "999":
			return "sk_v1_never_talked"
		}
		return ""
	})

	al.PublishResponseIfNeeded(context.Background(), "grpc", "web-conv", mirrorCronSession, "para a web")
	al.PublishResponseIfNeeded(context.Background(), "telegram", "999", mirrorCronSession, "chat novo")

	if history := sessions.GetHistory("sk_v1_never_talked"); len(history) != 0 {
		t.Fatalf("mirror created a conversation nobody reads: %+v", history)
	}
	if got := lastMessage(t, sessions, mirrorChatSession); got.Content == "para a web" {
		t.Fatal("a web delivery leaked into the Telegram chat")
	}
}

func TestMirrorDelivery_DisabledWithoutResolver(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	al.SetDeliverySessionResolver(nil)
	before := len(sessions.GetHistory(mirrorChatSession))

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("history grew from %d to %d with no resolver installed", before, after)
	}
}

// Writing into a running turn could land between a tool call and its result,
// or be wiped when the turn restores its snapshot on abort.
func TestMirrorDelivery_WaitsForTheActiveTurnToEnd(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	running := &turnState{turnID: "turn-1", sessionKey: mirrorChatSession}
	al.registerActiveTurn(running)
	before := len(sessions.GetHistory(mirrorChatSession))

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete das 14h")

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("mirror wrote into a running turn (history %d → %d)", before, after)
	}

	al.clearActiveTurn(running)

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "lembrete das 14h" {
		t.Fatalf("last chat message after the turn = %q, want the deferred delivery", got.Content)
	}
}

// endTurnBeforeFlush leaves the session as a turn's end does right before its
// flush runs.
func endTurnBeforeFlush(al *AgentLoop, sessionKey string) {
	al.mirror.endTurn(sessionKey)
	al.activeTurnStates.Delete(sessionKey)
}

// A queued message can take the session over between the end of one turn and
// the flush; the delivery then waits for that turn as well.
func TestMirrorDelivery_DeferredDeliveryWaitsForTheTurnThatTookOver(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	first := &turnState{turnID: "turn-1", sessionKey: mirrorChatSession}
	al.registerActiveTurn(first)
	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")
	before := len(sessions.GetHistory(mirrorChatSession))

	endTurnBeforeFlush(al, mirrorChatSession)
	second := &turnState{turnID: "turn-2", sessionKey: mirrorChatSession}
	al.registerActiveTurn(second)
	al.flushMirroredDeliveries(mirrorChatSession)
	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("flushed into the turn that took over (history %d → %d)", before, after)
	}

	al.clearActiveTurn(second)
	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "lembrete" {
		t.Fatalf("last chat message = %q, want the deferred delivery", got.Content)
	}
}

// The chat session the webhook creates is the one that counts as having a
// conversation — not a hand-made fixture.
func TestMirrorDelivery_ReachesASessionCreatedByAnInboundTurn(t *testing.T) {
	al, sessions := newMirrorLoop(t)
	ctx := context.Background()
	if _, err := al.ProcessDirectWithMedia(ctx, "oi", mirrorChatSession, "telegram", "123", nil); err != nil {
		t.Fatalf("inbound turn: %v", err)
	}

	const briefing = "Bom dia! Sua agenda de hoje tem 3 reuniões."
	al.PublishResponseIfNeeded(ctx, "telegram", "123", mirrorCronSession, briefing)

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != briefing {
		t.Fatalf("last chat message = %q, want the delivery", got.Content)
	}
}

// The empty-response fallback is kept out of every history on purpose; the
// mirror must not bring it back through the chat.
func TestMirrorDelivery_SkipsSynthesizedResponses(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	before := len(sessions.GetHistory(mirrorChatSession))

	for _, text := range []string{defaultResponse, toolLimitResponse, handledToolResponseSummary} {
		al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, text)
	}

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("history grew from %d to %d with synthesized responses", before, after)
	}
}

type refusingChannelManager struct{ *recordingChannelManager }

func (refusingChannelManager) SendMessage(context.Context, bus.OutboundMessage) error {
	return errors.New("outside the 24h window")
}

func TestMirrorDelivery_RefusedSendIsNotMirrored(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	al.channelManager = refusingChannelManager{&recordingChannelManager{}}
	tool, _ := al.registry.GetDefaultAgent().Tools.Get("message")
	before := len(sessions.GetHistory(mirrorChatSession))

	ctx := tools.WithToolSessionContext(context.Background(), "main", mirrorCronSession, nil)
	result := tool.Execute(ctx, map[string]any{"channel": "telegram", "chat_id": "123", "content": "lembrete"})

	if result == nil || !result.IsError {
		t.Fatalf("message tool result = %+v, want the refusal", result)
	}
	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("a refused send was mirrored (history %d → %d)", before, after)
	}
}

// In the chat's own turn the tool call already carries the text.
func TestMirrorDelivery_MessageToolInTheChatsOwnTurnIsNotMirrored(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	tool, _ := al.registry.GetDefaultAgent().Tools.Get("message")
	before := len(sessions.GetHistory(mirrorChatSession))

	ctx := tools.WithToolSessionContext(context.Background(), "main", mirrorChatSession, nil)
	result := tool.Execute(ctx, map[string]any{"channel": "telegram", "chat_id": "123", "content": "aqui mesmo"})

	if result == nil || result.IsError {
		t.Fatalf("message tool failed: %+v", result)
	}
	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("history grew from %d to %d", before, after)
	}
}

// A subagent's result reaches the origin chat through a turn of the main
// session (processSystemMessage), not through PublishResponseIfNeeded.
func TestMirrorDelivery_SubagentResultEntersTheChatConversation(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)

	if _, err := al.processSystemMessage(context.Background(), bus.NormalizeInboundMessage(bus.InboundMessage{
		Context: bus.InboundContext{Channel: "system", ChatID: "telegram:123", SenderID: "subagent-1"},
		Content: "Task 'levantamento' completed.\n\nResult:\n3 fornecedores responderam.",
	})); err != nil {
		t.Fatalf("system message: %v", err)
	}

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "Mock response" {
		t.Fatalf("last chat message = %q, want the subagent turn's reply", got.Content)
	}
}

// Continue releases its placeholder on early exits; a delivery deferred while
// the session was held must not stay stranded until some later turn ends.
func TestMirrorDelivery_ContinueEarlyExitFlushesDeferredDelivery(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	running := &turnState{turnID: "turn-1", sessionKey: mirrorChatSession}
	al.registerActiveTurn(running)
	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")
	// A path that drops the turn without flushing (an upstream early exit).
	endTurnBeforeFlush(al, mirrorChatSession)

	if _, err := al.Continue(context.Background(), mirrorChatSession, "telegram", "123"); err != nil {
		t.Fatalf("Continue: %v", err)
	}

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "lembrete" {
		t.Fatalf("last chat message = %q, want the deferred delivery", got.Content)
	}
}

// A delivery that finds the session free right after a turn ended, before the
// flush ran, must not land ahead of the one that was waiting.
func TestMirrorDelivery_KeepsDeliveryOrderAcrossTheFlush(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	al.registerActiveTurn(&turnState{turnID: "turn-1", sessionKey: mirrorChatSession})
	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "primeira")
	// The turn is gone but its flush has not run yet.
	endTurnBeforeFlush(al, mirrorChatSession)

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "segunda")

	history := sessions.GetHistory(mirrorChatSession)
	if n := len(history); n < 2 || history[n-2].Content != "primeira" || history[n-1].Content != "segunda" {
		t.Fatalf("history tail = %+v, want primeira then segunda", history[len(history)-2:])
	}
}

// On the webhook path two turns of one session run at once and the second
// overwrites the first's entry; the delivery must wait for both.
func TestMirrorDelivery_WaitsForEveryConcurrentTurnOfTheSession(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	first := &turnState{turnID: "turn-a", sessionKey: mirrorChatSession}
	second := &turnState{turnID: "turn-b", sessionKey: mirrorChatSession}
	al.registerActiveTurn(first)
	al.registerActiveTurn(second)
	before := len(sessions.GetHistory(mirrorChatSession))

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")
	al.clearActiveTurn(second)
	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("wrote while the first turn was still running (history %d → %d)", before, after)
	}

	al.clearActiveTurn(first)
	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "lembrete" {
		t.Fatalf("last chat message after both turns = %q, want the delivery", got.Content)
	}
}

// An error notice goes to the chat but is not conversation; and a reply whose
// origin is unknown (a turn the pod received itself, with no session key) can't
// be told apart from the chat's own.
func TestMirrorDelivery_SkipsErrorNoticesAndUnknownOrigins(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	before := len(sessions.GetHistory(mirrorChatSession))

	al.maybePublishError(context.Background(), "telegram", "123", "", errors.New("provider down"))
	al.maybePublishError(context.Background(), "telegram", "123", mirrorCronSession, errors.New("provider down"))
	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", "", "resposta do próprio chat")

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("history grew from %d to %d", before, after)
	}
}

// A session migrated from the legacy JSON store has history but no scope until
// its next inbound turn.
func TestMirrorDelivery_ReachesAMigratedSessionWithoutScope(t *testing.T) {
	al, sessions := newMirrorLoop(t)
	sessions.AddMessage(mirrorChatSession, "user", "oi")
	sessions.AddMessage(mirrorChatSession, "assistant", "Oi!")

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "Aprovo a reunião?")

	if got := lastMessage(t, sessions, mirrorChatSession); got.Content != "Aprovo a reunião?" {
		t.Fatalf("last chat message = %q, want the delivery", got.Content)
	}
}

// The JSON fallback store only persists on Save; a restart must not lose the
// mirrored delivery.
func TestMirrorDelivery_SurvivesARestartOnTheJSONStore(t *testing.T) {
	al, _ := newMirrorLoop(t)
	dir := t.TempDir()
	store := session.NewSessionManager(dir)
	store.AddMessage(mirrorChatSession, "user", "oi")
	if err := store.Save(mirrorChatSession); err != nil {
		t.Fatalf("save: %v", err)
	}
	al.registry.GetDefaultAgent().Sessions = store

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")

	reloaded := session.NewSessionManager(dir)
	if got := lastMessage(t, reloaded, mirrorChatSession); got.Content != "lembrete" {
		t.Fatalf("last message after reload = %q, want the mirrored delivery", got.Content)
	}
}

// A subagent turn that got an empty reply publishes processSystemMessage's
// DefaultResponse; that synthesized text is not conversation.
func TestMirrorDelivery_SubagentFallbackIsNotMirrored(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: ""})
	al.SetDeliverySessionResolver(func(_, chatID string) string {
		if chatID == "123" {
			return mirrorChatSession
		}
		return ""
	})
	sessions := al.registry.GetDefaultAgent().Sessions
	sessions.AddMessage(mirrorChatSession, "user", "oi")
	before := len(sessions.GetHistory(mirrorChatSession))

	if _, err := al.processSystemMessage(context.Background(), bus.NormalizeInboundMessage(bus.InboundMessage{
		Context: bus.InboundContext{Channel: "system", ChatID: "telegram:123", SenderID: "subagent-1"},
		Content: "Task 'x' completed.\n\nResult:\nfeito",
	})); err != nil {
		t.Fatalf("system message: %v", err)
	}

	if after := len(sessions.GetHistory(mirrorChatSession)); after != before {
		t.Fatalf("the fallback reached the chat conversation (history %d → %d)", before, after)
	}
}

// Right after /clear the conversation is empty; a delivery would make it start
// with an assistant turn, which some providers refuse.
func TestMirrorDelivery_SkipsAClearedConversation(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	sessions.SetHistory(mirrorChatSession, nil)

	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")

	if history := sessions.GetHistory(mirrorChatSession); len(history) != 0 {
		t.Fatalf("a cleared conversation now starts with %+v", history)
	}
}

// appendingProvider writes to the session while the summarizer waits on it, as
// the next turn or a mirrored delivery would.
type appendingProvider struct {
	sessions session.SessionStore
	key      string
}

func (p *appendingProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.sessions.AddMessage(p.key, "assistant", "entrega durante o resumo")
	return &providers.LLMResponse{Content: "resumo"}, nil
}

func (p *appendingProvider) GetDefaultModel() string { return "mock-model" }

// Summarizing keeps the messages after the summarized prefix, including what was
// written during the summary call; trimming to the old count would drop an
// unsummarized one instead.
func TestSummarizeSession_KeepsWhatWasWrittenDuringTheSummary(t *testing.T) {
	al, sessions := newMirrorLoop(t)
	agent := al.registry.GetDefaultAgent()
	agent.Provider = &appendingProvider{sessions: sessions, key: mirrorChatSession}
	agent.KeepLastMessages = 4
	sessions.SetHistory(mirrorChatSession, []providers.Message{
		msg("user", "Q1"), msg("assistant", "A1"),
		msg("user", "Q2"), msg("assistant", "A2"),
		msg("user", "Q3"), msg("assistant", "A3"),
	})

	(&legacyContextManager{al: al}).summarizeSession(agent, mirrorChatSession)

	history := sessions.GetHistory(mirrorChatSession)
	if len(history) != 5 || history[0].Content != "Q2" || history[4].Content != "entrega durante o resumo" {
		t.Fatalf("history after summary = %+v, want Q2..A3 plus the message written during it", history)
	}
}

func TestDeliveredText(t *testing.T) {
	for _, c := range []struct {
		name    string
		content string
		parts   []bus.MediaPart
		want    string
	}{
		{name: "text only", content: " Bom dia ", want: "Bom dia"},
		{
			name:    "text and file",
			content: "Segue o relatório",
			parts:   []bus.MediaPart{{Type: "file", Filename: "dre.pdf"}},
			want:    "Segue o relatório\n\n[attachments: dre.pdf]",
		},
		{
			name:  "unnamed media falls back to its type",
			parts: []bus.MediaPart{{Type: "image"}, {Type: "file", Filename: "a.csv"}},
			want:  "[attachments: image, a.csv]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := deliveredText(c.content, c.parts); got != c.want {
				t.Fatalf("deliveredText = %q, want %q", got, c.want)
			}
		})
	}
}
