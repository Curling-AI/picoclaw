package protocoltypes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
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
//     liftGLMToolCalls and by the truncated-call guards in text_extract.go.
//  2. The gateway does return a structured call, but the model left a closing
//     tag out and the rest of the markup rode along inside one argument:
//     {"action": "replace_text<arg_key>new_text</arg_key><arg_value>…"}.
//     The tool then rejects the call ("is not in enum") and the model repeats
//     the same broken call over and over. Handled by RepairToolCallMarkup.
//
// Both only act on markup that cannot be anything else. An argument is
// arbitrary text (a file, a command, a patch) and may quote this very markup;
// read as a call, a quote rebinds arguments or runs a command the model only
// showed. Whatever is ambiguous stays as it came: a text call is not lifted
// (the guard then asks the model to retry) and a structured call is not
// rewritten.
//
// Values stay strings. The executor converts a string to the type the tool's
// schema declares (numbers, booleans, and JSON text for arrays and objects);
// guessing here turned "1.10" into 1.1 and long ids into floats.
const (
	glmArgKeyOpen    = "<arg_key>"
	glmArgKeyClose   = "</arg_key>"
	glmArgValueOpen  = "<arg_value>"
	glmArgValueClose = "</arg_value>"
)

const (
	glmCallOpen  = "<tool_call>"
	glmCallClose = "</tool_call>"
)

