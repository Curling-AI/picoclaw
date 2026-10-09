package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
)

// GLM-family models (glm-5.x, glm-5.x-flash) write tool calls in their own
// markup, which the gateway is supposed to turn into structured tool_calls:
//
//	<tool_call>skip_file_list
//	<arg_key>projectId</arg_key>
//	<arg_value>65025</arg_value>
//	</tool_call>
//
// Two failures reach us in production (2026-10, every provider that serves
// GLM, so it is the model's output and not one parser):
//
//  1. The whole block, or its tail, arrives as plain content. Handled by
//     extractGLMToolCalls and by the truncated-call guards in text_extract.go.
//  2. The gateway does return a structured call, but the model left a closing
//     tag out and the rest of the markup rode along inside one argument:
//     {"action": "replace_text<arg_key>new_text</arg_key><arg_value>…"}.
//     The tool then rejects the call ("is not in enum") and the model repeats
//     the same broken call over and over. Handled by RepairToolCallMarkup.
//
// Values stay strings. The executor already converts a string to the type the
// tool's schema declares; guessing here turned "1.10" into 1.1 and long ids
// into floats.
const (
	glmArgKeyOpen    = "<arg_key>"
	glmArgKeyClose   = "</arg_key>"
	glmArgValueOpen  = "<arg_value>"
	glmArgValueClose = "</arg_value>"
)

