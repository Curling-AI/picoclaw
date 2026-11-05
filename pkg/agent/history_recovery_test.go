package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestSessionStoreHistoryRecoveryIsolatesBrokenSession(t *testing.T) {
	t.Setenv(cronRetentionEnv, "0")
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"healthy", "broken"} {
		if err := store.AddMessage(t.Context(), key, "user", key+" history"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(dir, "broken.jsonl.commit.json"),
		[]byte(`{"meta":{},"next":{}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	backend := initSessionStore(dir)
	jsonl, ok := backend.(*session.JSONLBackend)
	if !ok {
		t.Fatalf("one broken journal replaced the whole JSONL backend with %T", backend)
	}
	healthy := backend.GetHistory("healthy")
	if len(healthy) != 1 || healthy[0].Content != "healthy history" {
		t.Fatal("startup migration hid another session", healthy)
	}
	if _, err := jsonl.ReadHistoryPage(t.Context(), memory.HistoryPageRequest{Key: "broken"}); err == nil {
		t.Fatal("broken journal was not isolated")
	}
}
