// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"sort"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// parseTurnBoundaries returns the starting index of each Turn in the history.
// A Turn is a complete "user input → LLM iterations → final response" cycle
// (as defined in #1316). Each Turn begins at a user message and extends
// through all subsequent assistant/tool messages until the next user message.
//
// Cutting at a Turn boundary guarantees that no tool-call sequence
// (assistant+ToolCalls → tool results) is split across the cut.
func parseTurnBoundaries(history []providers.Message) []int {
	var starts []int
	for i, msg := range history {
		if msg.Role == "user" {
			starts = append(starts, i)
		}
	}
	return starts
}

// isSafeBoundary reports whether index is a valid Turn boundary — i.e.,
// a position where the kept portion (history[index:]) begins at a user
// message, so no tool-call sequence is torn apart.
func isSafeBoundary(history []providers.Message, index int) bool {
	if index <= 0 || index >= len(history) {
		return true
	}
	return history[index].Role == "user"
}

// findSafeBoundary locates the nearest Turn boundary to targetIndex.
// It prefers the boundary at or before targetIndex (preserving more recent
// context). Falls back to the nearest boundary after targetIndex, and
// returns targetIndex unchanged only when no Turn boundary exists at all.
func findSafeBoundary(history []providers.Message, targetIndex int) int {
	if len(history) == 0 {
		return 0
	}
	if targetIndex <= 0 {
		return 0
	}
	if targetIndex >= len(history) {
		return len(history)
	}

	turns := parseTurnBoundaries(history)
	if len(turns) == 0 {
		return targetIndex
	}

	// Find the last Turn boundary at or before targetIndex.
	// Prefer backward: keeps more recent messages.
	backward := -1
	for _, t := range turns {
		if t <= targetIndex {
			backward = t
		}
	}
	if backward > 0 {
		return backward
	}

	// No valid Turn boundary before target (or only at index 0 which
	// would keep everything). Use the first Turn after targetIndex.
	for _, t := range turns {
		if t > targetIndex {
			return t
		}
	}

	// No Turn boundary after targetIndex either. The only boundary is at
	// index 0, meaning the entire history is a single Turn. Return 0 to
	// signal that safe compression is not possible — callers check for
	// mid <= 0 and skip compression in that case.
	return 0
}

// EstimateMessageTokens estimates the token count for a single message.
// Delegates to the shared tokenizer package for consistency across agent and seahorse.
func EstimateMessageTokens(msg providers.Message) int {
	return tokenizer.EstimateMessageTokens(msg)
}

// EstimateToolDefsTokens estimates the total token cost of tool definitions
// as they appear in the LLM request. Delegates to the shared tokenizer package.
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
	return tokenizer.EstimateToolDefsTokens(defs)
}

// isOverContextBudget checks whether the assembled messages plus tool definitions
// and output reserve would exceed the model's context window. This enables
// proactive compression before calling the LLM, rather than reacting to 400 errors.
func isOverContextBudget(
	contextWindow int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) bool {
	return contextBudgetOverflow(contextWindow, messages, toolDefs, maxTokens) > 0
}

// contextBudgetOverflow is how many tokens the call has past the window; zero
// or less when it fits.
func contextBudgetOverflow(
	contextWindow int,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) int {
	return estimateCallTokens(messages, toolDefs, maxTokens) - contextWindow
}

// estimateCallTokens is the estimated size of a call: its messages, its tool
// definitions and the output reserve.
func estimateCallTokens(messages []providers.Message, toolDefs []providers.ToolDefinition, maxTokens int) int {
	tokens := EstimateToolDefsTokens(toolDefs) + maxTokens
	for _, m := range messages {
		tokens += EstimateMessageTokens(m)
	}
	return tokens
}

// compactionCanHelp reports whether dropping conversation history could
// possibly bring the request under the context budget. build must be the same
// prompt builder used for trimming; invoked with nil history it yields only
// the irreducible parts of the request — system prompt, summary and the
// active turn. When even that plus tool definitions and the max_tokens output
// reserve exceeds the window, the overflow is structural (misconfigured
// context_window/max_tokens, or an oversized tool set / system prompt):
// compacting history cannot fix it, and persisting a history cut would
// destroy conversation data for nothing.
func compactionCanHelp(
	build func([]providers.Message) []providers.Message,
	contextWindow int,
	toolDefs []providers.ToolDefinition,
	maxTokens int,
) bool {
	return !isOverContextBudget(contextWindow, build(nil), toolDefs, maxTokens)
}

