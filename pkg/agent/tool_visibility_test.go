package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestToolCallNamesFromMessages(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: "faz aí"},
		// Session-loaded history: name only inside Function.
		{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "1", Function: &providers.FunctionCall{Name: "mcp_skip_skip_file_write"}},
		}},
		{Role: "tool", Content: "ok", ToolCallID: "1"},
		// Live normalized call: name on the call itself; duplicate must dedup.
		{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "2", Name: "exec"},
			{ID: "3", Name: "mcp_skip_skip_file_write"},
		}},
	}

	got := toolCallNamesFromMessages(msgs)
	want := []string{"mcp_skip_skip_file_write", "exec"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
}

func TestDiscoveryPromoteTTL_Default(t *testing.T) {
	cfg := &config.Config{}
	if ttl := discoveryPromoteTTL(cfg); ttl != 5 {
		t.Fatalf("default ttl = %d, want 5", ttl)
	}
	cfg.Tools.MCP.Discovery.TTL = 20
	if ttl := discoveryPromoteTTL(cfg); ttl != 20 {
		t.Fatalf("configured ttl = %d, want 20", ttl)
	}
}
