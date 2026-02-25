package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPruneMessages_OversizedToolResult(t *testing.T) {
	bigContent := strings.Repeat("x", 5000)
	messages := []providers.Message{
		{Role: "user", Content: "search for something"},
		{Role: "assistant", Content: "I'll search."},
		{Role: "tool", Name: "web_fetch", Content: bigContent, ToolCallID: "tc1"},
	}

	pruned := PruneMessages(messages, PruningConfig{
		MaxToolResultChars: 4000,
		KeepHeadChars:      500,
		KeepTailChars:      500,
	})

	if len(pruned) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(pruned))
	}

	// Tool result should be trimmed
	if len(pruned[2].Content) >= 5000 {
		t.Errorf("tool result should be trimmed, got %d chars", len(pruned[2].Content))
	}
	if !strings.Contains(pruned[2].Content, "[...trimmed") {
		t.Errorf("expected trimmed marker, got: %s", pruned[2].Content[:100])
	}
}

func TestPruneMessages_SmallToolResultPassthrough(t *testing.T) {
	messages := []providers.Message{
		{Role: "tool", Name: "read_file", Content: "small content", ToolCallID: "tc1"},
	}

	pruned := PruneMessages(messages, PruningConfig{MaxToolResultChars: 4000})

	if pruned[0].Content != "small content" {
		t.Errorf("small tool result should pass through unchanged, got: %q", pruned[0].Content)
	}
}

func TestPruneMessages_NonToolUntouched(t *testing.T) {
	bigContent := strings.Repeat("x", 10000)
	messages := []providers.Message{
		{Role: "user", Content: bigContent},
		{Role: "assistant", Content: bigContent},
	}

	pruned := PruneMessages(messages, PruningConfig{MaxToolResultChars: 4000})

	for i, msg := range pruned {
		if msg.Content != bigContent {
			t.Errorf("message %d (role=%s) should not be pruned", i, msg.Role)
		}
	}
}

func TestPruneMessages_SkipToolNames(t *testing.T) {
	bigContent := strings.Repeat("x", 5000)
	messages := []providers.Message{
		{Role: "tool", Name: "web_fetch", Content: bigContent, ToolCallID: "tc1"},
		{Role: "tool", Name: "protected_tool", Content: bigContent, ToolCallID: "tc2"},
	}

	pruned := PruneMessages(messages, PruningConfig{
		MaxToolResultChars: 4000,
		SkipToolNames:      []string{"protected_tool"},
	})

	// web_fetch should be trimmed
	if !strings.Contains(pruned[0].Content, "[...trimmed") {
		t.Errorf("web_fetch should be trimmed")
	}

	// protected_tool should not be trimmed
	if pruned[1].Content != bigContent {
		t.Errorf("protected_tool should not be trimmed")
	}
}

func TestPruneMessages_DoesNotMutateInput(t *testing.T) {
	bigContent := strings.Repeat("x", 5000)
	original := []providers.Message{
		{Role: "tool", Name: "web_fetch", Content: bigContent, ToolCallID: "tc1"},
	}

	_ = PruneMessages(original, PruningConfig{MaxToolResultChars: 4000})

	if original[0].Content != bigContent {
		t.Errorf("input slice was mutated")
	}
}

func TestPruneMessages_DefaultConfig(t *testing.T) {
	bigContent := strings.Repeat("x", 5000)
	messages := []providers.Message{
		{Role: "tool", Name: "web_fetch", Content: bigContent, ToolCallID: "tc1"},
	}

	pruned := PruneMessages(messages, PruningConfig{})

	if !strings.Contains(pruned[0].Content, "[...trimmed") {
		t.Errorf("should use default MaxToolResultChars=4000 and trim")
	}
}
