package protocoltypes

import (
	"encoding/json"
	"fmt"
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
	toolDef("write_file", "path", "content", "append"),
	toolDef("exec", "command", "cwd", "background"),
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
func TestGLMBlocks_UnclosedValueStaysInItsBlock(t *testing.T) {
	first := "<tool_call>exec<arg_key>command</arg_key><arg_value>ls</tool_call>"
	second := "<tool_call>read_file<arg_key>path</arg_key><arg_value>a.txt</arg_value></tool_call>"
	text := first + "\nSome prose.\n" + second
	blocks := glmBlocks(text)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want two", blocks)
	}
	if got := text[blocks[0].start:blocks[0].end]; got != first {
		t.Errorf("first block = %q", got)
	}
	if got := text[blocks[1].start:blocks[1].end]; got != second {
		t.Errorf("second block = %q", got)
	}
}

// Several calls ending the message are all lifted, each with its own id.
func TestExtractGLMToolCall_SeveralCallsAtTheEnd(t *testing.T) {
	text := "Vou olhar os dois.\n" +
		"<tool_call>exec<arg_key>command</arg_key><arg_value>ls</tool_call>\n" +
		"<tool_call>mcp_skip_skip_file_read<arg_key>path</arg_key><arg_value>a.txt</arg_value></tool_call>\n"
	calls, rest := LiftToolCallsFromText(text)
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want two", calls)
	}
	if calls[0].Arguments["command"] != "ls" || calls[1].Arguments["path"] != "a.txt" {
		t.Errorf("args = %#v / %#v", calls[0].Arguments, calls[1].Arguments)
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("both calls got id %q", calls[0].ID)
	}
	if rest != "Vou olhar os dois." {
		t.Errorf("rest = %q, want the prose kept", rest)
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
		{
			"ends with a closed value of a call",
			"Vou ler.\n<tool_call>read_file<arg_key>path</arg_key><arg_value>a.txt</arg_value>",
			true,
		},
		{"ends with a key", "<arg_key>path</arg_key>", true},
		{
			"prose then a call cut inside a value",
			"Vou ler.\n<tool_call>read_file\n<arg_key>path</arg_key>\n<arg_value>src/Ap",
			true,
		},
		{"cut inside the first key", "Vou ler.\n<tool_call>read_file\n<arg_key>pa", true},
		{
			"cut inside a later key",
			"<tool_call>exec<arg_key>command</arg_key><arg_value>ls</arg_value>\n<arg_key>cw",
			true,
		},
		{"reply that is a key cut short", "<arg_key>cont", true},
		{"cut right after the tool name", "Vou criar o arquivo.\n<tool_call>write_file", true},
		{"cut inside the closing tag", "<arg_value>a.txt</arg_value>\n</tool_c", true},
		{"cut inside a value tag", "Vou ler.\n<tool_call>read_file\n<arg_key>path</arg_key>\n<arg_va", true},
		{
			"a whole call that was not lifted",
			"<tool_call>write_file<arg_key>path</arg_key><arg_value>a</arg_value><arg_key>path</arg_key><arg_value>b</arg_value></tool_call>",
			true,
		},
		{
			"explaining the format",
			"O GLM escreve <arg_key>k</arg_key><arg_value>v</arg_value> no lugar do JSON.",
			false,
		},
		{"mentioning one tag", "Use a tag `<arg_value>` para o valor", false},
		{"mentioning the key tag", "Use a tag `<arg_key>` para a chave", false},
		{"explaining the closing tag", "The closing tag is </arg_value>", false},
		{"explaining the key closing tag", "Cada chave termina em </arg_key>", false},
		{"mentioning the call tag before a word", "I will use the <tool_call> format", false},
		{"ending with a lone angle bracket", "Use a < b", false},
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
		{
			name: "the call closed after an unclosed value",
			call: ToolCall{Name: "memory", Arguments: map[string]any{
				"action": "replace_text<arg_key>new_text</arg_key><arg_value>texto novo\n</tool_call>\n",
			}},
			wantName: "memory",
			want:     map[string]any{"action": "replace_text", "new_text": "texto novo"},
		},
		{
			name: "the call closed after a closed value",
			call: ToolCall{Name: "memory", Arguments: map[string]any{
				"action": "replace_text<arg_key>new_text</arg_key><arg_value>texto</arg_value></tool_call>",
			}},
			wantName: "memory",
			want:     map[string]any{"action": "replace_text", "new_text": "texto"},
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
// tool does not take, a broken-key shape that would lose part of the call. The
// quoted keys are ones the tool takes and the call left out, so only the shape
// of the markup keeps these calls whole.
func TestRepairToolCallMarkup_LeavesNormalCallsAlone(t *testing.T) {
	cases := map[string]ToolCall{
		"file content quoting the format": {Name: "write_file", Arguments: map[string]any{
			"path": "docs/glm.md",
			"content": "Exemplo: `<tool_call>write_file<arg_key>append</arg_key><arg_value>true</arg_value>" +
				"</tool_call>` grava no fim do arquivo.",
		}},
		"fixture written by a command": {Name: "exec", Arguments: map[string]any{
			"command": "printf '<arg_key>cwd</arg_key><arg_value>/etc</arg_value>' > fixture.txt && rm -rf build",
		}},
		"grep for a pair": {Name: "exec", Arguments: map[string]any{
			"command": "grep -r '<arg_key>background</arg_key><arg_value>true</arg_value>' docs/ && rm tmp",
		}},
		"grep for the tag": {Name: "exec", Arguments: map[string]any{
			"command": "grep -rn '<arg_key>' pkg/ | head",
		}},
		"recovered key repeated": {Name: "exec", Arguments: map[string]any{
			"command": "ls<arg_key>cwd</arg_key><arg_value>/tmp</arg_value><arg_key>cwd</arg_key><arg_value>/etc",
		}},
		"recovered key repeated across arguments": {Name: "memory", Arguments: map[string]any{
			"action":  "add_section<arg_key>section</arg_key><arg_value>Notas",
			"content": "texto<arg_key>section</arg_key><arg_value>Outra",
		}},
		"text between pairs": {Name: "exec", Arguments: map[string]any{
			"command": "ls<arg_key>cwd</arg_key><arg_value>/tmp</arg_value> e depois <arg_key>background</arg_key><arg_value>true",
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

// No input shape may make the parser quadratic: the text is model output,
// up to the output cap and beyond when a gateway concatenates.
func TestGLMParsing_StaysLinear(t *testing.T) {
	var unclosed strings.Builder
	for i := range 40_000 {
		fmt.Fprintf(&unclosed, "<arg_key>k%d</arg_key><arg_value>value without its closing tag ", i)
	}
	var keysOnly strings.Builder
	for i := range 40_000 {
		fmt.Fprintf(&keysOnly, "<arg_key>k%d</arg_key>", i)
	}
	var quotedClosers strings.Builder
	quotedClosers.WriteString("<tool_call>x")
	for i := range 40_000 {
		fmt.Fprintf(&quotedClosers, "<arg_key>k%d</arg_key><arg_value>a</tool_call></arg_value>", i)
	}
	quotedClosers.WriteString("</tool_call>")

	cases := map[string]func(){
		"many unclosed pairs": func() {
			if pairs, _, ok := parseGLMArgs(unclosed.String()); !ok || len(pairs) != 40_000 {
				t.Errorf("pairs = %d, ok = %v", len(pairs), ok)
			}
		},
		"keys with no value": func() {
			if _, _, ok := parseGLMArgs(keysOnly.String()); ok {
				t.Error("keys with no value were accepted")
			}
		},
		"values quoting the closing tag": func() {
			if blocks := glmBlocks(quotedClosers.String()); len(blocks) != 1 {
				t.Errorf("blocks = %d, want the whole call", len(blocks))
			}
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			run()
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("took %s", elapsed)
			}
		})
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
