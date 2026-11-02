package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

type emptySessionPruner interface {
	PruneEmptySessions(
		minIdle time.Duration,
		now time.Time,
		beforeRemove func(memory.SessionMeta) error,
	) ([]string, error)
}

// A sweep started before a config reload keeps running after it. A message
// that reaches the reloaded agent while the sweep is removing that session must
// survive it, so the reload keeps the store, and with it the lock the sweep
// holds.
func TestPruneEmptySessions_AMessageThroughTheReloadedStoreSurvivesTheSweep(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &countingReplyProvider{})
	t.Cleanup(al.Close)
	const key = "agent:main:whatsapp:direct:42"
	old := al.GetRegistry().GetDefaultAgent().Sessions
	scopes, ok := old.(session.MetadataAwareSessionStore)
	if !ok {
		t.Fatal("the session store keeps no metadata")
	}
	scopes.EnsureSessionMetadata(
		key,
		&session.SessionScope{Version: session.ScopeVersionV1, AgentID: "main", Channel: "whatsapp"},
		nil,
	)
	meta := filepath.Join(cfg.Agents.Defaults.Workspace, "sessions", "agent_main_whatsapp_direct_42.meta.json")
	stamp := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(meta, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	pruner, ok := old.(emptySessionPruner)
	if !ok {
		t.Fatal("the session store cannot prune")
	}

	written := make(chan struct{})
	_, err := pruner.PruneEmptySessions(24*time.Hour, time.Now(), func(memory.SessionMeta) error {
		if err := al.ReloadProviderAndConfig(context.Background(), &countingReplyProvider{}, cfg); err != nil {
			t.Errorf("ReloadProviderAndConfig: %v", err)
		}
		reloaded := al.GetRegistry().GetDefaultAgent().Sessions
		if reloaded != old {
			t.Error("the reload opened a second session store on the directory")
		}
		go func() {
			reloaded.AddMessage(key, "user", "olá")
			close(written)
		}()
		select {
		case <-written:
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatalf("PruneEmptySessions: %v", err)
	}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("the write through the reloaded store never finished")
	}

	if got := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(key); len(got) != 1 {
		t.Fatalf("history = %v; the message written during the sweep is gone", got)
	}
}

// A turn that started before a config reload still writes through the store it
// was given. A delete through the reloaded agent must stick: with a second
// store, the old one's cached meta came back with that write, summary and all.
func TestReload_ADeleteSticksForWritesThroughTheStoreFromBefore(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &countingReplyProvider{})
	t.Cleanup(al.Close)
	const key = "agent:main:whatsapp:direct:42"
	before := al.GetRegistry().GetDefaultAgent().Sessions
	before.AddMessage(key, "user", "hello")
	before.SetSummary(key, "greetings")
	before.ListSessions()

	if err := al.ReloadProviderAndConfig(context.Background(), &countingReplyProvider{}, cfg); err != nil {
		t.Fatalf("ReloadProviderAndConfig: %v", err)
	}
	after := al.GetRegistry().GetDefaultAgent().Sessions
	deleter, ok := after.(interface{ DeleteSession(key string) error })
	if !ok {
		t.Fatal("the session store cannot delete")
	}
	if err := deleter.DeleteSession(key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	before.AddMessage(key, "user", "again")

	if got := after.GetSummary(key); got != "" {
		t.Errorf("summary = %q, the deleted conversation's came back", got)
	}
	if got := after.GetHistory(key); len(got) != 1 {
		t.Errorf("history = %v, want only the message written after the delete", got)
	}
}
