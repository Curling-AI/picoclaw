package protocoltypes

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Shapes below are the ones seen in prod in 2026-10 (maestro-ultra and
// maestro-flash, several providers); the values are synthetic.

func toolDef(name string, props ...string) ToolDefinition {
	properties := map[string]any{}
	for _, p := range props {
		properties[p] = map[string]any{"type": "string"}
	}
	return ToolDefinition{Type: "function", Function: ToolFunctionDefinition{
		Name:       name,
		Parameters: map[string]any{"type": "object", "properties": properties},
	}}
}

var offeredTools = []ToolDefinition{
	toolDef("memory", "action", "section", "content", "new_text", "old_text"),
	toolDef("write_file", "path", "content"),
	toolDef("exec", "command", "cwd"),
	toolDef("mcp_skip_skip_file_read", "projectId", "path"),
}

func TestExtractGLMToolCall(t *testing.T) {
	text := "Vou listar os arquivos.\n<tool_call>mcp_skip_skip_file_read\n" +
		"<arg_key>projectId</arg_key>\n<arg_value>65025</arg_value>\n" +
		"<arg_key>path</arg_key>\n<arg_value>src/App.tsx</arg_value>\n</tool_call>"

	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "mcp_skip_skip_file_read" {
		t.Errorf("name = %q", calls[0].Name)
	}
	// Values stay strings; the executor converts them by the tool's schema.
	want := map[string]any{"projectId": "65025", "path": "src/App.tsx"}
	if !reflect.DeepEqual(calls[0].Arguments, want) {
		t.Errorf("args = %#v, want %#v", calls[0].Arguments, want)
	}
	if got := StripToolCallsFromText(text); got != "Vou listar os arquivos." {
		t.Errorf("stripped = %q", got)
	}
}

func TestExtractGLMToolCall_NoArguments(t *testing.T) {
	calls := ExtractToolCallsFromText("<tool_call>mcp_skip_skip_org_list</tool_call>")
	if len(calls) != 1 || calls[0].Name != "mcp_skip_skip_org_list" || len(calls[0].Arguments) != 0 {
		t.Fatalf("calls = %#v", calls)
	}
}

// A value holding code that looks like a bare JSON call must not be lifted out
// as the call itself, and keeps its exact text.
func TestExtractGLMToolCall_WinsOverBareJSONInsideValue(t *testing.T) {
	text := "<tool_call>write_file<arg_key>path</arg_key><arg_value>a.json</arg_value>" +
		"<arg_key>content</arg_key><arg_value>{\"name\":\"x\",\"arguments\":{}}</arg_value></tool_call>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("calls = %#v", calls)
	}
	if got := calls[0].Arguments["content"]; got != `{"name":"x","arguments":{}}` {
		t.Errorf("content = %#v, want the text as written", got)
	}
}

// One value missing its closing tag must not run into the next call and eat
// the prose between them.
func TestExtractGLMToolCall_UnclosedValueStaysInItsBlock(t *testing.T) {
	text := "<tool_call>exec<arg_key>command</arg_key><arg_value>ls</tool_call>\n" +
		"Some prose.\n<tool_call>read_file<arg_key>path</arg_key><arg_value>a.txt</arg_value></tool_call>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want two", calls)
	}
	if calls[0].Arguments["command"] != "ls" || calls[1].Arguments["path"] != "a.txt" {
		t.Errorf("args = %#v / %#v", calls[0].Arguments, calls[1].Arguments)
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("both calls got id %q", calls[0].ID)
	}
	if got := StripToolCallsFromText(text); got != "Some prose." {
		t.Errorf("stripped = %q, want the prose kept", got)
	}
}

