package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestSessionHistoryObserverSeesPersistedOutOfTurnNotes(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	const key = "background-observer-session"
	const content = "[System: async:spawn] completed after the turn"
	var observedKey string
	var observed []providers.Message
	al.SetSessionHistoryObserver(func(sessionKey string) {
		observedKey = sessionKey
		observed = agent.Sessions.GetHistory(sessionKey)
	})
	if err := al.writeMirrored(agent, key, []providers.Message{{Role: "user", Content: content}}); err != nil {
		t.Fatal(err)
	}
	if observedKey != key || len(observed) != 1 || observed[0].Content != content {
		t.Fatalf("observer missed saved note: key=%q history=%+v", observedKey, observed)
	}
	al.SetSessionHistoryObserver(nil)
	observedKey = ""
	if err := al.writeMirrored(agent, key, []providers.Message{{Role: "user", Content: "another note"}}); err != nil {
		t.Fatal(err)
	}
	if observedKey != "" {
		t.Fatal("detached observer was called")
	}
}
