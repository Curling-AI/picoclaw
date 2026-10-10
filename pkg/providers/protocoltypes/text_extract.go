package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// xmlToolCallRe matches <tool_call>...</tool_call> blocks used by qwen and similar models.
var xmlToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ExtractToolCallsFromText parses tool calls embedded in response text.
// It supports five formats:
//  1. GLM:          <tool_call>NAME<arg_key>KEY</arg_key><arg_value>VALUE</arg_value></tool_call>
//  2. JSON wrapper: {"tool_calls": [{"id":"…","type":"function","function":{…}}]}
//  3. XML tag:      <tool_call>{"name":"…","arguments":{…}}</tool_call>
//  4. Pseudo-XML:   <function=NAME><parameter=KEY>VALUE</parameter></function>
//  5. Bare JSON:    {"name":"…","arguments":{…}}
//
// GLM goes first, and the other formats are only looked for outside its
// blocks (an unclosed one runs to the end of the text): a GLM value is
// arbitrary text (a file, a command) and may hold a call in any of the JSON
// shapes, which is then something the model wrote down, not a call it made.
// For the same reason the markup formats are tried before bare JSON. Only
// the calls that end the text count (see LiftToolCallsFromText).
func ExtractToolCallsFromText(text string) []ToolCall {
	calls, _ := LiftToolCallsFromText(text)
	return calls
}

// StripToolCallsFromText removes from text the calls ExtractToolCallsFromText
// finds in it, and only those: markup the model merely quoted stays.
func StripToolCallsFromText(text string) string {
	_, rest := LiftToolCallsFromText(text)
	return rest
}

// LiftToolCallsFromText returns the tool calls that end text and the text
// left once their spans are removed. A call only counts as the last thing
// written, outside a ``` fence: a model making a call stops there, and one
// earlier in the text, or fenced, is a call it quotes (a page it summarizes,
// an example it gives) or, in its reasoning, one it weighs.
func LiftToolCallsFromText(text string) ([]ToolCall, string) {
	calls, spans := liftToolCalls(text)
	if !endsText(text, spans) {
		return nil, text
	}
	return calls, strings.TrimSpace(removeSpans(text, spans))
}

// endsText reports whether spans, in order, are the last thing in text with
// only whitespace between them, and the first one is outside a ``` fence.
func endsText(text string, spans []span) bool {
	if len(spans) == 0 || insideCodeFence(text[:spans[0].start]) {
		return false
	}
	for i, sp := range spans {
		next := len(text)
		if i+1 < len(spans) {
			next = spans[i+1].start
		}
		if strings.TrimSpace(text[sp.end:next]) != "" {
			return false
		}
	}
	return true
}

func liftToolCalls(text string) ([]ToolCall, []span) {
	if blocks := glmBlocks(text); len(blocks) > 0 {
		if calls, spans := liftGLMToolCalls(text, blocks); len(calls) > 0 {
			return calls, spans
		}
		masked := []byte(text)
		for _, b := range blocks {
			for i := b.start; i < b.end; i++ {
				masked[i] = ' '
			}
		}
		text = string(masked)
	}
	for _, extract := range []func(string) ([]ToolCall, []span){
		extractJSONWrapper,
		extractXMLToolCalls,
		extractPseudoXMLToolCalls,
		extractBareToolCalls,
	} {
		if calls, spans := extract(text); len(calls) > 0 {
			return calls, spans
		}
	}
	return nil, nil
}

// removeSpans returns text without the given spans, which are in order and
// do not overlap.
func removeSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(text[prev:sp.start])
		prev = sp.end
	}
	b.WriteString(text[prev:])
	return b.String()
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

