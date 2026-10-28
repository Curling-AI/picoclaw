package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type testPublicProjector struct{}

func (testPublicProjector) Commit(message providers.Message) (memory.PublicHistoryProjectionResult, error) {
	projected := message
	projected.Content = "versioned:" + message.Content
	return memory.PublicHistoryProjectionResult{Message: projected, HasVersionedReferences: true}, nil
}
func (testPublicProjector) Legacy(message providers.Message) (memory.PublicHistoryProjectionResult, error) {
	return memory.PublicHistoryProjectionResult{Message: message}, nil
}

func TestPublicHistoryProjectorSurvivesRegistryReloadWithoutChangingModelHistory(t *testing.T) {
	loop, cfg, _, provider, cleanup := newTestAgentLoop(t)
	defer cleanup()
	var calls atomic.Int32
	loop.SetPublicHistoryProjector(func(string) memory.PublicHistoryProjector { calls.Add(1); return testPublicProjector{} })
	key := "agent:main:web:projection"
	loop.GetRegistry().GetDefaultAgent().Sessions.AddFullMessage(key, providers.Message{Role: "assistant", Content: "before"})
	if reloadErr := loop.ReloadProviderAndConfig(t.Context(), provider, cfg); reloadErr != nil {
		t.Fatal(reloadErr)
	}
	sessions := loop.GetRegistry().GetDefaultAgent().Sessions
	sessions.AddFullMessage(key, providers.Message{Role: "assistant", Content: "after"})
	if calls.Load() < 2 {
		t.Fatal("new registry became visible without its producer projection")
	}
	history, readErr := sessions.(interface {
		ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error)
	}).ReadPublicHistory(t.Context(), key)
	if readErr != nil || len(history.Entries) != 2 || history.Entries[1].Message.Content != "versioned:after" {
		t.Fatalf("public projection lost across reload: %+v %v", history, readErr)
	}
	for _, message := range sessions.GetHistory(key) {
		if strings.HasPrefix(message.Content, "versioned:") || message.HistoryPublicMessage != nil {
			t.Fatal("private public projection entered provider context")
		}
	}
}
