package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// xmlToolCallRe matches <tool_call>...</tool_call> blocks used by qwen and similar models.
var xmlToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ExtractToolCallsFromText parses tool calls embedded in response text.
// It supports four formats:
//  1. JSON wrapper: {"tool_calls": [{"id":"…","type":"function","function":{…}}]}
//  2. XML tag:      <tool_call>{"name":"…","arguments":{…}}</tool_call>
//  3. Pseudo-XML:   <function=NAME><parameter=KEY>VALUE</parameter></function>
//  4. Bare JSON:    {"name":"…","arguments":{…}}
//
// Pseudo-XML is tried BEFORE bare JSON: its argument values are arbitrary text
// (patches, code) that can contain a {"name":…,"arguments":…} object, and the
// bare scanner would happily lift that inner object out as the call.
func ExtractToolCallsFromText(text string) []ToolCall {
	if calls := extractJSONWrapper(text); len(calls) > 0 {
		return calls
	}
	if calls := extractXMLToolCalls(text); len(calls) > 0 {
		return calls
	}
	if calls := extractPseudoXMLToolCalls(text); len(calls) > 0 {
		return calls
	}
	return extractBareToolCalls(text)
}

// StripToolCallsFromText removes tool call JSON/XML from response text.
func StripToolCallsFromText(text string) string {
	text = stripJSONWrapper(text)
	text = xmlToolCallRe.ReplaceAllString(text, "")
	text = pseudoXMLFunctionRe.ReplaceAllString(text, "")
	// The wrapper survives its own body: the function element is what carries
	// the call, and a lone <tool_call>/</tool_call> left behind reads as markup
	// leaking into the answer.
	text = strings.NewReplacer("<tool_call>", "", "</tool_call>", "").Replace(text)
	text = stripBareToolCalls(text)
	return strings.TrimSpace(text)
}

