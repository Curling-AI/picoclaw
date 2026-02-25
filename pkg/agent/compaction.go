package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// CompactSession summarizes old conversation history and persists the summary
// to the transcript. This replaces the old summarizeSession with JSONL-aware
// persistent compaction.
func (al *AgentLoop) CompactSession(ctx context.Context, agent *AgentInstance, sessionKey string) error {
	history := agent.Sessions.GetHistory(sessionKey)
	summary := agent.Sessions.GetSummary(sessionKey)

	keepLast := agent.KeepLastMessages
	if keepLast <= 0 {
		keepLast = 6
	}
	if len(history) <= keepLast {
		return nil
	}

	toSummarize := history[:len(history)-keepLast]

	// Oversized message guard
	maxMessageTokens := agent.ContextWindow / 2
	validMessages := make([]providers.Message, 0)
	omitted := false

	for _, m := range toSummarize {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		msgTokens := len(m.Content) / 2
		if msgTokens > maxMessageTokens {
			omitted = true
			continue
		}
		validMessages = append(validMessages, m)
	}

	if len(validMessages) == 0 {
		return nil
	}

	// Multi-part summarization (reuses existing logic)
	var finalSummary string
	if len(validMessages) > 10 {
		mid := len(validMessages) / 2
		part1 := validMessages[:mid]
		part2 := validMessages[mid:]

		s1, _ := al.summarizeBatch(ctx, agent, part1, "")
		s2, _ := al.summarizeBatch(ctx, agent, part2, "")

		mergePrompt := fmt.Sprintf(
			"Merge these two conversation summaries into one cohesive summary:\n\n1: %s\n\n2: %s",
			s1, s2,
		)
		resp, err := agent.Provider.Chat(
			ctx,
			[]providers.Message{{Role: "user", Content: mergePrompt}},
			nil,
			agent.Model,
			map[string]any{
				"max_tokens":  1024,
				"temperature": 0.3,
			},
		)
		if err == nil {
			finalSummary = resp.Content
		} else {
			finalSummary = s1 + " " + s2
		}
	} else {
		finalSummary, _ = al.summarizeBatch(ctx, agent, validMessages, summary)
	}

	if omitted && finalSummary != "" {
		finalSummary += "\n[Note: Some oversized messages were omitted from this summary for efficiency.]"
	}

	if finalSummary == "" {
		return nil
	}

	// Write summary entry to transcript
	tw := agent.Sessions.GetTranscriptWriter(sessionKey)
	if tw != nil {
		tw.Append(session.TranscriptEntry{
			Kind:    "summary",
			Summary: finalSummary,
		})
		// Write compaction meta event
		tw.Append(session.TranscriptEntry{
			Kind: "meta",
			Meta: map[string]any{
				"action":             "compaction",
				"messages_before":    len(history),
				"messages_after":     keepLast,
				"summarized_count":   len(validMessages),
				"timestamp":          time.Now().Format(time.RFC3339),
			},
		})
	}

	// Update in-memory session
	agent.Sessions.SetSummary(sessionKey, finalSummary)
	agent.Sessions.TruncateHistory(sessionKey, keepLast)
	agent.Sessions.Save(sessionKey)

	logger.InfoCF("agent", "Compaction completed", map[string]any{
		"session_key":    sessionKey,
		"messages_before": len(history),
		"messages_after":  keepLast,
		"summary_len":    len(finalSummary),
	})

	return nil
}
