package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// PruningConfig controls tool result pruning behavior.
type PruningConfig struct {
	MaxToolResultChars int      // default 4000
	KeepHeadChars      int      // default 500
	KeepTailChars      int      // default 500
	SkipToolNames      []string // tool names to exclude from pruning
}

// PruneMessages returns a new message slice with oversized tool results trimmed.
// Only prunes role=="tool" messages where len(Content) > MaxToolResultChars.
// Keeps head + tail, replaces middle with "[...trimmed N chars...]".
// Never mutates the input slice.
func PruneMessages(messages []providers.Message, cfg PruningConfig) []providers.Message {
	if cfg.MaxToolResultChars <= 0 {
		cfg.MaxToolResultChars = 4000
	}
	if cfg.KeepHeadChars <= 0 {
		cfg.KeepHeadChars = 500
	}
	if cfg.KeepTailChars <= 0 {
		cfg.KeepTailChars = 500
	}

	skipSet := make(map[string]bool, len(cfg.SkipToolNames))
	for _, name := range cfg.SkipToolNames {
		skipSet[name] = true
	}

	result := make([]providers.Message, len(messages))
	copy(result, messages)

	for i, msg := range result {
		if msg.Role != "tool" {
			continue
		}
		if skipSet[msg.Name] {
			continue
		}
		if len(msg.Content) <= cfg.MaxToolResultChars {
			continue
		}

		// Head + tail must fit within max, otherwise just keep head+tail
		minKeep := cfg.KeepHeadChars + cfg.KeepTailChars
		if minKeep >= len(msg.Content) {
			continue
		}

		head := msg.Content[:cfg.KeepHeadChars]
		tail := msg.Content[len(msg.Content)-cfg.KeepTailChars:]
		trimmedCount := len(msg.Content) - cfg.KeepHeadChars - cfg.KeepTailChars
		trimmed := fmt.Sprintf("%s\n[...trimmed %d chars...]\n%s", head, trimmedCount, tail)

		result[i].Content = trimmed
	}

	return result
}