func extractJSONWrapper(text string) ([]ToolCall, []span) {
	start := strings.Index(text, `{"tool_calls"`)
	if start == -1 {
		return nil, nil
	}

	end := FindMatchingBrace(text, start)
	if end == start {
		return nil, nil
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
		return nil, nil
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
	if len(result) == 0 {
		return nil, nil
	}
	return result, []span{{start, end}}
}

// --- Bare JSON format ---
// Matches {"name":"…","arguments":{…}} directly in text without any wrapper.

func extractBareToolCalls(text string) ([]ToolCall, []span) {
	var result []ToolCall
	var spans []span
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
		spans = append(spans, span{start, end})

		idx = end
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, spans
}

// --- XML <tool_call> format ---

func extractXMLToolCalls(text string) ([]ToolCall, []span) {
	matches := xmlToolCallRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	var result []ToolCall
	var spans []span
	for i, m := range matches {
		var raw struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(text[m[2]:m[3]]), &raw); err != nil {
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
		spans = append(spans, span{m[0], m[1]})
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, spans
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

func extractPseudoXMLToolCalls(text string) ([]ToolCall, []span) {
	matches := pseudoXMLFunctionRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	result := make([]ToolCall, 0, len(matches))
	spans := make([]span, 0, len(matches))
	for i, m := range matches {
		name := text[m[2]:m[3]]
		args := map[string]any{}
		for _, p := range pseudoXMLParameterRe.FindAllStringSubmatch(text[m[4]:m[5]], -1) {
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
		spans = append(spans, pseudoXMLSpan(text, m[0], m[1]))
	}

	return result, spans
}

// pseudoXMLSpan widens a function element to the <tool_call> wrapper around
// it, whichever of its two tags survived: left behind, a lone tag reads as
// markup leaking into the answer.
func pseudoXMLSpan(text string, start, end int) span {
	if before := strings.TrimRightFunc(text[:start], unicode.IsSpace); strings.HasSuffix(before, glmCallOpen) {
		start = len(before) - len(glmCallOpen)
	}
	after := strings.TrimLeftFunc(text[end:], unicode.IsSpace)
	if strings.HasPrefix(after, glmCallClose) {
		end = len(text) - len(after) + len(glmCallClose)
	}
	return span{start, end}
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

// truncatedToolCallSuffixes are the closing tags that end a tool call written
// as markup (pseudo-XML, and the wrapper GLM shares).
var truncatedToolCallSuffixes = []string{"</tool_call>", "</function>", "</parameter>"}

// glmClosingSuffixes close a GLM argument. Unlike the tags above they also end
// a sentence about the format ("the closing tag is </arg_value>"), so they only
// count next to evidence of a GLM call.
var glmClosingSuffixes = []string{glmArgValueClose, glmArgKeyClose}

// truncatedToolCallPrefixes are the tags a reply can only START with when it is
// the rest of a GLM call whose head the gateway consumed (prod: a reply that
// was just "<arg_value>pocketbase/migrations/0001"). A bare <arg_value> opens
// no sentence; "<tool_call> is the tag GLM uses" does, so the opening tags of
// whole calls are left out.
var truncatedToolCallPrefixes = []string{glmArgKeyOpen, glmArgValueOpen}

// markupTags are the tags a reply cut mid-tag can end with a piece of.
var markupTags = []string{
	glmCallOpen, glmCallClose, glmArgKeyOpen, glmArgKeyClose, glmArgValueOpen, glmArgValueClose,
	"</function>", "</parameter>",
}

var (
	// glmPairRe is a key followed by its value: GLM call markup, not a
	// mention of one tag.
	glmPairRe = regexp.MustCompile(`</arg_key>\s*<arg_value>`)
	// glmCallHeadRe is a GLM call opener with its tool name, written the way
	// the model writes it: no space after the tag.
	glmCallHeadRe = regexp.MustCompile(`<tool_call>[A-Za-z0-9_.\-]+`)
	glmKeyPieceRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]*$`)
)

// LooksLikeTruncatedToolCall reports whether text is a piece of a tool call
// written as markup that could not be turned into a call: the TAIL of one whose
// opening tags never reached us (a gateway parser matched `<tool_call>…`, bailed
// and forwarded the remainder), a reply that IS the rest of such a call (it
// opens with a bare `<arg_value>`), a whole call that was not lifted (ambiguous
// markup, or a tool the request did not offer), or a GLM call cut off anywhere:
// inside a value, inside a key, right after the tool name, or mid-tag.
//
// Callers must treat such text as a FAILED tool call, never as an answer: with
// the tool name gone the call cannot be reconstructed, and there is no way to
// tell where the model's prose ended and the call began — so any prefix kept
// would be a guess.
//
// The test is deliberately anchored at the START or END of the message.
// Requiring only that a tag appear somewhere would fire on an assistant
// legitimately explaining this markup, which happens the moment anyone debugs
// it.
func LooksLikeTruncatedToolCall(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	for _, suffix := range truncatedToolCallSuffixes {
		if strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}
	for _, prefix := range truncatedToolCallPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	glmCall := glmPairRe.MatchString(trimmed) || glmCallHeadRe.MatchString(trimmed)
	for _, suffix := range glmClosingSuffixes {
		if glmCall && strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}
	return endsInsideGLMArgValue(trimmed) || endsInsideGLMArgKey(trimmed) ||
		endsAfterGLMCallHead(trimmed) || endsMidTag(trimmed)
}

// endsInsideGLMArgValue reports whether text stops in the middle of a GLM
// argument value: its last <arg_value> follows an </arg_key> and is never
// closed. That is a call cut off mid-emission ("…</arg_key>\n<arg_value>src/App"),
// not an explanation of the format, which would show both tags.
func endsInsideGLMArgValue(text string) bool {
	open := strings.LastIndex(text, glmArgValueOpen)
	if open < 0 || strings.LastIndex(text, glmArgValueClose) > open {
		return false
	}
	return strings.HasSuffix(strings.TrimSpace(text[:open]), glmArgKeyClose)
}

// endsInsideGLMArgKey reports whether text stops in the middle of a GLM key
// ("…<arg_key>cont"): the last <arg_key> is never closed, what follows it could
// still be a key, and it sits where a key goes — first in the reply, after a
// value, or after the tool name.
func endsInsideGLMArgKey(text string) bool {
	open := strings.LastIndex(text, glmArgKeyOpen)
	if open < 0 || !glmKeyPieceRe.MatchString(text[open+len(glmArgKeyOpen):]) {
		return false
	}
	before := strings.TrimSpace(text[:open])
	return before == "" || strings.HasSuffix(before, glmArgValueClose) || endsAfterGLMCallHead(before)
}

// endsAfterGLMCallHead reports whether text stops right after a GLM call's
// tool name ("Vou criar o arquivo.\n<tool_call>write_file").
func endsAfterGLMCallHead(text string) bool {
	open := strings.LastIndex(text, glmCallOpen)
	if open < 0 {
		return false
	}
	head := text[open:]
	loc := glmCallHeadRe.FindStringIndex(head)
	return loc != nil && loc[0] == 0 && strings.TrimSpace(head[loc[1]:]) == ""
}

// endsMidTag reports whether text stops inside one of the call tags
// ("…</arg_value>\n</tool_c"). Three characters past the "<" at least, so a
// reply ending in a lone "<" or "</" is left alone.
func endsMidTag(text string) bool {
	open := strings.LastIndex(text, "<")
	if open < 0 || len(text)-open < 4 {
		return false
	}
	piece := text[open:]
	for _, tag := range markupTags {
		if len(piece) < len(tag) && strings.HasPrefix(tag, piece) {
			return true
		}
	}
	return false
}

// toolCallMarkupMarkers are the opening tokens of a tool call written as text.
// Only OPENING ones: they are what a stream can recognize before the block is
// complete, which is the whole point of catching this mid-stream.
var toolCallMarkupMarkers = []string{"<tool_call>", "<function=", "<parameter=", glmArgKeyOpen, glmArgValueOpen}

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
