package common

import "strings"

// lookaroundTokens are the regex constructs provider-side JSON Schema
// validators reject. RE2 — Go's engine, and the one several gateways use —
// leaves lookaround out by design; that omission is what buys linear time.
var lookaroundTokens = []string{"(?=", "(?!", "(?<=", "(?<!"}

// PatternUnsupported reports whether a JSON Schema `pattern` uses a construct
// the provider rejects.
func PatternUnsupported(pattern string) bool {
	for _, tok := range lookaroundTokens {
		if strings.Contains(pattern, tok) {
			return true
		}
	}
	return false
}

// StripUnsupportedPatterns drops `pattern` keywords the provider rejects,
// leaving the rest of the schema untouched.
//
// Why drop instead of passing through: a single bad pattern fails the WHOLE
// request with 400, not just the tool that carries it. In production this made
// one third-party MCP server mute an entire assistant — every message failed,
// because the tool list travels on every turn:
//
//	Invalid JSON schema: regex lookaround is not supported.
//	Found at $.properties.senderAddress.pattern.
//
// What is lost is a validation HINT: the model no longer knows the exact shape
// expected for that field and may get the value wrong, costing one iteration.
// What is gained is an assistant that still answers. Between a less precise
// tool and a mute agent, the choice is easy.
//
// It deliberately does NOT rewrite the pattern into a lookaround-free
// equivalent: translating regex automatically fails silently, and a WRONG
// pattern is worse than none — it would reject valid values.
func StripUnsupportedPatterns(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if k == "pattern" {
			if s, ok := v.(string); ok && PatternUnsupported(s) {
				continue
			}
		}
		out[k] = stripPatternsValue(v)
	}
	return out
}

func stripPatternsValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return StripUnsupportedPatterns(t)
	case []any:
		items := make([]any, len(t))
		for i, item := range t {
			items[i] = stripPatternsValue(item)
		}
		return items
	default:
		return v
	}
}
