// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// Tool results and the active turn (seucaranguejo fork).
//
// The retry path compacts history to make room, but never the active turn:
// its messages are what the model is working on. Nothing bounded what a tool
// could put into that turn, so one result (a 3.2M-character browser snapshot,
// a 2.1M-character exec output) or many medium ones (258 web searches) could
// make the turn alone larger than the window, and the chat failed with
// "refusing to drop active turn messages". Two bounds close that:
//
//   - boundToolResultForLLM trims a single result past the limit before it
//     enters the turn, keeping its start and end and saving the full output
//     to the workspace, where read_file can page through it;
//   - elideTurnToolResultsToFit, on a context retry where even the turn alone
//     does not fit, stands a short note in for the earliest results of the
//     turn. Only in what is sent: the session keeps every result.

// toolOutputDir sits under artifacts/ to share its persistent mount; the
// Artifacts screen lists only the top level, so these files stay out of the
// user's deliverables (as artifacts/mcp does).
const toolOutputDir = "tool-output"

// toolOutputHeadShare is the part of the kept characters taken from the start
// of the output; the rest comes from its end, where commands print errors and
// summaries.
const toolOutputHeadShare = 0.75

const (
	elidedToolResultMarker = "[Tool output elided"
	// A shorter result costs about as much as the note that would replace it.
	minElidableToolResultChars = 1024
	// Each later call of the turn adds its tool results: eliding just enough
	// to fit would have the next call rejected again, at a round trip each.
	elisionHeadroomDivisor = 10
)

// boundToolResultForLLM returns content as the turn should keep it: unchanged
// up to the configured limit, otherwise trimmed and pointing at the saved
// full output.
func (al *AgentLoop) boundToolResultForLLM(ts *turnState, toolName, content string) string {
	limit := al.cfg.Tools.GetMaxResultChars()
	if len(content) <= limit {
		return content
	}
	total := utf8.RuneCountInString(content)
	if total <= limit {
		return content
	}
	fields := map[string]any{
		"tool":        toolName,
		"chars":       total,
		"limit":       limit,
		"session_key": ts.sessionKey,
	}
	path, err := saveToolOutput(ts.workspace, toolName, content)
	if err != nil {
		fields["error"] = err.Error()
	} else {
		fields["saved_to"] = path
	}
	logger.WarnCF("agent", "Tool result over the limit; trimmed before entering the turn", fields)
	return truncateToolResult(content, limit, path)
}

func saveToolOutput(workspace, toolName, content string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("no workspace to save the full output")
	}
	dir := filepath.Join(workspace, "artifacts", toolOutputDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, utils.SanitizeFilename(toolName)+"_*.txt")
	if err != nil {
		return "", fmt.Errorf("create output file: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close %s: %w", path, err)
	}
	return path, nil
}

// truncateToolResult keeps the start and the end of content within limit
// runes, notes included. savedPath is where the full output went, or "".
func truncateToolResult(content string, limit int, savedPath string) string {
	runes := []rune(content)
	total := len(runes)
	notice := truncatedToolResultNotice(total, savedPath)
	// The omitted count is below total, so this marker is never shorter than
	// the real one.
	markerRunes := utf8.RuneCountInString(omittedToolOutputMarker(total))
	keep := max(limit-utf8.RuneCountInString(notice)-markerRunes, 0)
	head := int(float64(keep) * toolOutputHeadShare)
	tail := keep - head
	return notice + string(runes[:head]) + omittedToolOutputMarker(total-keep) + string(runes[total-tail:])
}

func truncatedToolResultNotice(total int, savedPath string) string {
	where := "The full output could not be saved."
	if savedPath != "" {
		where = fmt.Sprintf(
			"The full output is saved at %s: read it in parts with read_file (offset/length) or search it with grep.",
			savedPath,
		)
	}
	return fmt.Sprintf(
		"[Tool output truncated: %d characters, too long for the conversation; only its start and end are shown. %s "+
			"Narrow the request (filters, selectors, limits) instead of repeating it.]\n\n",
		total,
		where,
	)
}

func omittedToolOutputMarker(omitted int) string {
	return fmt.Sprintf("\n\n[… %d characters omitted …]\n\n", omitted)
}

func elidedToolResultNote(chars int) string {
	return fmt.Sprintf("%s to keep this turn within the context window (%d characters). "+
		"Run the tool again with a narrower request if you still need it.]", elidedToolResultMarker, chars)
}

// withElidedToolResults returns messages with the results of the tool calls in
// elided replaced by their note. messages is the turn's own history and is
// never written; without elisions it is returned as is.
func withElidedToolResults(messages []providers.Message, elided map[string]int) []providers.Message {
	if len(elided) == 0 {
		return messages
	}
	out := append([]providers.Message(nil), messages...)
	for i := range out {
		if chars, ok := elided[out[i].ToolCallID]; ok && out[i].Role == "tool" {
			out[i].Content = elidedToolResultNote(chars)
		}
	}
	return out
}

// elideTurnToolResultsToFit adds tool results of the active turn to elided,
// oldest first, until the call fits with headroom tokens to spare, or at least
// fits once nothing is left to elide. measure builds the call with a set of
// elisions and says how many tokens it has past the window. It returns the
// grown set, the call built with it and whether that call fits; elided itself
// is not modified.
func elideTurnToolResultsToFit(
	turn []providers.Message,
	elided map[string]int,
	headroom int,
	measure func(map[string]int) ([]providers.Message, int),
) (map[string]int, []providers.Message, bool) {
	next := maps.Clone(elided)
	if next == nil {
		next = make(map[string]int)
	}
	messages, over := measure(next)
	for over+headroom > 0 {
		// Pick by estimated savings, then rebuild once to confirm.
		saved, picked := 0, false
		for _, msg := range turn {
			if !elidableToolResult(msg, next) {
				continue
			}
			chars := utf8.RuneCountInString(msg.Content)
			next[msg.ToolCallID] = chars
			note := msg
			note.Content = elidedToolResultNote(chars)
			saved += EstimateMessageTokens(msg) - EstimateMessageTokens(note)
			picked = true
			if saved >= over+headroom {
				break
			}
		}
		if !picked {
			break
		}
		messages, over = measure(next)
	}
	if over > 0 {
		return elided, messages, false
	}
	return next, messages, true
}

func elidableToolResult(msg providers.Message, elided map[string]int) bool {
	if msg.Role != "tool" || msg.ToolCallID == "" {
		return false
	}
	if _, done := elided[msg.ToolCallID]; done {
		return false
	}
	return utf8.RuneCountInString(msg.Content) >= minElidableToolResultChars
}
