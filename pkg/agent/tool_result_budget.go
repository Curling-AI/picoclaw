// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
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
//   - fitTurnByElision, on a context retry where even the turn alone does not
//     fit, stands a short note in for the earliest results of the turn. Only
//     in what is sent: the session keeps every result.

// toolOutputDir sits under artifacts/ to share its persistent mount; the
// Artifacts screen lists only the top level, so these files stay out of the
// user's deliverables (as artifacts/mcp does).
const toolOutputDir = "tool-output"

const (
	// toolOutputHeadShare is the part of the kept characters taken from the
	// start of the output; the rest comes from its end, where commands print
	// errors and summaries.
	toolOutputHeadShare = 0.75
	// maxSavedToolOutputs bounds the directory on the persistent volume: a
	// cron that snapshots a large page would otherwise add a file per run.
	maxSavedToolOutputs = 32
	// maxToolOutputNameLen keeps the file name far from the filesystem's
	// limit; MCP tool names come from the servers.
	maxToolOutputNameLen = 64
)

// The notice that opens a trimmed result. An elision note repeats where the
// full output went, so the model reads the file instead of running the tool
// again.
const (
	truncatedToolResultPrefix = "[Tool output truncated: "
	savedToolOutputPrefix     = "The full output is saved at "
	savedToolOutputSuffix     = ": read it in parts with read_file"
)

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
	return truncateToolResult(content, total, limit, path)
}

// saveToolOutput stores content under a name made from its hash: the same
// output is stored once, and its trimmed form reads the same each time, which
// the loop detector's no-progress check relies on.
func saveToolOutput(workspace, toolName, content string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("no workspace to save the full output")
	}
	dir := filepath.Join(workspace, "artifacts", toolOutputDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, toolOutputFileStem(toolName)+"_"+hex.EncodeToString(sum[:8])+".txt")
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now) // a repeat counts as recent for pruning
		return path, nil
	}
	if err := writeFileAtomically(dir, path, content); err != nil {
		return "", err
	}
	pruneSavedToolOutputs(dir, maxSavedToolOutputs)
	return path, nil
}

func toolOutputFileStem(toolName string) string {
	stem := utils.SanitizeFilename(toolName)
	if len(stem) > maxToolOutputNameLen {
		stem = stem[:maxToolOutputNameLen]
	}
	return stem
}

// writeFileAtomically writes through a temporary file, so a parallel call
// with the same output never sees it half written.
func writeFileAtomically(dir, path, content string) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	tmp := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

