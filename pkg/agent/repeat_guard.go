package agent

import (
	"github.com/sipeed/picoclaw/pkg/tools"
)

// Repeat guard (seucaranguejo fork). A model that degenerates into re-emitting
// its previous call — seen in prod: the same project-creating MCP call 36
// times in one turn, while its own text promised to stop — must not get the
// side effect again each round. The guard covers MCP tools that don't declare
// a repeat safe: a run of identical calls (same tool and arguments, nothing in
// between that could change what they mean) may run maxUnchangedRuns times;
// past that each call is refused, and refusing the same call again in a later
// round, after the model has read the refusal, ends the turn.
const (
	// One repeat stays allowed: a poll of a status the server didn't declare
	// read-only, or a publish retried after it failed downstream.
	maxUnchangedRuns = 2
)

const repeatedSideEffectContent = "Not executed: this exact call (same tool, same arguments) already " +
	"ran twice in a row in this turn, and this tool doesn't declare itself read-only or safe to " +
	"repeat, so running it again could repeat its effect. Its result is above. Do not send this " +
	"call again in this turn: if you meant a different action, call the tool for that action; " +
	"otherwise finish and report what was done."

const repeatStopSkipContent = "Not executed: the turn ended because a refused call kept being repeated."

// repeatStopSummary is the turn's reply when the guard ends it. toolName is
// the name the user knows the tool by (the server's, for an MCP tool).
func repeatStopSummary(toolName string) string {
	return "I stopped this turn: I kept sending the same " + toolName + " call after it had already " +
		"run twice and been refused. This tool doesn't declare itself read-only or safe to repeat, so " +
		"running it again could have repeated its effect. Nothing else ran after that. Tell me how " +
		"you want to continue."
}

// callEffect is what a call that ran means for the run of identical calls.
type callEffect int

const (
	// effectNone: the call leaves the run going — a read, tool discovery, a
	// message to the user, a note to memory, a file edit in the workspace.
	effectNone callEffect = iota
	// effectChange: the call may have changed what an identical call acts on
	// (an idempotent MCP call, a shell command, another agent), so the same
	// call after it is a new action.
	effectChange
	// effectGuarded: an MCP call that may change things again on every run.
	effectGuarded
)

// nativeToolsThatAct run arbitrary actions — a shell command, another agent —
// that can undo or change what an MCP call acts on. Every other native call
// leaves the run going: a model stuck on a create can interleave a "creating
// the project" message, a reaction or a memory note with each call.
var nativeToolsThatAct = map[string]bool{
	"exec":     true,
	"spawn":    true,
	"subagent": true,
	"delegate": true,
}

func classifyCall(registry *tools.ToolRegistry, toolName string) callEffect {
	if nativeToolsThatAct[toolName] {
		return effectChange
	}
	// GetRegistered, not Get: ExecuteTools revives an expired deferred tool the
	// model calls (EnsureVisible) right before running it.
	tool, ok := registry.GetRegistered(toolName)
	if !ok {
		// Unknown: the registry won't run it.
		return effectNone
	}
	hinter, ok := tool.(tools.SideEffectHinter)
	if !ok {
		return effectNone
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

// displayToolName is the name the user knows a tool by: the server's own name
// for an MCP tool, without the mcp_<server>_ prefix.
func displayToolName(registry *tools.ToolRegistry, toolName string) string {
	if tool, ok := registry.GetRegistered(toolName); ok {
		if named, ok := tool.(tools.ServerNamedTool); ok && named.ServerToolName() != "" {
			return named.ServerToolName()
		}
	}
	return toolName
}

// callKey is the call as the tool will receive it: after the registry's typing
// fixes, so "40235" and 40235 are one action.
func callKey(registry *tools.ToolRegistry, toolName string, args map[string]any) string {
	return toolName + "\x00" + normalizeArgs(registry.CoercedArgs(toolName, args))
}

// repeatGuard tracks the current run of identical guarded calls in a turn.
// Turn goroutine only, like the loop detector.
type repeatGuard struct {
	lastKey  string
	lastRuns int
	// refusedRound is the round in which the current run was first refused,
	// or 0. Iterations start at 1.
	refusedRound int
}

func (g *repeatGuard) refuses(key string) bool {
	return key == g.lastKey && g.lastRuns >= maxUnchangedRuns
}

// recordRefusal reports whether the turn must end: the run was already refused
// in an earlier round, so the model read the refusal and sent the call anyway.
// More copies of the call in the reply that got the first refusal are refused
// without ending the turn — the model hadn't seen the refusal yet.
func (g *repeatGuard) recordRefusal(round int) bool {
	if g.refusedRound == 0 {
		g.refusedRound = round
		return false
	}
	return round > g.refusedRound
}

// recordRun updates the run after a call ran. A call reported as not done
// (the server answered with an error) doesn't count; one whose outcome is
// unknown does (see ToolResult.OutcomeUnknown).
func (g *repeatGuard) recordRun(effect callEffect, key string) {
	switch effect {
	case effectGuarded:
		if key == g.lastKey {
			g.lastRuns++
			return
		}
		*g = repeatGuard{lastKey: key, lastRuns: 1}
	case effectChange:
		*g = repeatGuard{}
	case effectNone:
	}
}

// reset starts over when the user steers the turn: a repeat they ask for is
// their decision, not the model stuck on its last call.
func (g *repeatGuard) reset() {
	*g = repeatGuard{}
}
