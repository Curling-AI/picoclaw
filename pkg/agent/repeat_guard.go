package agent

import "github.com/sipeed/picoclaw/pkg/tools"

// repeatedSideEffectContent replaces the result of a call that would repeat,
// unchanged, the side effect the model caused last. A model that degenerates
// into re-emitting its previous call (seen in prod: the same project-creating
// MCP call 36 times in one turn, while its own text promised to stop) gets
// this instead of another duplicate.
const repeatedSideEffectContent = "Not executed: this is the same call (same tool, same arguments) " +
	"as the one that just succeeded, and this tool changes things outside the conversation, so " +
	"running it again would repeat that change (another record, message or project). Its result " +
	"is above; continue from it. If you meant a different action, call the tool for that action. " +
	"If a second run is really wanted, ask the user first."

// repeatHints reports whether the tool's server declared how it behaves on a
// repeat (MCP tools do, through annotations or their absence) and, if so,
// whether a repeat is harmless.
func repeatHints(registry *tools.ToolRegistry, toolName string) (declared, harmless bool) {
	tool, ok := registry.GetRegistered(toolName)
	if !ok {
		return false, false
	}
	hinter, ok := tool.(tools.SideEffectHinter)
	if !ok {
		return false, false
	}
	return true, hinter.RepeatIsHarmless()
}

// sideEffectCallKey returns the key of a call whose repetition is not known to
// be harmless, and false for every other call.
//
// Only tools whose effects are declared by their server (MCP) are guarded: they
// act outside the agent, and without annotations the MCP defaults say a call
// may change its environment and repeating it repeats the change. Native tools
// stay out — re-running exec or read_file with the same arguments is routine.
func sideEffectCallKey(registry *tools.ToolRegistry, toolName string, args map[string]any) (string, bool) {
	if declared, harmless := repeatHints(registry, toolName); !declared || harmless {
		return "", false
	}
	return toolName + ":" + normalizeArgs(args), true
}

// repeatsLastSideEffect reports whether key is the call that changed things
// last in this turn, with nothing since that could make running it again mean
// something else.
func (ts *turnState) repeatsLastSideEffect(key string) bool {
	return ts.lastSideEffectCall != "" && ts.lastSideEffectCall == key
}

// recordSucceededCall tracks the last call that could have changed anything.
// A side-effecting call becomes the one a repeat is compared to. Any other call
// that may have changed something (a native exec or write, an MCP call to a
// different action) clears it: write A, write B, write A is a revert, not a
// loop. Tool discovery and calls declared harmless change nothing and keep it.
func (ts *turnState) recordSucceededCall(registry *tools.ToolRegistry, toolName, sideEffectKey string) {
	if sideEffectKey != "" {
		ts.lastSideEffectCall = sideEffectKey
		return
	}
	if tools.IsToolDiscoveryToolName(toolName) {
		return
	}
	if declared, harmless := repeatHints(registry, toolName); declared && harmless {
		return
	}
	ts.lastSideEffectCall = ""
}