func TestLooksLikeTruncatedToolCall_GLM(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"reply that is a value cut short", "<arg_value>pocketbase/migrations/0001", true},
		{"tail of a call", "<arg_value>65025</arg_value>\n</tool_call>", true},
		{"ends with a closed value", "src/App.tsx</arg_value>", true},
		{"ends with a key", "<arg_key>path</arg_key>", true},
		{
			"prose then a call cut inside a value",
			"Vou ler.\n<tool_call>read_file\n<arg_key>path</arg_key>\n<arg_value>src/Ap",
			true,
		},
		{
			"explaining the format",
			"O GLM escreve <arg_key>k</arg_key><arg_value>v</arg_value> no lugar do JSON.",
			false,
		},
		{"mentioning one tag", "Use a tag `<arg_value>` para o valor", false},
		{"opening with the call tag in prose", "<tool_call> is the tag GLM uses; the answer is no.", false},
		{"opening with the pseudo-XML tag in prose", "<function=foo> is how Qwen writes it.", false},
		{"plain answer", "Pronto, o arquivo foi criado.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeTruncatedToolCall(tc.text); got != tc.want {
				t.Errorf("LooksLikeTruncatedToolCall(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestLooksLikeToolCallMarkup_GLM(t *testing.T) {
	if !LooksLikeToolCallMarkup("Vou criar o arquivo.\n<tool_call>write_file\n<arg_key>pa") {
		t.Error("a GLM call starting mid-stream must stop live publishing")
	}
	if !LooksLikeToolCallMarkup("<arg_value>65025") {
		t.Error("a bare GLM value must stop live publishing")
	}
}

func TestRepairToolCallMarkup(t *testing.T) {
	cases := []struct {
		name     string
		call     ToolCall
		wantName string
		want     map[string]any
	}{
		{
			name: "rest of the call inside the first value",
			call: ToolCall{Name: "memory", Arguments: map[string]any{
				"action": "replace_text<arg_key>new_text</arg_key><arg_value>- linha 1\n- linha 2",
			}},
			wantName: "memory",
			want:     map[string]any{"action": "replace_text", "new_text": "- linha 1\n- linha 2"},
		},
		{
			name: "several pairs, one closed and one not",
			call: ToolCall{Name: "memory", Arguments: map[string]any{
				"action": "add_section<arg_key>section</arg_key><arg_value>Notas</arg_value>" +
					"<arg_key>content</arg_key><arg_value>texto",
			}},
			wantName: "memory",
			want:     map[string]any{"action": "add_section", "section": "Notas", "content": "texto"},
		},
		{
			name: "values keep their text",
			call: ToolCall{Name: "mcp_skip_skip_file_read", Arguments: map[string]any{
				"projectId": "65025<arg_key>path</arg_key><arg_value>src/v1.10.tsx</arg_value>",
			}},
			wantName: "mcp_skip_skip_file_read",
			want:     map[string]any{"projectId": "65025", "path": "src/v1.10.tsx"},
		},
		{
			name:     "markup inside the tool name",
			call:     ToolCall{Name: "exec<arg_key>command</arg_key><arg_value>ls -la", Arguments: map[string]any{}},
			wantName: "exec",
			want:     map[string]any{"command": "ls -la"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := []ToolCall{tc.call}
			if got := RepairToolCallMarkup(calls, offeredTools); !reflect.DeepEqual(got, []string{tc.wantName}) {
				t.Fatalf("repaired = %v, want [%s]", got, tc.wantName)
			}
			got := calls[0]
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
			if !reflect.DeepEqual(got.Arguments, tc.want) {
				t.Errorf("args = %#v, want %#v", got.Arguments, tc.want)
			}
			if got.Function == nil || got.Function.Name != tc.wantName {
				t.Fatalf("function = %#v, want name %q", got.Function, tc.wantName)
			}
			var fromJSON map[string]any
			if err := json.Unmarshal([]byte(got.Function.Arguments), &fromJSON); err != nil {
				t.Fatalf("function arguments are not JSON: %v", err)
			}
			if !reflect.DeepEqual(fromJSON, tc.want) {
				t.Errorf("function arguments = %#v, want %#v", fromJSON, tc.want)
			}
		})
	}
}

// Calls that merely carry this markup as content are normal calls: writing a
// test fixture, grepping for the tag, a key the model already sent, a key the
// tool does not take, a broken-key shape that would lose part of the call.
func TestRepairToolCallMarkup_LeavesNormalCallsAlone(t *testing.T) {
	cases := map[string]ToolCall{
		"file content quoting the format": {Name: "write_file", Arguments: map[string]any{
			"path":    "glm_test.go",
			"content": "const sample = `<tool_call>exec<arg_key>command</arg_key><arg_value>ls</arg_value></tool_call>`",
		}},
		"grep for the tag": {Name: "exec", Arguments: map[string]any{
			"command": "grep -rn '<arg_key>' pkg/ | head",
		}},
		"recovered key already given": {Name: "write_file", Arguments: map[string]any{
			"path":    "a.txt",
			"content": "x<arg_key>path</arg_key><arg_value>b.txt",
		}},
		"recovered key the tool does not take": {Name: "memory", Arguments: map[string]any{
			"action": "add_section<arg_key>priority</arg_key><arg_value>high",
		}},
		"markup inside an argument name": {Name: "exec", Arguments: map[string]any{
			"cena 1 && echo ok</arg_value><arg_key>command": "ls -la",
		}},
		"undecodable arguments": {Name: "write_file", Arguments: map[string]any{
			"raw": `{"path":"a<arg_key>content</arg_key><arg_value>b`,
		}},
		"tag without a key": {Name: "write_file", Arguments: map[string]any{
			"path": "notes.md", "content": "Use <b>negrito</b> e `<arg_value>` no texto.",
		}},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := json.Marshal(call.Arguments)
			calls := []ToolCall{call}
			if got := RepairToolCallMarkup(calls, offeredTools); len(got) != 0 {
				t.Fatalf("repaired %v, want nothing", got)
			}
			after, _ := json.Marshal(calls[0].Arguments)
			if string(before) != string(after) || calls[0].Name != call.Name || calls[0].Function != nil {
				t.Errorf("call changed: %s %s -> %s %s", call.Name, before, calls[0].Name, after)
			}
		})
	}
}

