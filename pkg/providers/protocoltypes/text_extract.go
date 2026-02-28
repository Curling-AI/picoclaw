package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// xmlToolCallRe matches <tool_call>...</tool_call> blocks used by qwen and similar models.
var xmlToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ExtractToolCallsFromText parses tool call JSON embedded in response text.
// It supports three formats:
//  1. JSON wrapper: {"tool_calls": [{"id":"…","type":"function","function":{…}}]}
//  2. XML tag:      <tool_call>{"name":"…","arguments":{…}}</tool_call>
//  3. Bare JSON:    {"name":"…","arguments":{…}}
func ExtractToolCallsFromText(text string) []ToolCall {
	if calls := extractJSONWrapper(text); len(calls) > 0 {
		return calls
	}
	if calls := extractXMLToolCalls(text); len(calls) > 0 {
		return calls
	}
	return extractBareToolCalls(text)
}

// StripToolCallsFromText removes tool call JSON/XML from response text.
func StripToolCallsFromText(text string) string {
	text = stripJSONWrapper(text)
	text = xmlToolCallRe.ReplaceAllString(text, "")
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
