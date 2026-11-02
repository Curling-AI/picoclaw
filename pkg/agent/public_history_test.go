package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type testPublicProjector struct{}

func (testPublicProjector) Commit(message providers.Message) (memory.PublicHistoryProjectionResult, error) {
	projected := message
	projected.Content = "versioned:" + message.Content
	return memory.PublicHistoryProjectionResult{Message: projected, HasVersionedReferences: true}, nil
}

func TestPublicHistoryPreservesRunAndProducerRefsThroughTheRealAgentLoop(t *testing.T) {
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 2,
	}}}
	provider := &recordingProvider{}
	loop := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	loop.SetPublicHistoryProjector(func(string) memory.PublicHistoryProjector { return testPublicProjector{} })
	sessions := loop.registry.GetDefaultAgent().Sessions
	requestKey, run := "public-history-composition", uuid.NewString()
	if _, err := loop.ProcessDirectWithMedia(
		t.Context(),
		"seed",
		requestKey,
		"grpc",
		uuid.NewString(),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	keys := sessions.ListSessions()
	if len(keys) != 1 {
		t.Fatal("real route did not establish exactly one canonical source", keys)
	}
	key := keys[0]
	lineage, ok := sessions.(memory.HistoryContentLineage)
	if !ok {
		t.Fatal("real session backend has no content capability")
	}
	frame, err := lineage.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lineage.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation); err != nil {
		t.Fatal(err)
	}
	if _, err = loop.ProcessDirectWithMedia(
		providers.WithHistoryRun(t.Context(), run),
		"synthetic prompt",
		requestKey,
		"grpc",
		run,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	reader, ok := sessions.(interface {
		ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error)
	})
	if !ok {
		t.Fatal("real session backend has no public source")
	}
	history, err := reader.ReadPublicHistory(t.Context(), key)
	if err != nil || len(history.Entries) < len(frame.Entries)+2 {
		t.Fatal("real loop did not commit its display source", history, err)
	}
	for _, entry := range history.Entries[len(frame.Entries):] {
		if entry.RunID != run || entry.Ref == nil || !entry.Ref.Valid() ||
			!strings.HasPrefix(entry.Message.Content, "versioned:") {
			t.Fatal("real loop lost run, producer ref or immutable projection", entry)
		}
	}
	for _, message := range provider.lastMessages {
		if message.HistoryID != "" || message.HistoryRunID != "" || message.HistoryPublicMessage != nil ||
			strings.HasPrefix(message.Content, "versioned:") {
			t.Fatal("display identity/projection entered model context")
		}
	}
}

func (testPublicProjector) Legacy(message providers.Message) (memory.PublicHistoryProjectionResult, error) {
	return memory.PublicHistoryProjectionResult{Message: message}, nil
}

func TestPublicHistoryProjectorSurvivesRegistryReloadWithoutChangingModelHistory(t *testing.T) {
	loop, cfg, _, provider, cleanup := newTestAgentLoop(t)
	defer cleanup()
	var calls atomic.Int32
	loop.SetPublicHistoryProjector(
		func(string) memory.PublicHistoryProjector { calls.Add(1); return testPublicProjector{} },
	)
	key := "agent:main:web:projection"
	loop.GetRegistry().
		GetDefaultAgent().
		Sessions.AddFullMessage(
		key,
		providers.Message{Role: "assistant", Content: "before"},
	)
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
