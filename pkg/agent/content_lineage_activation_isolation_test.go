package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestContentLineageLegacyCompactionIgnoresUnrelatedBrokenMetadata(t *testing.T) {
	al := newCMTestAgentLoop(testConfig(t))
	t.Cleanup(al.Close)
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-job-run-1"
	backend := session.NewJSONLBackend(store)
	al.registry.GetDefaultAgent().Sessions = backend
	for i := range 5 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		backend.AddMessage(key, role, "legacy content")
	}
	if err = os.WriteFile(filepath.Join(dir, "other.meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = al.contextManager.Compact(t.Context(), &CompactRequest{
		SessionKey: key, Reason: ContextCompressReasonRetry,
	}); err != nil {
		t.Fatal("unrelated malformed meta changed never-initialized legacy behavior", err)
	}
	if got := backend.GetHistory(key); len(got) != 3 {
		t.Fatal("never-initialized source no longer follows its existing compaction", got)
	}
}
