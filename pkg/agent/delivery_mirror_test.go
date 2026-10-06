package agent

import (
	"context"
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

// newMirrorTestLoop builds a loop whose Telegram chat 123 already has a
// conversation, with the resolver the control plane would install.
func newMirrorTestLoop(t *testing.T) (*AgentLoop, session.SessionStore) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockProvider{})
	sessions := al.registry.GetDefaultAgent().Sessions

	ensureSessionMetadata(sessions, mirrorChatSession,
		&session.SessionScope{Version: session.ScopeVersionV1, AgentID: "main", Channel: "telegram"}, nil)
	sessions.AddMessage(mirrorChatSession, "user", "oi")
	sessions.AddMessage(mirrorChatSession, "assistant", "Oi! Em que posso ajudar?")

	al.SetDeliverySessionResolver(func(channel, chatID string) string {
		if channel == "telegram" && chatID == "123" {
			return mirrorChatSession
		}
		return ""
	})
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

// A queued message can take the session over between the end of one turn and
// the flush; the delivery then waits for that turn as well.
func TestMirrorDelivery_DeferredDeliveryWaitsForTheTurnThatTookOver(t *testing.T) {
	al, sessions := newMirrorTestLoop(t)
	first := &turnState{turnID: "turn-1", sessionKey: mirrorChatSession}
	al.registerActiveTurn(first)
	al.PublishResponseIfNeeded(context.Background(), "telegram", "123", mirrorCronSession, "lembrete")
	before := len(sessions.GetHistory(mirrorChatSession))

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
