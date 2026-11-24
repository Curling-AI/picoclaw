package session

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
)

// Another process sharing sessions/ (the other pod of a rollout) writes the
// session after this backend cached its meta. RefreshSession, which the agent
// calls when a turn starts, makes the turn's reads see that write.
func TestJSONLBackendRefreshSessionRereadsAnotherWriter(t *testing.T) {
	dir := t.TempDir()
	key := "agent:main:direct:refresh"
	other, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	own, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewJSONLBackend(own)
	backend.AddMessage(key, "user", "one")
	if got := backend.GetSummary(key); got != "" {
		t.Fatalf("precondition: summary = %q", got)
	}
	if err := other.SetSummary(context.Background(), key, "summary-other"); err != nil {
		t.Fatal(err)
	}

	var refresher SessionRefresher = backend
	if err := refresher.RefreshSession(key); err != nil {
		t.Fatalf("RefreshSession = %v", err)
	}
	if got := backend.GetSummary(key); got != "summary-other" {
		t.Fatalf("summary after RefreshSession = %q, want the other writer's", got)
	}
}
