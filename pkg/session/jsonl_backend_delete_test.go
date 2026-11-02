package session_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func recordKeys(b *session.JSONLBackend) []string {
	records := b.ListSessionRecords()
	keys := make([]string, 0, len(records))
	for _, r := range records {
		keys = append(keys, r.SessionKey)
	}
	slices.Sort(keys)
	return keys
}

func seedScopedSession(b *session.JSONLBackend, key string) {
	scope := &session.SessionScope{
		Version: session.ScopeVersionV1,
		AgentID: "main",
		Channel: "telegram",
		Values:  map[string]string{"chat": "direct:42"},
	}
	b.EnsureSessionMetadata(key, scope, nil)
	b.AddMessage(key, "user", "hello")
	b.AddMessage(key, "assistant", "hi")
	b.SetSummary(key, "greetings")
}

func TestJSONLBackend_DeleteSessionDropsItFromEveryListing(t *testing.T) {
	b := newBackend(t)
	seedScopedSession(b, "gone")
	seedScopedSession(b, "kept")

	if err := b.DeleteSession("gone"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	if got := b.ListSessions(); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("ListSessions() = %v, want [kept]", got)
	}
	if got := recordKeys(b); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("ListSessionRecords() = %v, want [kept]", got)
	}
	if got := b.GetHistory("gone"); len(got) != 0 {
		t.Errorf("GetHistory() = %v, want empty", got)
	}
	if got := b.GetSummary("gone"); got != "" {
		t.Errorf("GetSummary() = %q, want empty", got)
	}
	if got := b.GetSessionScope("gone"); got != nil {
		t.Errorf("GetSessionScope() = %+v, want none", got)
	}
	if got := b.GetHistory("kept"); len(got) != 2 {
		t.Errorf("the other session has %d messages, want 2", len(got))
	}
}

// Every other operation of the backend resolves an alias to the session it
// belongs to, and delete follows: it must not leave the canonical session
// behind because it was asked for by an older name.
func TestJSONLBackend_DeleteSessionResolvesAnAlias(t *testing.T) {
	b := newBackend(t)
	scope := &session.SessionScope{Version: session.ScopeVersionV1, AgentID: "main", Channel: "whatsapp"}
	b.AddMessage("agent:main:direct:7", "user", "before the canonical key")
	b.EnsureSessionMetadata("canonical", scope, []string{"agent:main:direct:7"})
	b.AddMessage("canonical", "user", "hello")

	if err := b.DeleteSession("agent:main:direct:7"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	if got := recordKeys(b); len(got) != 0 {
		t.Errorf("ListSessionRecords() = %v, want none", got)
	}
}

func TestJSONLBackend_DeleteSessionNeedsAStoreThatCanDelete(t *testing.T) {
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.AddMessage(t.Context(), "s1", "user", "hello")

	// Only the embedded interface's methods are promoted, so the wrapper
	// has no DeleteSession.
	b := session.NewJSONLBackend(struct{ memory.Store }{store})
	if err := b.DeleteSession("s1"); err == nil {
		t.Fatal("DeleteSession on a store that cannot delete reported success")
	}
	if got := b.GetHistory("s1"); len(got) != 1 {
		t.Errorf("a refused delete changed the history: %v", got)
	}
}

func TestJSONLBackend_PruneEmptySessionsRemovesAbandonedGhostsOnly(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	b := session.NewJSONLBackend(store)

	b.AddMessage("ghost", "user", "hello")
	b.SetHistory("ghost", nil)
	b.AddMessage("alive", "user", "hello")
	old := time.Now().Add(-48 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if chErr := os.Chtimes(filepath.Join(dir, entry.Name()), old, old); chErr != nil {
			t.Fatal(chErr)
		}
	}

	var seen []string
	removed, err := b.PruneEmptySessions(24*time.Hour, time.Now(), func(meta memory.SessionMeta) error {
		seen = append(seen, meta.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("PruneEmptySessions: %v", err)
	}

	if !slices.Equal(removed, []string{"ghost"}) || !slices.Equal(seen, []string{"ghost"}) {
		t.Errorf("removed = %v, hook saw %v; want only [ghost]", removed, seen)
	}
	if got := recordKeys(b); !slices.Equal(got, []string{"alive"}) {
		t.Errorf("ListSessionRecords() = %v, want [alive]", got)
	}
}

func TestJSONLBackend_PruneEmptySessionsNeedsAStoreThatCan(t *testing.T) {
	store, err := memory.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	b := session.NewJSONLBackend(struct{ memory.Store }{store})
	if _, err := b.PruneEmptySessions(time.Hour, time.Now(), nil); err == nil {
		t.Fatal("PruneEmptySessions on a store that cannot prune reported success")
	}
}

// The pre-migration history lives under the legacy key, and the next message
// to the conversation promotes it into an empty canonical session.
func TestJSONLBackend_ADeletedConversationDoesNotComeBackFromItsLegacyAlias(t *testing.T) {
	b := newBackend(t)
	const legacy = "agent:main:whatsapp:direct:7"
	scope := &session.SessionScope{Version: session.ScopeVersionV1, AgentID: "main", Channel: "whatsapp"}
	b.AddMessage(legacy, "user", "legacy secret")
	b.EnsureSessionMetadata("canonical", scope, []string{legacy})
	if got := b.GetHistory("canonical"); len(got) != 1 {
		t.Fatalf("the legacy history was not promoted: %v", got)
	}

	if err := b.DeleteSession("canonical"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	b.EnsureSessionMetadata("canonical", scope, []string{legacy})

	if got := b.GetHistory("canonical"); len(got) != 0 {
		t.Errorf("the deleted conversation came back: %v", got)
	}
	if slices.Contains(b.ListSessions(), legacy) {
		t.Error("the legacy alias is still listed")
	}
}
