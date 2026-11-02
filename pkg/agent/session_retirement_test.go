package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestDeletedSessionRejectsResolvedBackgroundResult(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "deleted"
		if reuse {
			name = "reused key"
		}
		t.Run(name, func(t *testing.T) {
			al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
			message := spawnResultMessage(conversationSession)
			message.LaunchedAt = time.Now()
			key, agent, ok := al.backgroundResultTarget(message)
			if !ok {
				t.Fatal("background result target was unavailable")
			}
			deleter, ok := sessions.(interface{ DeleteSession(key string) error })
			if !ok {
				t.Fatal("fixture lacks session deletion")
			}
			remove := func() error { return deleter.DeleteSession(key) }
			if err := al.RetireSession(t.Context(), key, remove); err != nil {
				t.Fatal(err)
			}
			if reuse {
				sessions.AddMessage(key, "user", "fresh question")
			}
			if _, err := al.deliverBackgroundNote(agent, key, message); !errors.Is(err, errClearedSinceLaunch) {
				t.Fatal("old background result survived deletion", err)
			}
			want := 0
			if reuse {
				want = 1
			}
			if history := sessions.GetHistory(key); len(history) != want {
				t.Fatal("old result recreated source or reached new conversation", history)
			}
			if reuse {
				fresh := spawnResultMessage(key)
				fresh.LaunchedAt = time.Now()
				if _, err := al.deliverBackgroundNote(agent, key, fresh); err != nil {
					t.Fatal("fresh result was rejected", err)
				}
			}
		})
	}
}

func TestDeletedSessionRejectsResolvedMirroredDelivery(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	stale := providers.Message{
		Role:     "assistant",
		Content:  "old delivery",
		Delivery: &providers.Delivery{Origin: "old origin", At: time.Now()},
	}
	deleter, ok := sessions.(interface{ DeleteSession(key string) error })
	if !ok {
		t.Fatal("fixture lacks session deletion")
	}
	if err := al.RetireSession(
		t.Context(),
		conversationSession,
		func() error { return deleter.DeleteSession(conversationSession) },
	); err != nil {
		t.Fatal(err)
	}
	sessions.AddMessage(conversationSession, "user", "fresh question")
	if _, err := al.deliverToConversation(agent, conversationSession, stale); !errors.Is(err, errClearedSinceLaunch) {
		t.Fatal("resolved old delivery reached reused key", err)
	}
	fresh := stale
	fresh.Content = "fresh delivery"
	fresh.Delivery = &providers.Delivery{Origin: "new origin", At: time.Now()}
	if _, err := al.deliverToConversation(agent, conversationSession, fresh); err != nil {
		t.Fatal("new delivery was rejected", err)
	}
	history := sessions.GetHistory(conversationSession)
	if len(history) != 2 || history[1].Content != "fresh delivery" {
		t.Fatal("wrong conversation delivery", history)
	}
}
