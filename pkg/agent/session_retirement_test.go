package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
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

func TestDeletedSessionRejectsDeliveryResolvedDuringRemoval(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "retired", true: "failed removal"}[failed], func(t *testing.T) {
			al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
			agent := al.registry.GetDefaultAgent()
			stale := providers.Message{Role: "assistant", Content: "resolved during removal"}
			deleter, ok := sessions.(interface{ DeleteSession(key string) error })
			if !ok {
				t.Fatal("fixture lacks deletion")
			}
			removalErr := errors.New("remove failed")
			err := al.RetireSession(t.Context(), conversationSession, func() error {
				// mirrorDelivery resolves an existing conversation and stamps Delivery.At
				// before taking this stripe. Its envelope can therefore be newer than
				// the initial retirement stamp while it waits for remove to finish.
				stale.Delivery = &providers.Delivery{Origin: "inflight origin", At: time.Now()}
				if failed {
					return removalErr
				}
				return deleter.DeleteSession(conversationSession)
			})
			if failed {
				if !errors.Is(err, removalErr) {
					t.Fatal("removal failure hidden", err)
				}
				if _, err = al.deliverToConversation(agent, conversationSession, stale); err != nil {
					t.Fatal("failed removal rejected a surviving conversation delivery", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = al.deliverToConversation(
				agent,
				conversationSession,
				stale,
			); !errors.Is(
				err,
				errClearedSinceLaunch,
			) {
				t.Fatal("inflight delivery recreated retired source", err)
			}
			if h := sessions.GetHistory(conversationSession); len(h) != 0 {
				t.Fatal("retired history resurrected", h)
			}
			fresh := stale
			fresh.Delivery = &providers.Delivery{Origin: "fresh origin", At: time.Now()}
			if _, err = al.deliverToConversation(agent, conversationSession, fresh); err != nil {
				t.Fatal("fresh delivery after retirement rejected", err)
			}
		})
	}
}

type retirementReadBarrier struct {
	session.SessionStore
	observed chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (s *retirementReadBarrier) GetHistory(key string) []providers.Message {
	history := s.SessionStore.GetHistory(key)
	if key == conversationSession {
		s.once.Do(func() { close(s.observed); <-s.resume })
	}
	return history
}

func TestDeletedSessionRejectsMirrorPausedAfterConversationRead(t *testing.T) {
	al, _, sessions := newSystemMessageTestLoop(t, &countingReplyProvider{})
	agent := al.registry.GetDefaultAgent()
	barrier := &retirementReadBarrier{
		SessionStore: sessions,
		observed:     make(chan struct{}),
		resume:       make(chan struct{}),
	}
	agent.Sessions = barrier
	al.SetDeliverySessionResolver(func(string, string) string { return conversationSession })
	var release sync.Once
	unblock := func() { release.Do(func() { close(barrier.resume) }) }
	t.Cleanup(unblock)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		al.MirrorDelivery(
			tools.ExternalDelivery{
				Origin:  "old cron",
				Channel: "telegram",
				ChatID:  "123",
				Text:    "old inflight delivery",
			},
		)
	}()
	select {
	case <-barrier.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("mirror did not reach admission barrier")
	}
	deleter, ok := sessions.(interface{ DeleteSession(key string) error })
	if !ok {
		t.Fatal("fixture lacks deletion")
	}
	if err := al.RetireSession(
		t.Context(),
		conversationSession,
		func() error { return deleter.DeleteSession(conversationSession) },
	); err != nil {
		t.Fatal(err)
	}
	sessions.AddMessage(conversationSession, "user", "new conversation turn")
	unblock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("mirror did not leave admission barrier")
	}
	if history := sessions.GetHistory(
		conversationSession,
	); len(history) != 1 ||
		history[0].Content != "new conversation turn" {
		t.Fatal("pre-retirement admission reached reused conversation", history)
	}
	al.MirrorDelivery(
		tools.ExternalDelivery{Origin: "new cron", Channel: "telegram", ChatID: "123", Text: "fresh delivery"},
	)
	history := sessions.GetHistory(conversationSession)
	if len(history) != 2 || history[1].Content != "fresh delivery" {
		t.Fatal("new post-retirement delivery was rejected", history)
	}
}