var (
	// One block at a time: its body is parsed on its own, so a value missing
	// its closing tag cannot swallow the prose and the call that follow.
	glmToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*([A-Za-z0-9_.\-]+)(.*?)</tool_call>`)
	toolNameRe    = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	glmCallSeq    atomic.Uint64
)

type glmArg struct {
	key   string
	value string
}

// glmBody reports whether the text after a tool name is a GLM argument list:
// nothing, or pairs starting right away.
func glmBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	return trimmed == "" || strings.HasPrefix(trimmed, glmArgKeyOpen)
}

func extractGLMToolCalls(text string) []ToolCall {
	var result []ToolCall
	for _, m := range glmToolCallRe.FindAllStringSubmatch(text, -1) {
		if !glmBody(m[2]) {
			continue
		}
		name := m[1]
		args := map[string]any{}
		for _, a := range parseGLMArgs(m[2]) {
			args[a.key] = a.value
		}

		argsJSON, _ := json.Marshal(args)
		result = append(result, ToolCall{
			// Unique per process: some providers refuse a replayed history
			// with two tool calls sharing an id.
			ID:        fmt.Sprintf("glm_call_%d", glmCallSeq.Add(1)),
			Name:      name,
			Arguments: args,
			Function: &FunctionCall{
				Name:      name,
				Arguments: string(argsJSON),
			},
		})
	}
	return result
}

// stripGLMToolCalls removes the blocks extractGLMToolCalls turns into calls.
func stripGLMToolCalls(text string) string {
	return glmToolCallRe.ReplaceAllStringFunc(text, func(block string) string {
		m := glmToolCallRe.FindStringSubmatch(block)
		if m == nil || !glmBody(m[2]) {
			return block
		}
		return ""
	})
}

// parseGLMArgs reads a run of <arg_key>K</arg_key><arg_value>V</arg_value>
// pairs, tolerating the closing tags the model forgets: a value ends at its
// </arg_value>, at the next <arg_key>, or at the end of the text. Pairs whose
// key is not a plain identifier are skipped; a guess there would hand the
// tool an argument nobody wrote. Linear in the input: each search is bounded
// by the next <arg_key>.
func parseGLMArgs(s string) []glmArg {
	var out []glmArg
	for {
		start := strings.Index(s, glmArgKeyOpen)
		if start < 0 {
			return out
		}
		s = s[start+len(glmArgKeyOpen):]

		keyEnd := strings.Index(s, glmArgKeyClose)
		if keyEnd < 0 {
			return out
		}
		key := strings.TrimSpace(s[:keyEnd])
		s = s[keyEnd+len(glmArgKeyClose):]

		valueStart := strings.Index(s, glmArgValueOpen)
		if valueStart < 0 || strings.TrimSpace(s[:valueStart]) != "" {
			continue
		}
		s = s[valueStart+len(glmArgValueOpen):]

		window := len(s)
		if next := strings.Index(s, glmArgKeyOpen); next >= 0 {
			window = next
		}
		valueEnd := window
		if closing := strings.Index(s[:window], glmArgValueClose); closing >= 0 {
			valueEnd = closing
		}
		value := glmValue(s[:valueEnd])
		s = strings.TrimPrefix(s[valueEnd:], glmArgValueClose)

		if toolNameRe.MatchString(key) {
			out = append(out, glmArg{key: key, value: value})
		}
	}
}

// glmValue drops the one newline the format puts on each side of a value and
// nothing else: the value may be a patch whose first line is indented.
func glmValue(raw string) string {
	return strings.TrimSuffix(strings.TrimPrefix(raw, "\n"), "\n")
}

// RepairToolCallMarkup rewrites, in place, structured tool calls that carry the
// rest of a GLM call inside their name or one string argument, and returns the
// names of the calls it changed.
//
// It only acts when the markup is unambiguous: every argument recovered from
// it must be a parameter the tool declares (tools is what the request offered)
// and must be missing from the call. Anything else stays as the model sent it —
// a write_file whose content quotes this markup, or an exec that greps for it,
// is a normal call.
func RepairToolCallMarkup(calls []ToolCall, tools []ToolDefinition) []string {
	if len(calls) == 0 || len(tools) == 0 {
		return nil
	}
	params := make(map[string]map[string]any, len(tools))
	for _, t := range tools {
		props, _ := t.Function.Parameters["properties"].(map[string]any)
		params[t.Function.Name] = props
	}
	var repaired []string
	for i := range calls {
		if repairToolCall(&calls[i], params) {
			repaired = append(repaired, calls[i].Name)
		}
	}
	return repaired
}

func repairToolCall(tc *ToolCall, params map[string]map[string]any) bool {
	name := tc.Name
	if name == "" && tc.Function != nil {
		name = tc.Function.Name
	}
	args := make(map[string]any, len(tc.Arguments)+2)
	for k, v := range tc.Arguments {
		args[k] = v
	}

	var recovered []glmArg
	if head, rest, ok := strings.Cut(name, glmArgKeyOpen); ok {
		name = strings.TrimSpace(head)
		recovered = append(recovered, parseGLMArgs(glmArgKeyOpen+rest)...)
	}
	props, known := params[name]
	if !known {
		return false
	}

	for key, value := range args {
		str, ok := value.(string)
		// "raw" holds arguments the provider could not decode; the executor
		// explains that case to the model.
		if !ok || key == "raw" || !strings.Contains(str, glmArgKeyOpen) {
			continue
		}
		head, rest, _ := strings.Cut(str, glmArgKeyOpen)
		pairs := parseGLMArgs(glmArgKeyOpen + rest)
		if len(pairs) == 0 {
			continue
		}
		args[key] = strings.TrimSuffix(strings.TrimRight(head, " \t\r\n"), glmArgValueClose)
		recovered = append(recovered, pairs...)
	}

	if len(recovered) == 0 {
		return false
	}
	for _, a := range recovered {
		if _, declared := props[a.key]; !declared {
			return false
		}
		if _, given := tc.Arguments[a.key]; given {
			return false
		}
	}
	for _, a := range recovered {
		if _, taken := args[a.key]; !taken {
			args[a.key] = a.value
		}
	}

	tc.Name = name
	tc.Arguments = args
	argsJSON, _ := json.Marshal(args)
	if tc.Function == nil {
		tc.Function = &FunctionCall{}
	}
	tc.Function.Name = name
	tc.Function.Arguments = string(argsJSON)
	return true
}
