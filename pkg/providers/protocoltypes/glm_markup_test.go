package protocoltypes

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Shapes below are the ones seen in prod in 2026-10 (maestro-ultra and
// maestro-flash, several providers); the values are synthetic.

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
	want := map[string]any{"projectId": float64(65025), "path": "src/App.tsx"}
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
// as the call itself.
func TestExtractGLMToolCall_WinsOverBareJSONInsideValue(t *testing.T) {
	text := "<tool_call>write_file<arg_key>path</arg_key><arg_value>a.json</arg_value>" +
		"<arg_key>content</arg_key><arg_value>{\"name\":\"x\",\"arguments\":{}}</arg_value></tool_call>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("calls = %#v", calls)
	}
	if _, ok := calls[0].Arguments["content"].(map[string]any); !ok {
		t.Errorf("content = %#v, want the decoded object", calls[0].Arguments["content"])
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
			name: "argument the model wrote itself wins",
			call: ToolCall{Name: "write_file", Arguments: map[string]any{
				"path":    "a.txt",
				"content": "x<arg_key>path</arg_key><arg_value>b.txt",
			}},
			wantName: "write_file",
			want:     map[string]any{"path": "a.txt", "content": "x"},
		},
		{
			name: "numeric value keeps its type",
			call: ToolCall{Name: "mcp_skip_skip_file_read", Arguments: map[string]any{
				"projectId": "65025<arg_key>path</arg_key><arg_value>src/App.tsx</arg_value>",
			}},
			wantName: "mcp_skip_skip_file_read",
			want:     map[string]any{"projectId": float64(65025), "path": "src/App.tsx"},
		},
		{
			name: "markup inside the argument name",
			call: ToolCall{Name: "exec", Arguments: map[string]any{
				"cena 1 do reel && echo ok</arg_value><arg_key>command": "ls -la",
			}},
			wantName: "exec",
			want:     map[string]any{"command": "ls -la"},
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
			if n := RepairToolCallMarkup(calls); n != 1 {
				t.Fatalf("repaired = %d, want 1", n)
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

func TestRepairToolCallMarkup_LeavesCleanCallsAlone(t *testing.T) {
	calls := []ToolCall{{
		Name:      "write_file",
		Arguments: map[string]any{"path": "notes.md", "content": "Use <b>negrito</b> e `<arg_value>` no texto."},
	}}
	if n := RepairToolCallMarkup(calls); n != 0 {
		t.Fatalf("repaired = %d, want 0", n)
	}
	if calls[0].Function != nil {
		t.Error("an untouched call must not grow a Function block")
	}
}
