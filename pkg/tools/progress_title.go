package tools

import (
	"maps"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The progress title: an optional argument the agent loop adds to every
// tool's schema in a turn whose channel renders tool cards
// (channels.ToolEventsCapable). The model writes there a short title for the
// person; the loop takes it out of the call before the tool sees it.
// (seucaranguejo fork — MST-957)
const (
	ProgressTitleParam    = "progress_title"
	ProgressTitleMaxRunes = 80
)

const progressTitleDescription = "Optional. A short title for the person following along, " +
	"in the language they write in, saying what this call does " +
	"(e.g. \"Searching flights to Lisbon\"). At most 80 characters. " +
	"Shown to the person only; the tool does not receive it."

// WithProgressTitle returns defs with ProgressTitleParam added to each tool's
// parameters. The maps it changes are copies: a tool's own schema, which the
// registry and other turns share, stays as it is. A tool that already declares
// a property of that name, or whose parameters are not an object schema, is
// left untouched.
func WithProgressTitle(defs []providers.ToolDefinition) []providers.ToolDefinition {
	if len(defs) == 0 {
		return defs
	}
	out := make([]providers.ToolDefinition, len(defs))
	for i, def := range defs {
		out[i] = def
		if params, ok := withProgressTitleParam(def.Function.Parameters); ok {
			out[i].Function.Parameters = params
		}
	}
	return out
}

func withProgressTitleParam(params map[string]any) (map[string]any, bool) {
	if DeclaresProgressTitle(params) {
		return nil, false
	}
	if schemaType, ok := params["type"]; ok && schemaType != "object" {
		return nil, false
	}
	var properties map[string]any
	switch raw := params["properties"].(type) {
	case nil:
		properties = map[string]any{}
	case map[string]any:
		properties = maps.Clone(raw)
	default:
		return nil, false
	}
	properties[ProgressTitleParam] = map[string]any{
		"type":        "string",
		"maxLength":   ProgressTitleMaxRunes,
		"description": progressTitleDescription,
	}
	next := maps.Clone(params)
	if next == nil {
		next = map[string]any{"type": "object"}
	}
	next["properties"] = properties
	return next, true
}

// DeclaresProgressTitle reports whether a tool's own parameters have a
// property named ProgressTitleParam, which then stays the tool's argument.
func DeclaresProgressTitle(params map[string]any) bool {
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, declared := properties[ProgressTitleParam]
	return declared
}

// TakeProgressTitle returns args without ProgressTitleParam, and the title the
// model wrote there, trimmed and cut to ProgressTitleMaxRunes. taken is false
// when args carry no such argument; args itself never changes.
func TakeProgressTitle(args map[string]any) (title string, rest map[string]any, taken bool) {
	raw, ok := args[ProgressTitleParam]
	if !ok {
		return "", args, false
	}
	rest = maps.Clone(args)
	delete(rest, ProgressTitleParam)
	if text, isString := raw.(string); isString {
		runes := []rune(strings.TrimSpace(text))
		title = strings.TrimSpace(string(runes[:min(len(runes), ProgressTitleMaxRunes)]))
	}
	return title, rest, true
}
