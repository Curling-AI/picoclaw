package agent

import (
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestClearRemovesCompactedPublicDisplay(t *testing.T) {
	al, _, _ := newSystemMessageTestLoop(t, &countingReplyProvider{})
	source, err := memory.NewJSONLStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	al.registry.GetDefaultAgent().Sessions = session.NewJSONLBackend(source)
	for _, text := range []string{"prefix A", "prefix B", "active tail"} {
		if operationErr := source.AddMessage(t.Context(), conversationSession, "assistant", text); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	if operationErr := source.TruncateHistory(t.Context(), conversationSession, 1); operationErr != nil {
		t.Fatal(operationErr)
	}
	manager := &legacyContextManager{al: al}
	if operationErr := manager.Clear(t.Context(), conversationSession); operationErr != nil {
		t.Fatal(operationErr)
	}
	public, err := source.ReadPublicHistory(t.Context(), conversationSession)
	if err != nil || len(public.Entries) != 0 {
		t.Fatal("explicit clear retained compacted public display", public, err)
	}
}
