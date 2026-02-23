package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestSanitizeHistory_MultipleToolResults(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "do stuff"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{ID: "call_1", Name: "read_file"},
				{ID: "call_2", Name: "write_file"},
				{ID: "call_3", Name: "exec"},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "ok1"},
		{Role: "tool", ToolCallID: "call_2", Content: "ok2"},
		{Role: "tool", ToolCallID: "call_3", Content: "ok3"},
	}

	sanitized := sanitizeHistoryForProvider(history)

	// All 5 messages should survive: user + assistant + 3 tool results
	if len(sanitized) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(sanitized))
	}
	for i, role := range []string{"user", "assistant", "tool", "tool", "tool"} {
		if sanitized[i].Role != role {
			t.Errorf("message[%d]: expected role %q, got %q", i, role, sanitized[i].Role)
		}
	}
}

func TestSanitizeHistory_BackfillsToolResultName(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{
					ID:   "call_abc",
					Name: "search",
					Function: &providers.FunctionCall{
						Name:      "search",
						Arguments: `{}`,
					},
				},
			},
		},
		// Old session data: tool result without Name
		{Role: "tool", ToolCallID: "call_abc", Content: "results"},
	}

	sanitized := sanitizeHistoryForProvider(history)
	if len(sanitized) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(sanitized))
	}

	toolMsg := sanitized[2]
	if toolMsg.Name != "search" {
		t.Fatalf("expected backfilled Name %q, got %q", "search", toolMsg.Name)
	}
}

func TestSanitizeHistory_BackfillsFromFunctionField(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{
					ID: "call_xyz",
					// Name not set, only Function.Name
					Function: &providers.FunctionCall{
						Name:      "get_weather",
						Arguments: `{"city":"SF"}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_xyz", Content: `{"temp":72}`},
	}

	sanitized := sanitizeHistoryForProvider(history)
	toolMsg := sanitized[2]
	if toolMsg.Name != "get_weather" {
		t.Fatalf("expected backfilled Name %q, got %q", "get_weather", toolMsg.Name)
	}
}

func TestSanitizeHistory_BackfillsMultipleToolResults(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{ID: "call_1", Name: "read_file"},
				{ID: "call_2", Name: "write_file"},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "ok1"},
		{Role: "tool", ToolCallID: "call_2", Content: "ok2"},
	}

	sanitized := sanitizeHistoryForProvider(history)
	if len(sanitized) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(sanitized))
	}

	if sanitized[2].Name != "read_file" {
		t.Fatalf("tool[0]: expected Name %q, got %q", "read_file", sanitized[2].Name)
	}
	if sanitized[3].Name != "write_file" {
		t.Fatalf("tool[1]: expected Name %q, got %q", "write_file", sanitized[3].Name)
	}
}

func TestSanitizeHistory_PreservesExistingName(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{ID: "call_1", Name: "read_file"},
			},
		},
		{Role: "tool", Name: "already_set", ToolCallID: "call_1", Content: "ok"},
	}

	sanitized := sanitizeHistoryForProvider(history)
	if sanitized[2].Name != "already_set" {
		t.Fatalf("expected preserved Name %q, got %q", "already_set", sanitized[2].Name)
	}
}

func TestSanitizeHistory_DropsOrphanedToolMessages(t *testing.T) {
	// Tool message without preceding assistant
	history := []providers.Message{
		{Role: "tool", ToolCallID: "call_1", Content: "orphan"},
	}
	sanitized := sanitizeHistoryForProvider(history)
	if len(sanitized) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(sanitized))
	}

	// Tool message after user (no tool calls)
	history = []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "call_1", Content: "orphan"},
	}
	sanitized = sanitizeHistoryForProvider(history)
	if len(sanitized) != 1 {
		t.Fatalf("expected 1 message (user only), got %d", len(sanitized))
	}
}