// pruneSavedToolOutputs keeps the keep most recent outputs. A failure leaves
// extra files behind and is not worth failing the tool result over.
func pruneSavedToolOutputs(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type saved struct {
		path    string
		modTime time.Time
	}
	var files []saved
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, saved{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	if len(files) <= keep {
		return
	}
	slices.SortFunc(files, func(a, b saved) int { return b.modTime.Compare(a.modTime) })
	for _, f := range files[keep:] {
		_ = os.Remove(f.path)
	}
}

// truncateToolResult keeps the start and the end of content, total runes long,
// within limit runes, notes included. savedPath is where the full output went,
// or "". It slices by byte offsets found rune by rune: a []rune of an 18MB
// output would cost four times its size.
func truncateToolResult(content string, total, limit int, savedPath string) string {
	notice := truncatedToolResultNotice(total, savedPath)
	// The omitted count is below total, so this marker is never shorter than
	// the real one.
	markerRunes := utf8.RuneCountInString(omittedToolOutputMarker(total))
	keep := max(limit-utf8.RuneCountInString(notice)-markerRunes, 0)
	head := int(float64(keep) * toolOutputHeadShare)
	tail := keep - head
	headEnd := 0
	for range head {
		_, size := utf8.DecodeRuneInString(content[headEnd:])
		headEnd += size
	}
	tailStart := len(content)
	for range tail {
		_, size := utf8.DecodeLastRuneInString(content[:tailStart])
		tailStart -= size
	}
	return notice + content[:headEnd] + omittedToolOutputMarker(total-keep) + content[tailStart:]
}

func truncatedToolResultNotice(total int, savedPath string) string {
	where := "The full output could not be saved."
	if savedPath != "" {
		where = savedToolOutputPrefix + savedPath + savedToolOutputSuffix + " (offset/length) or search it with grep."
	}
	return fmt.Sprintf("%s%d characters, too long for the conversation; only its start and end are shown. %s "+
		"Narrow the request (filters, selectors, limits) instead of repeating it.]\n\n",
		truncatedToolResultPrefix, total, where)
}

func omittedToolOutputMarker(omitted int) string {
	return fmt.Sprintf("\n\n[… %d characters omitted …]\n\n", omitted)
}

// savedToolOutputPath returns where a result trimmed by boundToolResultForLLM
// saved its full output, or "".
func savedToolOutputPath(content string) string {
	if !strings.HasPrefix(content, truncatedToolResultPrefix) {
		return ""
	}
	notice, _, _ := strings.Cut(content, "]\n\n")
	_, rest, found := strings.Cut(notice, savedToolOutputPrefix)
	if !found {
		return ""
	}
	path, _, found := strings.Cut(rest, savedToolOutputSuffix)
	if !found {
		return ""
	}
	return path
}

// elidedToolResult is a tool result of the active turn a context retry
// replaced with a note. The id guards against the turn's messages moving:
// text-lifted tool calls reuse ids like "xml_call_0" in every response, so the
// id alone cannot tell results apart.
type elidedToolResult struct {
	toolCallID string
	chars      int
	savedPath  string
}

// turnElisions maps offsets from the start of the active turn to the results
// replaced there. The turn only grows by appending, so offsets stay put.
type turnElisions map[int]elidedToolResult

func elidedToolResultNote(e elidedToolResult) string {
	next := "Run the tool again with a narrower request if you still need it."
	if e.savedPath != "" {
		next = "Its full output is saved at " + e.savedPath + ": read it from there instead of running the tool again."
	}
	return fmt.Sprintf("%s to keep this turn within the context window (%d characters). %s]",
		elidedToolResultMarker, e.chars, next)
}

// withElidedToolResults returns messages, whose active turn starts at
// turnStart, with the elided results replaced by their notes. messages is the
// turn's own history and is never written; without elisions it is returned as
// is.
func withElidedToolResults(messages []providers.Message, turnStart int, elided turnElisions) []providers.Message {
	if len(elided) == 0 || turnStart < 0 {
		return messages
	}
	var out []providers.Message
	for offset, e := range elided {
		i := turnStart + offset
		if i >= len(messages) || messages[i].Role != "tool" || messages[i].ToolCallID != e.toolCallID {
			continue
		}
		if out == nil {
			out = append([]providers.Message(nil), messages...)
		}
		out[i].Content = elidedToolResultNote(e)
	}
	if out == nil {
		return messages
	}
	return out
}

// callBudget is the room a call must fit in.
type callBudget struct {
	window    int
	toolDefs  []providers.ToolDefinition
	maxTokens int
}

func (b callBudget) overflow(messages []providers.Message) int {
	return contextBudgetOverflow(b.window, messages, b.toolDefs, b.maxTokens)
}

// fitTurnByElision is the context retry's last resort when even the active
// turn alone is past the window. It elides tool results of the turn, keeping
// headroom for the turn's next calls, then puts back as much of the earlier
// history as still fits under that headroom. build makes the call from a slice
// of the earlier history and a set of elisions.
func fitTurnByElision(
	stable, turn []providers.Message,
	elided turnElisions,
	budget callBudget,
	build func([]providers.Message, turnElisions) []providers.Message,
) (history, call []providers.Message, next turnElisions, ok bool) {
	headroom := budget.window / elisionHeadroomDivisor
	next, call, ok = elideTurnToolResultsToFit(turn, elided, headroom, func(e turnElisions) ([]providers.Message, int) {
		msgs := build(nil, e)
		return msgs, budget.overflow(msgs)
	})
	if !ok {
		return nil, call, elided, false
	}
	kept, keptCall, fits := trimHistoryToFitContextWindow(
		stable,
		func(h []providers.Message) []providers.Message { return build(h, next) },
		budget.window-headroom,
		budget.toolDefs,
		budget.maxTokens,
	)
	if fits && len(kept) > 0 {
		return kept, keptCall, next, true
	}
	return nil, call, next, true
}

// elideTurnToolResultsToFit adds tool results of the active turn to elided
// until measure says the call fits. Earlier results go first, oldest first,
// until headroom tokens are spare; the newest batch, which the model has not
// read yet, goes only when the call fits no other way. It returns the grown
// set, the call built with it and whether that call fits; elided itself is
// not modified.
func elideTurnToolResultsToFit(
	turn []providers.Message,
	elided turnElisions,
	headroom int,
	measure func(turnElisions) ([]providers.Message, int),
) (turnElisions, []providers.Message, bool) {
	next := maps.Clone(elided)
	if next == nil {
		next = make(turnElisions)
	}
	newest := newestToolBatchStart(turn)
	messages, over := measure(next)
	for _, phase := range []struct{ from, to, headroom int }{{0, newest, headroom}, {newest, len(turn), 0}} {
		for over+phase.headroom > 0 && pickElisions(turn, phase.from, phase.to, next, over+phase.headroom) {
			messages, over = measure(next)
		}
	}
	if over > 0 {
		return elided, messages, false
	}
	return next, messages, true
}

// pickElisions adds results of turn[from:to] to next, oldest first, until
// their estimated savings reach goal tokens. It reports whether it added any.
func pickElisions(turn []providers.Message, from, to int, next turnElisions, goal int) bool {
	saved, picked := 0, false
	for offset := from; offset < to && saved < goal; offset++ {
		msg := turn[offset]
		if !elidableToolResult(msg, offset, next) {
			continue
		}
		e := elidedToolResult{
			toolCallID: msg.ToolCallID,
			chars:      utf8.RuneCountInString(msg.Content),
			savedPath:  savedToolOutputPath(msg.Content),
		}
		next[offset] = e
		note := msg
		note.Content = elidedToolResultNote(e)
		saved += EstimateMessageTokens(msg) - EstimateMessageTokens(note)
		picked = true
	}
	return picked
}

func elidableToolResult(msg providers.Message, offset int, elided turnElisions) bool {
	if msg.Role != "tool" {
		return false
	}
	if _, done := elided[offset]; done {
		return false
	}
	return utf8.RuneCountInString(msg.Content) >= minElidableToolResultChars
}

// newestToolBatchStart is where the results of the turn's last tool calls
// begin: right after its last assistant message.
func newestToolBatchStart(turn []providers.Message) int {
	for i := len(turn) - 1; i >= 0; i-- {
		if turn[i].Role == "assistant" {
			return i + 1
		}
	}
	return 0
}
