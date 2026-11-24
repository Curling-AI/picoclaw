package memory

import (
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// A reader of the conversation tells the runtime's fixed text from a reply by
// its kind, also when it reads the volume while the pod sleeps: the display
// projection used to drop the kind, and that reader got a reply.
func TestPublicHistoryKeepsTheFallbackKind(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, run := "agent:web:fallback-kind", uuid.NewString()
	for _, m := range []providers.Message{
		{Role: "user", Content: "organize my files"},
		{Role: "assistant", Content: "I stopped at the step limit.", Fallback: protocoltypes.FallbackToolLimit},
	} {
		if err = s.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, m); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	fromVolume, err := ReadPublicHistoryFS(t.Context(), os.DirFS(s.dir), key)
	if err != nil {
		t.Fatal(err)
	}
	for name, history := range map[string]PublicHistory{"store": live, "volume": fromVolume} {
		if len(history.Entries) != 2 {
			t.Fatalf("%s: %d entries, want 2", name, len(history.Entries))
		}
		if got := history.Entries[1].Message.Fallback; got != protocoltypes.FallbackToolLimit {
			t.Errorf("%s: fallback kind = %q, want %q", name, got, protocoltypes.FallbackToolLimit)
		}
		if got := history.Entries[0].Message.Fallback; got != "" {
			t.Errorf("%s: the person's message has fallback kind %q", name, got)
		}
	}
}