// FindMatchingBrace finds the index after the closing brace matching the
// opening brace at pos. Returns pos if no match is found.
func FindMatchingBrace(text string, pos int) int {
	depth := 0
	for i := pos; i < len(text); i++ {
		if text[i] == '{' {
			depth++
		} else if text[i] == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return pos
}

// --- JSON wrapper format ---

func extractJSONWrapper(text string) []ToolCall {
	start := strings.Index(text, `{"tool_calls"`)
	if start == -1 {
		return nil
	}

	end := FindMatchingBrace(text, start)
	if end == start {
		return nil
	}

	jsonStr := text[start:end]

	var wrapper struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return nil
	}

	var result []ToolCall
	for _, tc := range wrapper.ToolCalls {
		var args map[string]any
		json.Unmarshal([]byte(tc.Function.Arguments), &args)

		result = append(result, ToolCall{
			ID:        tc.ID,
			Type:      tc.Type,
			Name:      tc.Function.Name,
			Arguments: args,
			Function: &FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	return result
}

func stripJSONWrapper(text string) string {
	start := strings.Index(text, `{"tool_calls"`)
	if start == -1 {
		return text
	}

	end := FindMatchingBrace(text, start)
	if end == start {
		return text
	}

	return strings.TrimSpace(text[:start] + text[end:])
}

// --- Bare JSON format ---
// Matches {"name":"…","arguments":{…}} directly in text without any wrapper.

func extractBareToolCalls(text string) []ToolCall {
	var result []ToolCall
	idx := 0
	for idx < len(text) {
		start := strings.Index(text[idx:], "{")
		if start == -1 {
			break
		}
		start += idx

		end := FindMatchingBrace(text, start)
		if end == start {
			idx = start + 1
			continue
		}

		jsonStr := text[start:end]

		var raw struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil || raw.Name == "" || raw.Arguments == nil {
			idx = start + 1
			continue
		}

		argsJSON, _ := json.Marshal(raw.Arguments)
		result = append(result, ToolCall{
			ID:        fmt.Sprintf("bare_call_%d", len(result)),
			Name:      raw.Name,
			Arguments: raw.Arguments,
			Function: &FunctionCall{
				Name:      raw.Name,
				Arguments: string(argsJSON),
			},
		})

		idx = end
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func stripBareToolCalls(text string) string {
	idx := 0
	for idx < len(text) {
		start := strings.Index(text[idx:], "{")
		if start == -1 {
			break
		}
		start += idx

		end := FindMatchingBrace(text, start)
		if end == start {
			idx = start + 1
			continue
		}

		jsonStr := text[start:end]

		var raw struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil || raw.Name == "" || raw.Arguments == nil {
			idx = start + 1
			continue
		}

		text = text[:start] + text[end:]
		// don't advance idx — next JSON object may start at same position
	}

	return text
}

// --- XML <tool_call> format ---

func extractXMLToolCalls(text string) []ToolCall {
	matches := xmlToolCallRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var result []ToolCall
	for i, m := range matches {
		var raw struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
			continue
		}

		argsJSON, _ := json.Marshal(raw.Arguments)

		result = append(result, ToolCall{
			ID:        fmt.Sprintf("xml_call_%d", i),
			Name:      raw.Name,
			Arguments: raw.Arguments,
			Function: &FunctionCall{
				Name:      raw.Name,
				Arguments: string(argsJSON),
			},
		})
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// --- Pseudo-XML <function=…>/<parameter=…> format ---
//
// Emitted by DeepSeek/Qwen-family models when they fall out of the structured
// tool_calls channel:
//
//	<tool_call>
//	<function=mcp_skip_skip_file_patch>
//	<parameter=patch>
//	<<<<<<< SEARCH
//	…
//	</parameter>
//	<parameter=projectId>50503</parameter>
//	</function>
//	</tool_call>
//
// It is NOT the JSON-in-XML shape xmlToolCallRe handles: the name rides on the
// tag itself and each argument is its own element, so the payload is never
// valid JSON. Without this the whole block reaches the user as prose and the
// turn dies (seen in prod on 2026-08-19, `ethos-flash`).
//
// The <tool_call> wrapper is optional on purpose — some models emit only the
// <function=…> element, and a gateway that half-parses the block can eat the
// wrapper before we ever see it.
var (
	pseudoXMLFunctionRe  = regexp.MustCompile(`(?s)<function=([A-Za-z0-9_.\-]+)\s*>(.*?)</function>`)
	pseudoXMLParameterRe = regexp.MustCompile(`(?s)<parameter=([A-Za-z0-9_.\-]+)\s*>(.*?)</parameter>`)
)

func extractPseudoXMLToolCalls(text string) []ToolCall {
	matches := pseudoXMLFunctionRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var result []ToolCall
	for i, m := range matches {
		name := m[1]
		args := map[string]any{}
		for _, p := range pseudoXMLParameterRe.FindAllStringSubmatch(m[2], -1) {
			args[p[1]] = decodePseudoXMLValue(p[2])
		}

		argsJSON, _ := json.Marshal(args)
		result = append(result, ToolCall{
			ID:        fmt.Sprintf("pseudo_xml_call_%d", i),
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

// decodePseudoXMLValue recovers one argument value from its element body.
//
// Two rules, both load-bearing:
//
//  1. Only ONE leading and ONE trailing newline are removed — the format puts
//     the value on its own lines, but the value itself may be a patch whose
//     first line is indented. TrimSpace here would silently corrupt every
//     SEARCH/REPLACE payload, which is the very argument that fails most.
//  2. A value that parses as JSON is decoded, so `50503` reaches a tool
//     expecting a number as a number and not as the string "50503". Prose and
//     code never parse, so they keep their exact bytes.
func decodePseudoXMLValue(raw string) any {
	v := strings.TrimPrefix(raw, "\n")
	v = strings.TrimSuffix(v, "\n")

	var decoded any
	if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &decoded); err == nil {
		return decoded
	}
	return v
}

// truncatedToolCallSuffixes are the closing tags a pseudo-XML tool call ends
// with. Nothing else in prose ends this way.
var truncatedToolCallSuffixes = []string{"</tool_call>", "</function>", "</parameter>"}

// LooksLikeTruncatedToolCall reports whether text is the TAIL of a pseudo-XML
// tool call whose opening tags never reached us — the shape a gateway leaves
// behind when its own tool parser starts matching `<tool_call><function=…>` and
// then bails, forwarding the remainder as ordinary text.
//
// Callers must treat such text as a FAILED tool call, never as an answer: with
// the `<function=NAME>` tag gone the call cannot be reconstructed (there is no
// name), and there is no way to tell where the model's prose ended and the call
// began — so any prefix kept would be a guess.
//
// The test is deliberately anchored at the END of the message. Requiring only
// that a closing tag appear somewhere would fire on an assistant legitimately
// explaining this markup, which happens the moment anyone debugs it.
func LooksLikeTruncatedToolCall(text string) bool {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return false
	}
	for _, suffix := range truncatedToolCallSuffixes {
		if strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}
	return false
}

// toolCallMarkupMarkers are the opening tokens of a tool call written as text.
// Only OPENING ones: they are what a stream can recognize before the block is
// complete, which is the whole point of catching this mid-stream.
var toolCallMarkupMarkers = []string{"<tool_call>", "<function=", "<parameter="}

// LooksLikeToolCallMarkup reports whether accumulated streamed text has turned
// into a tool call written as markup — the signal to stop publishing it live.
//
// Deliberately looser than LooksLikeTruncatedToolCall, which decides what to do
// with a FINISHED message and so can afford to anchor at the end. Here we only
// have a prefix and the cost of being wrong is small: the text still arrives,
// just all at once instead of streaming.
func LooksLikeToolCallMarkup(text string) bool {
	for _, marker := range toolCallMarkupMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
