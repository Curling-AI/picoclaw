package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestTurnHistoryProvenanceStaysOutOfProviderMessages(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 2,
			},
		},
	}
	provider := &recordingProvider{}
	loop := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	key, run := "history-provenance", uuid.NewString()
	ctx := providers.WithHistoryRun(t.Context(), run)
	if _, err := loop.ProcessDirectWithMedia(ctx, "synthetic prompt", key, "grpc", run, nil); err != nil {
		t.Fatal(err)
	}
	instance, ok := loop.registry.GetAgent("main")
	if !ok {
		t.Fatal("default agent absent")
	}
	reader, ok := instance.Sessions.(interface {
		ReadHistoryPage(ctx context.Context, request memory.HistoryPageRequest) (memory.HistoryPage, error)
	})
	if !ok {
		t.Fatal("bounded reader absent")
	}
	page, err := reader.ReadHistoryPage(t.Context(), memory.HistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) < 2 {
		t.Fatal("turn persistence incomplete", err)
	}
	for _, entry := range page.Entries {
		if entry.Message.HistoryRunID != run || uuid.Validate(entry.ID) != nil {
			t.Fatal("turn provenance missing")
		}
	}
	for _, message := range provider.lastMessages {
		if message.HistoryID != "" || message.HistoryRunID != "" {
			t.Fatal("persistence identity reached provider")
		}
	}
}
