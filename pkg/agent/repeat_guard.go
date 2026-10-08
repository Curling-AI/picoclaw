package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// Repeat guard (seucaranguejo fork). A model that degenerates into re-emitting
// its previous call — seen in prod: the same project-creating MCP call 36
// times in one turn, while its own text promised to stop — must not get the
// side effect again each round. The guard covers MCP tools that may change
// things on every call: a run of identical calls (same tool and arguments,
// nothing in between that could change what they mean) may run
// maxUnchangedRuns times; past that each call is refused, and after
// maxRepeatRefusals refusals the turn ends.
const (
	// One repeat stays allowed: a poll of a status the server didn't declare
	// read-only, or a publish retried after it failed downstream.
	maxUnchangedRuns = 2
	// A model that sends a refused call again is stuck; ending the turn beats
	// paying LLM rounds until max_tool_iterations.
	maxRepeatRefusals = 2
)

const repeatedSideEffectContent = "Not executed: this exact call (same tool, same arguments) already " +
	"ran twice in a row in this turn, and this tool changes things outside the conversation, so " +
	"running it again would repeat that change (another record, message or project). Its result is " +
	"above. Do not send this call again in this turn: if you meant a different action, call the tool " +
	"for that action; otherwise finish and report what was done."

const repeatStopSkipContent = "Not executed: the turn ended because a refused call kept being repeated."

// repeatStopSummary is the turn's reply when the guard ends it.
func repeatStopSummary(toolName string) string {
	return "I stopped this turn: I kept sending the same " + toolName + " call after it had already " +
		"run and been refused, and running it again would have repeated its effect (another record, " +
		"message or project). Nothing else ran after that. Tell me how you want to continue."
}

// callEffect is what a successful call means for the run of identical calls.
type callEffect int

const (
	// effectNone: reads and tool discovery change nothing; the run goes on.
	effectNone callEffect = iota
	// effectChange: the call may have changed something (a native exec or
	// write, an idempotent MCP call), so the same call after it is a new action.
	effectChange
	// effectGuarded: an MCP call that may change things again on every run.
	effectGuarded
)

// readOnlyNativeTool reports native tools that only read, besides tool search.
func readOnlyNativeTool(toolName string) bool {
	return tools.IsToolDiscoveryToolName(toolName) ||
		nonMutatingTools[toolName] ||
		toolName == tools.FindInstalledSkillsToolName ||
		toolName == "spawn_status"
}

func classifyCall(registry *tools.ToolRegistry, toolName string) callEffect {
	if readOnlyNativeTool(toolName) {
		return effectNone
	}
	tool, ok := registry.GetRegistered(toolName)
	if !ok {
		return effectChange
	}
	hinter, ok := tool.(tools.SideEffectHinter)
	if !ok {
		return effectChange
	}
	switch hinter.RepeatSafety() {
	case tools.RepeatReadOnly:
		return effectNone
	case tools.RepeatIdempotent:
		return effectChange
	default:
		return effectGuarded
	}
}

// callKey hashes the call as the tool will receive it: after the registry's
// typing fixes, so "40235" and 40235 are one action, and hashed because
// arguments can be large (a whole file to write).
func callKey(registry *tools.ToolRegistry, toolName string, args map[string]any) string {
	normalized := normalizeArgs(registry.CoercedArgs(toolName, args))
	sum := sha256.Sum256([]byte(toolName + "\x00" + normalized))
	return hex.EncodeToString(sum[:])
}

// repeatGuard tracks the current run of identical guarded calls in a turn.
// Turn goroutine only, like the loop detector.
type repeatGuard struct {
	lastKey  string
	lastRuns int
	refusals int
}

func (g *repeatGuard) refuses(key string) bool {
	return key == g.lastKey && g.lastRuns >= maxUnchangedRuns
}

// recordRefusal counts a refusal and reports whether the turn must end.
func (g *repeatGuard) recordRefusal() bool {
	g.refusals++
	return g.refusals >= maxRepeatRefusals
}

// recordSuccess updates the run after a call succeeded. Failures don't count:
// the server reported the call as not done.
func (g *repeatGuard) recordSuccess(effect callEffect, key string) {
	switch effect {
	case effectGuarded:
		if key == g.lastKey {
			g.lastRuns++
			return
		}
		g.lastKey, g.lastRuns = key, 1
	case effectChange:
		g.lastKey, g.lastRuns = "", 0
	case effectNone:
	}
}

// reset starts over when the user steers the turn: a repeat they ask for is
// their decision, not the model stuck on its last call.
func (g *repeatGuard) reset() {
	*g = repeatGuard{}
}
