package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
)

func TestStaleLegacyCountKeepsAgentTurnsAndLifecycleUsable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	dir := filepath.Join(cfg.Agents.Defaults.Workspace, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := "agent:main:cli:direct:legacy"
	base := filepath.Join(dir, "agent_main_cli_direct_legacy")
	// The production writer synced the third row but crashed before its meta.
	rows := "{\"role\":\"user\",\"content\":\"u0\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"a0\"}\n" +
		"{\"role\":\"user\",\"content\":\"u1\"}\n"
	if err := os.WriteFile(base+".jsonl", []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".meta.json", []byte(`{"key":"`+key+`","count":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loop := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: "reply"})
	t.Cleanup(loop.Close)
	for range 2 {
		if response, err := loop.ProcessDirect(t.Context(), "hello", key); err != nil || response != "reply" {
			t.Fatal("a durable legacy tail stopped the ordinary turn", response, err)
		}
	}
	agent := loop.registry.GetDefaultAgent()
	history := agent.Sessions.GetHistory(key)
	if len(history) != 7 || history[2].Content != "u1" || history[6].Content != "reply" {
		t.Fatal("turn persistence lost the durable tail or subsequent replies", history)
	}
	reader, ok := agent.Sessions.(interface {
		ReadHistoryPage(ctx context.Context, request memory.HistoryPageRequest) (memory.HistoryPage, error)
	})
	if !ok {
		t.Fatal("bounded reader absent")
	}
	assertFenced := func() {
		t.Helper()
		if _, err := reader.ReadHistoryPage(
			t.Context(),
			memory.HistoryPageRequest{Key: key},
		); !errors.Is(
			err,
			memory.ErrHistoryRecoveryRequired,
		) {
			t.Fatal("content lifecycle reattested an unproven source", err)
		}
	}
	assertFenced()
	manager, ok := loop.contextManager.(*legacyContextManager)
	if !ok {
		t.Fatal("legacy context manager absent")
	}
	compressed, changed := manager.forceCompression(key, 0)
	if !changed || len(agent.Sessions.GetHistory(key)) != compressed.RemainingMessages ||
		compressed.DroppedMessages == 0 {
		t.Fatal("emergency compression claimed success without retaining its requested suffix", compressed)
	}
	assertFenced()
	if _, err := loop.ProcessDirect(t.Context(), "/clear", key); err != nil {
		t.Fatal("explicit content clear failed", err)
	}
	if history := agent.Sessions.GetHistory(key); len(history) != 0 {
		t.Fatal("explicit clear retained the old content", history)
	}
	assertFenced()
	if response, err := loop.ProcessDirect(t.Context(), "after clear", key); err != nil || response != "reply" {
		t.Fatal("content append after clear failed", response, err)
	}
	assertFenced()
}