// Without the offered tools there is nothing to check recovered keys against.
func TestRepairToolCallMarkup_NeedsTheOfferedTools(t *testing.T) {
	calls := []ToolCall{{Name: "memory", Arguments: map[string]any{
		"action": "replace_text<arg_key>new_text</arg_key><arg_value>x",
	}}}
	if got := RepairToolCallMarkup(calls, nil); len(got) != 0 {
		t.Fatalf("repaired %v without tools", got)
	}
}

// Many unclosed pairs must not make the parser quadratic.
func TestParseGLMArgs_StaysLinear(t *testing.T) {
	var b strings.Builder
	for range 40_000 {
		b.WriteString("<arg_key>k</arg_key><arg_value>value without its closing tag ")
	}
	start := time.Now()
	if got := len(parseGLMArgs(b.String())); got != 40_000 {
		t.Fatalf("pairs = %d", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("parsing took %s", elapsed)
	}
}

// A value that quotes the closing tag (plausibly why the gateway's own parser
// gave up) keeps its whole text, and nothing leaks into the content.
func TestExtractGLMToolCall_ValueQuotingTheClosingTag(t *testing.T) {
	text := "Anotado.\n<tool_call>write_file<arg_key>path</arg_key><arg_value>notes.md</arg_value>" +
		"<arg_key>content</arg_key><arg_value>GLM ends calls with </tool_call> like this</arg_value></tool_call>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one", calls)
	}
	if got := calls[0].Arguments["content"]; got != "GLM ends calls with </tool_call> like this" {
		t.Errorf("content = %#v, want the whole value", got)
	}
	if got := StripToolCallsFromText(text); got != "Anotado." {
		t.Errorf("stripped = %q, want only the prose", got)
	}
}
