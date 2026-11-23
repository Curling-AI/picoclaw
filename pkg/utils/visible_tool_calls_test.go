package utils

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestBuildVisibleToolCalls_DoesNotTruncateExplanation(t *testing.T) {
	explanation := "Read README.md first to confirm the current project structure before editing the config example."
	toolCalls := []providers.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: &providers.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":"README.md","start_line":1,"end_line":10,"extra":"abcdefghijklmnopqrstuvwxyz"}`,
		},
		ExtraContent: &providers.ExtraContent{
			ToolFeedbackExplanation: explanation,
		},
	}}

	visible := BuildVisibleToolCalls(toolCalls, 20)
	if len(visible) != 1 {
		t.Fatalf("len(visible) = %d, want 1", len(visible))
	}
	if visible[0].ExtraContent == nil || visible[0].ExtraContent.ToolFeedbackExplanation != explanation {
		t.Fatalf("visible explanation = %#v, want %q", visible[0].ExtraContent, explanation)
	}
	if visible[0].Function == nil || visible[0].Function.Arguments == "" {
		t.Fatalf("visible function = %#v, want truncated args preview", visible[0].Function)
	}
}

// The web reads the same payload: without a title it stays as it was.
// (seucaranguejo fork — MST-957)
func TestVisibleToolCallTitleOnlyWhenWritten(t *testing.T) {
	withTitle, err := json.Marshal(VisibleToolCall{ID: "call_1", Title: "Checking the weather"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(withTitle), `"title":"Checking the weather"`) {
		t.Fatalf("payload = %s, want the title", withTitle)
	}
	without, err := json.Marshal(VisibleToolCall{ID: "call_1"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(without), "title") {
		t.Fatalf("payload = %s, want no title", without)
	}
}
