package utils

import (
	"regexp"
	"strings"
)

const (
	maxAutomationNameRunes     = 80
	maxAutomationFallbackRunes = 30
)

var automationRunPrefix = regexp.MustCompile(`(?i)^(?:rodada\s+da\s+automa(?:ção|cao)(?:[\s:.;—–\-…]+|$))+`)

// StripAutomationRunPrefix removes only the known leading operational label.
// A mention inside a real topic is deliberately left intact.
func StripAutomationRunPrefix(text string) (string, bool) {
	clean := strings.TrimSpace(text)
	prefix := automationRunPrefix.FindStringIndex(clean)
	if prefix == nil {
		return clean, false
	}
	return strings.TrimSpace(clean[prefix[1]:]), true
}

// AutomationName prefers an explicit topic and falls back to the task itself.
// It never changes the task payload, and bounds names independently of debug
// switches that can disable generic message truncation.
func AutomationName(name, task string) string {
	limit := maxAutomationNameRunes
	clean, prefixed := StripAutomationRunPrefix(name)
	if prefixed && (strings.HasSuffix(clean, "...") || strings.HasSuffix(clean, "…")) {
		clean = "" // Recover old 30-character names from the complete task.
	}
	if clean == "" {
		clean, _ = StripAutomationRunPrefix(task)
		// Names appear in public creation receipts. Preserve the old bounded
		// preview when no explicit name was supplied, rather than exposing the task.
		limit = maxAutomationFallbackRunes
	}
	clean, _, _ = strings.Cut(clean, "\n")
	clean = strings.Trim(strings.Join(strings.Fields(clean), " "), "\"'“”‘’`")
	if clean == "" {
		clean = "Automação agendada"
	}
	runes := []rune(clean)
	if len(runes) > limit {
		clean = strings.TrimSpace(string(runes[:limit-3])) + "..."
	}
	return clean
}
