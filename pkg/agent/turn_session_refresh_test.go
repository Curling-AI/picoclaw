package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// refreshRecordingStore records the turn's session refreshes and reads, in order.
type refreshRecordingStore struct {
	session.SessionStore
	mu     sync.Mutex
	events []string
}

func (s *refreshRecordingStore) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *refreshRecordingStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *refreshRecordingStore) RefreshSession(key string) error {
	s.record("refresh:" + key)
	return nil
}

func (s *refreshRecordingStore) GetHistory(key string) []providers.Message {
	s.record("history")
	return s.SessionStore.GetHistory(key)
}

func (s *refreshRecordingStore) GetSummary(key string) string {
	s.record("summary")
	return s.SessionStore.GetSummary(key)
}

// Another process sharing sessions/ may have written the session since this
// one cached its meta, and the store's plain reads serve that cache. A turn
// rereads its session once, before it first reads the session.
func TestTurnRefreshesItsSessionOnceBeforeReadingIt(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	store := &refreshRecordingStore{SessionStore: agent.Sessions, mu: sync.Mutex{}, events: nil}
	agent.Sessions = store
	key := "test-session-refresh"

	if _, err := al.runAgentLoop(context.Background(), agent, makeTestProcessOptsWithHistory(key)); err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	events := store.snapshot()
	refreshes := 0
	for _, event := range events {
		if strings.HasPrefix(event, "refresh:") {
			refreshes++
		}
	}
	if refreshes != 1 || len(events) < 2 || events[0] != "refresh:"+key {
		t.Fatalf("session events = %v; want one refresh of %s before the first read", events, key)
	}
}
