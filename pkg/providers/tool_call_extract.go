package providers

import (
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// extractToolCallsFromText parses tool call JSON from response text.
// Both ClaudeCliProvider and CodexCliProvider use this to extract
// tool calls that the model outputs in its response text.
func extractToolCallsFromText(text string) []ToolCall {
	return protocoltypes.ExtractToolCallsFromText(text)
}

// stripToolCallsFromText removes tool call JSON from response text.
func stripToolCallsFromText(text string) string {
	return protocoltypes.StripToolCallsFromText(text)
}
