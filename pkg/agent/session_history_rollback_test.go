package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestSessionHistoryRollbackToPreviousWriterKeepsRealTurnsAndClearAvailable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	dir := filepath.Join(cfg.Agents.Defaults.Workspace, "sessions")
	key := "agent:main:cli:direct:rollback"
	first := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: "reply"})
	if _, turnErr := first.ProcessDirect(t.Context(), "before rollback", key); turnErr != nil {
		first.Close()
		t.Fatal(turnErr)
	}
	first.Close()
	base := filepath.Join(dir, "agent_main_cli_direct_rollback")
	raw, readErr := os.ReadFile(base + ".meta.json")
	if readErr != nil {
		t.Fatal(readErr)
	}
	var meta map[string]json.RawMessage
	if decodeErr := json.Unmarshal(raw, &meta); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	var count int
	if decodeErr := json.Unmarshal(meta["count"], &count); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	previous := map[string]json.RawMessage{"count": json.RawMessage(strconv.Itoa(count + 2))}
	for _, field := range []string{"key", "summary", "skip", "created_at", "updated_at", "scope", "aliases"} {
		if value, ok := meta[field]; ok {
			previous[field] = value
		}
	}
	rewritten, encodeErr := json.Marshal(previous)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if writeErr := os.WriteFile(base+".meta.json", rewritten, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	file, openErr := os.OpenFile(base+".jsonl", os.O_APPEND|os.O_WRONLY, 0o600)
	if openErr != nil {
		t.Fatal(openErr)
	}
	_, writeErr := file.WriteString(
		"{\"role\":\"user\",\"content\":\"during rollback\"}\n{\"role\":\"assistant\",\"content\":\"rollback reply\"}\n",
	)
	if closeErr := file.Close(); writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
	second := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: "reply"})
	t.Cleanup(second.Close)
	for range 2 {
		if _, turnErr := second.ProcessDirect(t.Context(), "after roll-forward", key); turnErr != nil {
			t.Fatal("rolled-forward turn could not finalize content", turnErr)
		}
	}
	backend := second.registry.GetDefaultAgent().Sessions
	if history := backend.GetHistory(key); len(history) != count+6 {
		t.Fatal("rolled-forward turns lost old or new content", history)
	}
	if clearErr := second.contextManager.Clear(t.Context(), key); clearErr != nil {
		t.Fatal(clearErr)
	}
	if history := backend.GetHistory(key); len(history) != 0 {
		t.Fatal("clear kept the conversation after roll-forward", history)
	}
	if _, turnErr := second.ProcessDirect(t.Context(), "after clear", key); turnErr != nil {
		t.Fatal("turn after clear could not finalize content", turnErr)
	}
	if history := backend.GetHistory(key); len(history) != 2 {
		t.Fatal("turn after clear lost its messages", history)
	}
	_, receiptErr := backend.(session.HistoryReceiptStore).ReadHistoryWithReceipts(t.Context(), key)
	if !errors.Is(receiptErr, memory.ErrHistoryOrderUnavailable) {
		t.Fatal("content recovery made the lost stream receipt-eligible", receiptErr)
	}
}