var (
	glmCallNameRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+)`)
	toolNameRe    = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	// Process-unique ids: some providers refuse a replayed history with two
	// tool calls sharing an id, and a per-process counter alone repeats after a
	// restart within one conversation. The whole start time: its low 32 bits
	// alone repeat every 4.3 s. Kept short: some providers cap the id length.
	glmCallIDPrefix = fmt.Sprintf("glm_call_%x_", time.Now().UnixNano())
	glmCallSeq      atomic.Uint64
)

type glmArg struct {
	key   string
	value string
}

// glmBlock is one <tool_call>NAME…</tool_call> span of the text.
type glmBlock struct {
	start, end int // byte offsets of the whole block, end exclusive
	name, body string
	open       bool // never closed: runs to the end of the text
}

// span is a byte range of a text, end exclusive.
type span struct{ start, end int }

// tagFinder finds the next occurrence of one tag at or after a position. It
// answers from its last search while the position stays between where that
// search started and what it found, so a walk that only moves forward scans
// the text once per tag instead of once per step.
type tagFinder struct {
	text, tag string
	from, at  int // the last search started at from and found the tag at at (-1: none)
}

func newTagFinder(text, tag string) *tagFinder {
	return &tagFinder{text: text, tag: tag, from: len(text) + 1, at: -1}
}

func (f *tagFinder) next(pos int) int {
	if pos >= f.from && (f.at < 0 || pos <= f.at) {
		return f.at
	}
	if pos > len(f.text) {
		return -1
	}
	f.from, f.at = pos, -1
	if i := strings.Index(f.text[pos:], f.tag); i >= 0 {
		f.at = pos + i
	}
	return f.at
}

// glmScanner walks the GLM blocks of one text.
type glmScanner struct {
	opens, closes, values, valueCloses *tagFinder
}

// glmBlocks finds the GLM calls in text, one block at a time. A block ends at
// the first </tool_call> outside a value: a value that is closed before any
// next <tool_call> may quote that tag. A value that never closes stays in its
// block instead of swallowing the prose and the call that follow. An opener
// not followed by an argument list (prose mentioning the tag) is skipped
// alone, so the call after it is still found. A block that never closes runs
// to the end of the text and is marked open: a truncated call, never lifted,
// whose values may still quote calls in other formats.
func glmBlocks(text string) []glmBlock {
	s := glmScanner{
		opens:       newTagFinder(text, glmCallOpen),
		closes:      newTagFinder(text, glmCallClose),
		values:      newTagFinder(text, glmArgValueOpen),
		valueCloses: newTagFinder(text, glmArgValueClose),
	}
	var blocks []glmBlock
	pos := 0
	for {
		start := s.opens.next(pos)
		if start < 0 {
			return blocks
		}
		afterOpen := start + len(glmCallOpen)
		name := glmCallNameRe.FindStringSubmatchIndex(text[afterOpen:])
		if name == nil || !glmArgsFollow(text[afterOpen+name[1]:]) {
			pos = afterOpen
			continue
		}
		bodyStart := afterOpen + name[1]
		end := s.blockEnd(bodyStart)
		if end < 0 {
			return append(blocks, glmBlock{
				start: start,
				end:   len(text),
				name:  text[afterOpen+name[2] : afterOpen+name[3]],
				body:  text[bodyStart:],
				open:  true,
			})
		}
		blocks = append(blocks, glmBlock{
			start: start,
			end:   end + len(glmCallClose),
			name:  text[afterOpen+name[2] : afterOpen+name[3]],
			body:  text[bodyStart:end],
		})
		pos = end + len(glmCallClose)
	}
}

// blockEnd returns where the </tool_call> closing the block whose body starts
// at pos sits, or -1 when the block never closes.
func (s *glmScanner) blockEnd(pos int) int {
	for {
		closing := s.closes.next(pos)
		if closing < 0 {
			return -1
		}
		value := s.values.next(pos)
		if value < 0 || value > closing {
			return closing
		}
		valueStart := value + len(glmArgValueOpen)
		valueEnd := s.valueCloses.next(valueStart)
		nextCall := s.opens.next(valueStart)
		if valueEnd < 0 || (nextCall >= 0 && nextCall < valueEnd) {
			return closing
		}
		pos = valueEnd + len(glmArgValueClose)
	}
}

// glmArgsFollow reports whether the text after a tool name is a GLM argument
// list: nothing before the closing tag, or a pair starting right away.
func glmArgsFollow(rest string) bool {
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	return strings.HasPrefix(rest, glmArgKeyOpen) || strings.HasPrefix(rest, glmCallClose)
}

// liftGLMToolCalls turns into calls the GLM blocks that end the text, with
// their spans. Only the tail counts: a model emitting a call stops there, and
// a block followed by more prose, or inside a ``` fence, is a call the model
// is quoting (a page it summarizes, an example it gives). One ambiguous block
// in the tail lifts nothing.
func liftGLMToolCalls(text string, blocks []glmBlock) ([]ToolCall, []span) {
	if blocks[len(blocks)-1].open {
		return nil, nil
	}
	first, end := len(blocks), len(text)
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.TrimSpace(text[blocks[i].end:end]) != "" {
			break
		}
		first, end = i, blocks[i].start
	}
	if first == len(blocks) || insideCodeFence(text[:blocks[first].start]) {
		return nil, nil
	}
	tail := blocks[first:]
	calls := make([]ToolCall, 0, len(tail))
	spans := make([]span, 0, len(tail))
	for _, b := range tail {
		pairs, rest, ok := parseGLMArgs(b.body)
		if !ok || strings.TrimSpace(rest) != "" {
			return nil, nil
		}
		args := make(map[string]any, len(pairs))
		for _, a := range pairs {
			args[a.key] = a.value
		}
		argsJSON, _ := json.Marshal(args)
		calls = append(calls, ToolCall{
			ID:        glmCallIDPrefix + strconv.FormatUint(glmCallSeq.Add(1), 10),
			Name:      b.name,
			Arguments: args,
			Function: &FunctionCall{
				Name:      b.name,
				Arguments: string(argsJSON),
			},
		})
		spans = append(spans, span{b.start, b.end})
	}
	return calls, spans
}

// insideCodeFence reports whether text leaves a ``` fence open.
func insideCodeFence(text string) bool {
	return strings.Count(text, "```")%2 == 1
}