// compactionGuard is what a context retry checks before the destructive
// compaction. stable is the history before the active turn, build the guard's
// prompt builder (see compactionCanHelp). (seucaranguejo fork)
type compactionGuard struct {
	// historyRead says the current history was read. Without it the guard
	// would measure the copy taken when the turn started, which lacks the
	// turn's tool results, and let a useless compaction through.
	historyRead bool
	stable      []providers.Message
	build       func([]providers.Message) []providers.Message
	budget      callBudget
}

// skipReason says why the compaction must not run, or "" when it may.
func (g compactionGuard) skipReason() string {
	switch {
	case !g.historyRead:
		return "could not read the current history"
	case len(g.stable) == 0:
		// A compaction could only cut into the active turn: the legacy
		// manager then keeps just its user message, and the model runs its
		// tools again.
		return "no history before the active turn"
	case !compactionCanHelp(g.build, g.budget.window, g.budget.toolDefs, g.budget.maxTokens):
		return "the active turn alone exceeds the budget"
	}
	return ""
}

// trimHistory drops the oldest complete turns of history until the prompt
// rebuilt from the rest fits the budget. Cutting at turn boundaries keeps
// tool-call sequences intact. It returns the history kept, the call built
// from it and whether that call fits.
//
// A positive historyLimit is a ceiling on what a needed cut keeps: at most
// that many estimated tokens of history, so a session that could not compact
// runs where compaction would have kept it, not at the window's edge on every
// call. When even the newest turn is past the limit, the cut keeps that turn
// alone.
func trimHistory(
	history []providers.Message,
	build func([]providers.Message) []providers.Message,
	budget callBudget,
	historyLimit int,
) ([]providers.Message, []providers.Message, bool) {
	messages := build(history)
	if budget.overflow(messages) <= 0 {
		return history, messages, true
	}
	if len(history) == 0 {
		return nil, messages, false
	}

	// starts are the cuts to try, oldest first: every turn after the first,
	// then no history at all. A lone turn behind stray messages is a cut too.
	turns := parseTurnBoundaries(history)
	starts := turns
	if len(turns) >= 2 || (len(turns) == 1 && turns[0] == 0) {
		starts = turns[1:]
	}
	starts = append(starts, len(history))

	first := 0
	if historyLimit > 0 {
		kept := make([]int, len(history)+1) // kept[i]: estimated tokens of history[i:]
		for i := len(history) - 1; i >= 0; i-- {
			kept[i] = kept[i+1] + EstimateMessageTokens(history[i])
		}
		first = sort.Search(len(starts)-1, func(i int) bool { return kept[starts[i]] <= historyLimit })
		if first == len(starts)-1 {
			first = max(len(starts)-2, 0)
		}
	}

	// A copy, nil when nothing is left: build may keep what it is given.
	suffix := func(start int) []providers.Message {
		return append([]providers.Message(nil), history[start:]...)
	}
	var call []providers.Message // the call built at the earliest cut known to fit
	fits := func(i int) bool {
		messages = build(suffix(starts[i]))
		if budget.overflow(messages) > 0 {
			return false
		}
		call = messages
		return true
	}
	// A shorter history never builds a larger call, so the first cut that fits
	// can be searched for. Each probe rebuilds the prompt, and that cut is
	// usually the first one tried or close to it: gallop, then bisect the gap.
	lo, hi := first, first
	for step := 1; hi < len(starts) && !fits(hi); step *= 2 {
		lo, hi = hi+1, hi+step
	}
	hi = min(hi, len(starts))
	cut := lo + sort.Search(hi-lo, func(i int) bool { return fits(lo + i) })
	if cut == len(starts) {
		return nil, messages, false
	}
	return suffix(starts[cut]), call, true
}
