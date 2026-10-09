package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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
//     The tool then rejects the call ("is not in enum", "unexpected property")
//     and the model repeats the same broken call over and over. Handled by
//     RepairToolCallMarkup.
const (
	glmArgKeyOpen    = "<arg_key>"
	glmArgKeyClose   = "</arg_key>"
	glmArgValueOpen  = "<arg_value>"
	glmArgValueClose = "</arg_value>"
)

var (
	glmToolCallRe  = regexp.MustCompile(`(?s)<tool_call>\s*([A-Za-z0-9_.\-]+)\s*((?:<arg_key>.*?</arg_key>\s*<arg_value>.*?</arg_value>\s*)*)</tool_call>`)
	toolNameRe     = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	glmMarkupTags  = []string{glmArgKeyOpen, glmArgKeyClose, glmArgValueOpen, glmArgValueClose}
	glmTagReplacer = strings.NewReplacer(glmArgKeyOpen, "", glmArgKeyClose, "", glmArgValueOpen, "", glmArgValueClose, "")
)

type glmArg struct {
	key   string
	value string
}

func extractGLMToolCalls(text string) []ToolCall {
	matches := glmToolCallRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	result := make([]ToolCall, 0, len(matches))
	for i, m := range matches {
		name := m[1]
		args := map[string]any{}
		for _, a := range parseGLMArgs(m[2]) {
			args[a.key] = decodePseudoXMLValue(a.value)
		}

		argsJSON, _ := json.Marshal(args)
		result = append(result, ToolCall{
			ID:        fmt.Sprintf("glm_call_%d", i),
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

// parseGLMArgs reads a run of <arg_key>K</arg_key><arg_value>V</arg_value>
// pairs, tolerating the closing tags the model forgets: a value ends at its
// </arg_value>, at the next <arg_key>, or at the end of the text. Pairs whose
// key is not a plain identifier are skipped; a guess there would hand the
// tool an argument nobody wrote.
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

		valueEnd := len(s)
		if i := strings.Index(s, glmArgValueClose); i >= 0 {
			valueEnd = i
		}
		if i := strings.Index(s, glmArgKeyOpen); i >= 0 && i < valueEnd {
			valueEnd = i
		}
		value := s[:valueEnd]
		s = strings.TrimPrefix(s[valueEnd:], glmArgValueClose)

		if toolNameRe.MatchString(key) {
			out = append(out, glmArg{key: key, value: value})
		}
	}
}

func containsGLMMarkup(s string) bool {
	for _, tag := range glmMarkupTags {
		if strings.Contains(s, tag) {
			return true
		}
	}
	return false
}

// RepairToolCallMarkup rewrites, in place, structured tool calls that carry
// GLM argument markup inside their name, argument names or string argument
// values, and reports how many calls it changed. An argument the model wrote
// on its own is never overwritten by one recovered from markup.
func RepairToolCallMarkup(calls []ToolCall) int {
	repaired := 0
	for i := range calls {
		if repairToolCall(&calls[i]) {
			repaired++
		}
	}
	return repaired
}

func repairToolCall(tc *ToolCall) bool {
	changed := false
	recovered := []glmArg{}

	name := tc.Name
	if name == "" && tc.Function != nil {
		name = tc.Function.Name
	}
	if head, rest, ok := strings.Cut(name, glmArgKeyOpen); ok && toolNameRe.MatchString(strings.TrimSpace(head)) {
		name = strings.TrimSpace(head)
		recovered = append(recovered, parseGLMArgs(glmArgKeyOpen+rest)...)
		changed = true
	}

	args := tc.Arguments
	if args == nil {
		args = map[string]any{}
	}
	fixed := make(map[string]any, len(args))
	for key, value := range args {
		if containsGLMMarkup(key) {
			// "…</arg_value><arg_key>command": the text before the last
			// <arg_key> belongs to a value whose key was lost; the name after
			// it is the argument this value was meant for.
			idx := strings.LastIndex(key, glmArgKeyOpen)
			if idx < 0 {
				fixed[key] = value
				continue
			}
			realKey := strings.TrimSpace(glmTagReplacer.Replace(key[idx+len(glmArgKeyOpen):]))
			if !toolNameRe.MatchString(realKey) {
				fixed[key] = value
				continue
			}
			if _, taken := args[realKey]; !taken {
				fixed[realKey] = value
			}
			changed = true
			continue
		}

		str, ok := value.(string)
		if !ok || !strings.Contains(str, glmArgKeyOpen) {
			fixed[key] = value
			continue
		}
		head, rest, _ := strings.Cut(str, glmArgKeyOpen)
		fixed[key] = decodePseudoXMLValue(strings.TrimSuffix(strings.TrimRight(head, " \t\r\n"), glmArgValueClose))
		recovered = append(recovered, parseGLMArgs(glmArgKeyOpen+rest)...)
		changed = true
	}

	if !changed {
		return false
	}
	for _, a := range recovered {
		if a.value == "" {
			continue
		}
		if _, taken := fixed[a.key]; taken {
			continue
		}
		fixed[a.key] = decodePseudoXMLValue(a.value)
	}

	tc.Name = name
	tc.Arguments = fixed
	argsJSON, _ := json.Marshal(fixed)
	if tc.Function == nil {
		tc.Function = &FunctionCall{}
	}
	tc.Function.Name = name
	tc.Function.Arguments = string(argsJSON)
	return true
}