// parseGLMArgs reads a run of <arg_key>K</arg_key><arg_value>V</arg_value>
// pairs. A value ends at its </arg_value> or, when the model forgot that tag,
// at the next <arg_key> or the end of the text. rest is what follows the last
// closed value when it is not another pair; the caller decides what may sit
// there.
//
// ok is false when the markup is ambiguous: a key repeats, is not a plain
// identifier or never closes, a key has no value, a value opens another call,
// or text sits between a closed value and the next key. That is what a value
// quoting this markup looks like, and reading it as pairs would hand the tool
// arguments nobody passed.
//
// Linear in the input: every search is bounded by the next <arg_key>.
func parseGLMArgs(s string) (args []glmArg, rest string, ok bool) {
	seen := map[string]bool{}
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(s, glmArgKeyOpen) {
			return args, s, true
		}
		s = s[len(glmArgKeyOpen):]
		pair, next := s, ""
		if i := strings.Index(s, glmArgKeyOpen); i >= 0 {
			pair, next = s[:i], s[i:]
		}

		keyEnd := strings.Index(pair, glmArgKeyClose)
		if keyEnd < 0 {
			return nil, "", false
		}
		key := strings.TrimSpace(pair[:keyEnd])
		if !toolNameRe.MatchString(key) || seen[key] {
			return nil, "", false
		}
		seen[key] = true
		value := strings.TrimLeftFunc(pair[keyEnd+len(glmArgKeyClose):], unicode.IsSpace)
		if !strings.HasPrefix(value, glmArgValueOpen) {
			return nil, "", false
		}
		value = value[len(glmArgValueOpen):]
		after := ""
		if closing := strings.Index(value, glmArgValueClose); closing >= 0 {
			value, after = value[:closing], value[closing+len(glmArgValueClose):]
			if next != "" && strings.TrimSpace(after) != "" {
				return nil, "", false
			}
		}
		if strings.Contains(value, glmCallOpen) {
			return nil, "", false
		}
		args = append(args, glmArg{key: key, value: glmValue(value)})
		if next == "" {
			return args, after, true
		}
		s = next
	}
}

// glmValue drops the one line break the format puts on each side of a value
// and nothing else: the value may be a patch whose first line is indented.
func glmValue(raw string) string {
	for _, nl := range []string{"\r\n", "\n"} {
		if strings.HasPrefix(raw, nl) {
			raw = raw[len(nl):]
			break
		}
	}
	for _, nl := range []string{"\r\n", "\n"} {
		if strings.HasSuffix(raw, nl) {
			raw = raw[:len(raw)-len(nl)]
			break
		}
	}
	return raw
}

// RepairToolCallMarkup rewrites, in place, structured tool calls that carry the
// rest of a GLM call inside their name or one string argument, and returns the
// names of the calls it changed.
//
// It only acts on the shape the model produces when it drops a closing tag:
// the markup starts inside the argument and runs to its end (at most a stray
// </tool_call> after the last pair), every recovered argument is a parameter
// the tool declares (tools is what the request offered), none repeats, and
// none was already in the call. Anything else stays as the model sent it: a
// write_file whose content quotes this markup, or an exec that greps for it,
// is a normal call, even when the key it quotes is one the tool takes.
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
	if head, rest, found := strings.Cut(name, glmArgKeyOpen); found {
		pairs, ok := trailingGLMArgs(rest)
		if !ok {
			return false
		}
		name = strings.TrimSpace(head)
		recovered = append(recovered, pairs...)
	}
	props, known := params[name]
	if !known {
		return false
	}

	for key, value := range args {
		str, isString := value.(string)
		// "raw" holds arguments the provider could not decode; the executor
		// explains that case to the model.
		if !isString || key == "raw" || !strings.Contains(str, glmArgKeyOpen) {
			continue
		}
		head, rest, _ := strings.Cut(str, glmArgKeyOpen)
		pairs, ok := trailingGLMArgs(rest)
		if !ok {
			return false
		}
		args[key] = strings.TrimSuffix(strings.TrimRight(head, " \t\r\n"), glmArgValueClose)
		recovered = append(recovered, pairs...)
	}

	if len(recovered) == 0 {
		return false
	}
	seen := make(map[string]bool, len(recovered))
	for _, a := range recovered {
		if _, declared := props[a.key]; !declared || seen[a.key] {
			return false
		}
		if _, given := tc.Arguments[a.key]; given {
			return false
		}
		seen[a.key] = true
	}
	for _, a := range recovered {
		args[a.key] = a.value
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

// trailingGLMArgs reads the pairs of markup that starts right after an
// <arg_key> and must run to the end of its text; a stray </tool_call> at the
// very end, where the model closed the call it thought it was writing, goes
// away with the last value.
func trailingGLMArgs(rest string) ([]glmArg, bool) {
	text := strings.TrimSuffix(strings.TrimRightFunc(rest, unicode.IsSpace), glmCallClose)
	pairs, tail, ok := parseGLMArgs(glmArgKeyOpen + text)
	if !ok || len(pairs) == 0 || strings.TrimSpace(tail) != "" {
		return nil, false
	}
	return pairs, true
}
